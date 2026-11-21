package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/wsevent/conversationframe"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// The task verb routes: claim, delegate and requeue. Each earns its verb by
// carrying an effect a field write can't express — an external Jira or Linear
// write, a spawned run, a stopped one. The lifecycle axis (dismissed / done /
// snoozed) is a field write and lives on PATCH /api/tasks/{id} in tasks.go;
// requeue lives there too, next to its finalizer.
//
// Claim and delegate both move a column, and by the same mechanism: assigning
// a task is what lands it in_progress, so the claim doors write the stage in
// the same UPDATE. Nothing the run does afterwards moves it again.

// taskClaimRequest is the body of POST /api/tasks/{id}/claim. An absent
// target_user_id is a self-claim; a present one is a handoff to that user.
// Both are the same intent — "this task's user claim moves" — with the same
// response, which is why they're one route rather than a claim/reassign pair.
type taskClaimRequest struct {
	// TargetUserID is the handoff target: the user the claim moves to.
	// Absent means the caller claims it themselves. A pointer so a present
	// but blank value is a rejectable fault rather than a second spelling of
	// "absent" — an empty id names nobody, and quietly reading it as a
	// self-claim would hand the task to whoever asked instead of erroring on
	// a client that lost track of its target.
	TargetUserID *string `json:"target_user_id,omitempty" doc:"The id of the person to hand the task to, which moves the claim another person holds to them; absent, the caller claims the task. It cannot be empty."`
	// TeamID is the team the caller's own claim lands the task on, one of
	// the caller's candidates (resolveClaimTeam). It may be absent when the
	// caller has one candidate. Refused beside target_user_id: a handoff
	// lands on a team derived from its target.
	TeamID *string `json:"team_id,omitempty" doc:"The id of the team the caller's own claim puts the task on, one of the task's claim_team_ids; absent when there is only one. It cannot be sent with target_user_id."`
	// Version is the task version the caller read (taskJSON.Version), on both
	// arms.
	Version *int64 `json:"version" doc:"The version the caller's read of the task returned; the claim is refused when the task has changed since. It is required."`
}

var opTaskClaim = httpx.NewJSONOp[taskClaimRequest, taskJSON](httpx.OpSpec{
	Pattern:     "POST /api/tasks/{id}/claim",
	ID:          "claim_task",
	Summary:     "Claim a task or hand it to another person",
	Description: "Without target_user_id, the caller claims the task: it moves to in_progress under the caller, any run the agent had on it is stopped, and a Jira or Linear issue behind it is assigned to the caller and moved to in-progress. With target_user_id, the claim another person holds moves to that person, and nothing else changes. Returns the task with the claim applied.",
	PathParams:  map[string]string{"id": "The task's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// handleTaskClaim moves a task's user claim. Self-claim (no target) takes
// ownership for the caller, lands the task in progress, tears down whatever
// the agent had in flight, and syncs a Jira ticket or Linear issue to the
// caller. Handoff (a target) moves an existing user claim to someone else and
// deliberately touches neither — nor the stage, since the row the handoff
// starts from is already in progress.
//
// POST /api/tasks/{id}/claim
func (s *Server) handleTaskClaim(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := taskIDOr404(w, r)
	if !ok {
		return
	}

	req, ok := opTaskClaim.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	version := taskWriteVersion(&v, req.Version)
	if req.TargetUserID != nil && strings.TrimSpace(*req.TargetUserID) == "" {
		v.Invalid("target_user_id", "target_user_id must name a user; omit it to claim the task yourself")
	}
	if req.TargetUserID != nil && req.TeamID != nil {
		v.Invalid("team_id", "team_id applies only when you claim the task yourself")
	} else {
		claimTeamField(&v, req.TeamID)
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// Viewers can't act on a team's tasks. A claim is a
	// team-scoped write, so gate it here with one clean 403 rather than
	// letting the store UPDATE fall off the tasks_update RLS policy as a
	// confusing 409/500. A task the caller can't see at all is a 404.
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

	if req.TargetUserID != nil {
		written, ok := s.reassignClaim(w, r, orgID, userID, id, *req.TargetUserID, version)
		if !ok {
			return
		}
		// Admin-pool read, deliberately: a handoff can consolidate team_id
		// to the target's team, which an overriding admin need not be in —
		// the RLS-scoped read would answer 404 for a write that landed.
		s.writeTaskResourceSystem(w, r, opTaskClaim, orgID, userID, id, written)
		return
	}

	claim, ok := s.selfClaim(w, r, orgID, userID, id, req.TeamID, version)
	if !ok {
		return
	}
	landing := claim.landing
	// Taking a task onto a human's plate takes it off the agent's: stop any
	// in-flight run. What the run left behind stays — the person claiming the
	// task inherits its draft PRs and staged reviews rather than a cleared
	// desk, and retires them one at a time through the deliverable verbs.
	//
	// Only the takeover is a boundary, and the landing is what says so. A
	// claim from unclaimed and an idempotent re-claim move the task off
	// nobody: there is no conversation the task stops being about, and
	// stamping one would record a handoff that never happened. (A prior
	// requeue has already ended whatever it ended, and the door's guard keeps
	// the reason that actually applied.)
	//
	// The boundary itself was stamped in the claim's transaction, ahead of the
	// mint, so what is left here is the agent's process and what follows a
	// stamp. The person's own conversation is passed over by the stop: it is
	// unsettled, as every conversation the stop pass enumerates is.
	cleanupCtx := context.WithoutCancel(r.Context())
	stopCause := delegate.StopCauseTaskDispositioned
	if landing == claimLandingFromAgent {
		stopCause = delegate.StopCauseTaskTakenOver
	}
	s.stopTaskConversations(cleanupCtx, orgID, userID, id, stopCause, claim.conversationID)
	s.afterConversationsEnded(cleanupCtx, orgID, claim.ended)
	if claim.jira != nil {
		s.syncJiraClaim(r, orgID, userID, id, claim.jira)
	}
	if claim.linear != nil {
		s.syncLinearClaim(r, orgID, userID, id, claim.linear)
	}
	s.writeTaskResource(w, r, opTaskClaim, orgID, userID, id, claim.version)
}

// claimLanding names which of selfClaim's three accept paths a claim took. It
// exists because the caller's cleanup differs by path and cannot re-derive it:
// after the claim lands, the task's columns read the same whether a person
// took it from the agent or picked it up from the queue, and only the first is
// a boundary on the task's conversations.
type claimLanding int

const (
	// claimLandingIdempotent — the caller already owned it; nothing moved.
	claimLandingIdempotent claimLanding = iota
	// claimLandingFromUnclaimed — picked up from the queue. Nobody was
	// working it, so nothing is handed over.
	claimLandingFromUnclaimed
	// claimLandingFromAgent — the takeover: a person took work the agent held.
	claimLandingFromAgent
)

// selfClaimed is what a self-claim that landed hands the claim route: the
// acting user's Jira and Linear clients (each nil unless the task is backed by
// that source), which accept path landed, and the version its write left.
type selfClaimed struct {
	jira    *jira.Client
	linear  *linear.Client
	landing claimLanding
	version int64
	// conversationID is the person's own conversation on the task as the
	// claim's transaction left it: the one it minted, or the one an earlier
	// claim minted. Empty when the task holds a conversation that is not the
	// person's, which a boundary write that failed can leave behind.
	conversationID string
	// ended is the agent's conversations a takeover ended, stamped in the
	// claim's transaction. Empty on every other landing.
	ended []domain.Conversation
}

// personClaimCancelReason is the reason a person's claim records on the
// task's queued firings (cancelFiringsForPersonClaim).
const personClaimCancelReason = "claimed by a person"

// errClaimRefused rolls a claim's transaction back when its claim write
// matched no row, so the cancellation requests recorded ahead of the write do
// not outlive a claim that did not land.
var errClaimRefused = errors.New("claim write matched no row")

// cancelFiringsForPersonClaim records a cancellation request on each of the
// task's queued firings, in the transaction of a person's claim. The
// conversation that claim mints keeps the task busy for as long as the person
// holds it, so a firing queued earlier would wait behind it and fire once the
// person delegated, for an event that delegation already covered.
//
// It runs ahead of the claim write: a transaction that writes pending_firings
// and tasks writes them in that order (db.PendingFiringsStore.Enqueue). A
// caller whose claim then matches no row rolls the transaction back.
func cancelFiringsForPersonClaim(ctx context.Context, tx db.TxStores, orgID, taskID, userID string) (int, error) {
	return tx.PendingFirings.RequestCancelForTask(ctx, orgID, taskID, userID, personClaimCancelReason)
}

// mintForPersonClaim mints the person's conversation on a task their claim
// just landed on, in the claim's transaction, and answers the id of the
// person's own conversation on the task: the minted one, or the interactive
// conversation the task already held. A task that holds some other
// conversation keeps it and answers "".
func mintForPersonClaim(ctx context.Context, tx db.TxStores, orgID, taskID, userID string) (string, error) {
	minted, err := tx.Conversations.MintForPersonClaim(ctx, orgID, taskID, userID)
	if err != nil {
		return "", fmt.Errorf("mint the claim's conversation: %w", err)
	}
	if minted != nil {
		return minted.ID, nil
	}
	task, err := tx.Tasks.Get(ctx, orgID, taskID)
	if err != nil || task == nil || task.ConversationID == "" {
		return "", err
	}
	held, err := tx.Conversations.Get(ctx, orgID, task.ConversationID)
	if err != nil || held == nil || held.Type != domain.ConversationTypeInteractive {
		return "", err
	}
	return held.ID, nil
}

// selfClaim is the claim route's no-target arm: a race-safe transition to the
// caller's ownership with three accept paths (idempotent same-user, takeover
// from bot, claim from unclaimed) and two refuse paths (a task past the
// version the caller read, and a different user owning it → 409). Both
// landing paths put the task in progress in the same UPDATE that writes the
// claim — the claim IS the stage marker, so the queue holds nobody's work.
// Every accept path also leaves the task with the person's conversation, in
// the claim's own transaction: a takeover ends the agent's first, and a task
// that already holds an un-ended conversation keeps it.
// For a Jira- or Linear-backed task it resolves the acting user's own client
// up front so a user with no connected Jira or Linear is refused BEFORE the
// claim lands (acting as the bot here would mis-assign the issue to the
// service account). Returns what landed (selfClaimed), and ok=false when it
// already wrote an error response.
//
// pickedTeam is the body's team_id. Every accept path resolves the team the
// claim lands on from it (resolveClaimTeam) before anything is written, the
// claim the caller already holds included, which names the team it is on.
//
// The version is compared on the read before anything else, and again by the
// write every accept path makes. The idempotent path's write changes nothing,
// but it is still a write at the version the caller read: what the claim route
// does after it (stopping runs, assigning the Jira or Linear issue) must not
// act on a task that moved after the read.
func (s *Server) selfClaim(w http.ResponseWriter, r *http.Request, orgID, userID, id string, pickedTeam *string, version int64) (selfClaimed, bool) {
	var task *domain.Task
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return selfClaimed{}, false
	}
	if task == nil {
		notFound(w, "task")
		return selfClaimed{}, false
	}
	if refuseStaleTaskRead(w, task, version) {
		return selfClaimed{}, false
	}
	// Terminal-status refusal: claim transitions on done/dismissed rows are
	// meaningless. The claim writes refuse a closed task as a lost race, and
	// the same-user arm below writes nothing at all, so this is what answers
	// that the task is closed.
	if isTerminalTaskStatus(task.Status) {
		writeTaskTerminal(w, "claim")
		return selfClaimed{}, false
	}
	// Ahead of the team: a task somebody else holds is refused whatever team
	// the body names.
	if task.ClaimedByUserID != "" && task.ClaimedByUserID != userID {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "task is already claimed by another user",
		})
		return selfClaimed{}, false
	}
	teamID, ok := s.resolveClaimTeam(w, r, orgID, userID, id, pickedTeam)
	if !ok {
		return selfClaimed{}, false
	}

	// A Jira-backed claim assigns the ticket to the claiming user and
	// transitions it — a write that must act as THAT user. Resolve the acting
	// user's credential up front, before anything is written. The
	// RequireJiraIdentity gate guarantees one is stored, but not that Jira
	// still accepts it: a Cloud OAuth grant can be revoked or expire.
	var jiraUserClient *jira.Client
	if task.EntitySource == "jira" {
		c, jerr := s.jiraResolver.ForUser(r.Context(), orgID, userID)
		if jerr != nil {
			writeJiraUserClientError(w, "tasks", jerr)
			return selfClaimed{}, false
		}
		jiraUserClient = c
	}
	// The Linear sibling, under the same rule: the issue is assigned to the
	// claiming user as themselves, never as the org's app or API-key identity.
	// This refusal is what enforces that, for every caller: the SPA's Linear
	// gate is onboarding, and a headless caller never passes through it.
	var linearUserClient *linear.Client
	if task.EntitySource == "linear" {
		c, lerr := s.linearResolver.ForUser(r.Context(), orgID, userID)
		if errors.Is(lerr, eventsource.ErrDisabled) {
			writeSourceDisabled(w, eventsource.KindLinear)
			return selfClaimed{}, false
		}
		if errors.Is(lerr, linear.ErrNoLinearUserCredential) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason:  httpx.ReasonNotConfigured,
				Message: linearUserCredentialMissingMessage(lerr),
			})
			return selfClaimed{}, false
		}
		if lerr != nil {
			internalError(w, "tasks", lerr)
			return selfClaimed{}, false
		}
		linearUserClient = c
	}

	// Each accept path is one write at the version the caller read. On the
	// idempotent path it re-asserts the stage the caller's claim already put
	// the task in, which changes nothing; the version predicate is why it is
	// made at all.
	var landing claimLanding
	var write func(tx db.TxStores) (bool, error)
	switch {
	case task.ClaimedByUserID == userID:
		landing = claimLandingIdempotent
		write = func(tx db.TxStores) (bool, error) {
			return tx.Tasks.AdvanceStatusForUser(r.Context(), orgID, id, userID, taskStatusInProgress, version)
		}
	case task.ClaimedByAgentID != "":
		landing = claimLandingFromAgent
		write = func(tx db.TxStores) (bool, error) {
			return tx.Tasks.TakeoverClaimFromAgent(r.Context(), orgID, id, userID, teamID, version)
		}
	default:
		landing = claimLandingFromUnclaimed
		write = func(tx db.TxStores) (bool, error) {
			return tx.Tasks.ClaimQueuedForUser(r.Context(), orgID, id, userID, teamID, version)
		}
	}
	// One transaction: the firings' cancellation requests, the claim, the
	// takeover's boundary, and the mint. The boundary precedes the mint so the
	// task never holds two un-ended conversations, and both follow the claim,
	// which is what puts the task on a team the claimant can write.
	var claimOK bool
	var written int64
	var conversationID string
	var ended []domain.Conversation
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		if landing != claimLandingIdempotent {
			if _, e = cancelFiringsForPersonClaim(r.Context(), tx, orgID, id, userID); e != nil {
				return e
			}
		}
		claimOK, e = write(tx)
		if e != nil {
			return e
		}
		if !claimOK {
			return errClaimRefused
		}
		if landing == claimLandingFromAgent {
			if ended, e = tx.Conversations.EndConversationsForTask(r.Context(), orgID, id, domain.EndedTakenOver); e != nil {
				return e
			}
		}
		if conversationID, e = mintForPersonClaim(r.Context(), tx, orgID, id, userID); e != nil {
			return e
		}
		written, e = writtenTaskVersion(r.Context(), tx, orgID, id)
		return e
	}); errors.Is(err, errClaimRefused) {
		claimOK = false
	} else if errors.Is(err, db.ErrTaskVersion) {
		writeTaskVersionConflict(w)
		return selfClaimed{}, false
	} else if writeClaimTeamRefusal(w, err) {
		return selfClaimed{}, false
	} else if err != nil {
		taskActionLog.Error("claim write failed", "task", id, "error", err)
		httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
			Reason: httpx.ReasonInternal, Message: "claim stamp failed" + localDetail(err),
		})
		return selfClaimed{}, false
	}
	if !claimOK {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason: httpx.ReasonConflict, Message: "claim race lost; refetch task and retry",
		})
		return selfClaimed{}, false
	}

	if landing != claimLandingIdempotent {
		s.ws.Broadcast(websocket.Event{
			Type:  "task_claimed",
			OrgID: orgID,
			Data:  map[string]any{},
		})
		s.publishTaskAssigned(orgID, id, "", userID)
	}
	return selfClaimed{
		jira: jiraUserClient, linear: linearUserClient, landing: landing, version: written,
		conversationID: conversationID, ended: ended,
	}, true
}

// publishTaskAssigned publishes system:task:assigned for a claim write that
// landed and gave the task a new assignee, after its transaction committed:
// exactly one of agentID and userID names who. Its subscriber tells the live
// conversations whose work overlaps the task. An idempotent re-claim changes
// nobody and publishes nothing. Best effort: the claim stands whatever
// becomes of the event.
func (s *Server) publishTaskAssigned(orgID, taskID, agentID, userID string) {
	if s.bus == nil {
		return
	}
	evt, err := events.TaskAssigned(orgID, taskID, agentID, userID)
	if err != nil {
		taskActionLog.Warn("build the task assignment event failed", "task", taskID, "error", err)
		return
	}
	s.bus.Publish(evt)
}

// linearUserCredentialMissingMessage is the claim's refusal for an
// ErrNoLinearUserCredential from ForUser, naming why the user's connection
// stopped working when the error says so.
func linearUserCredentialMissingMessage(err error) string {
	switch {
	case errors.Is(err, linear.ErrLinearUserCredentialAppRemoved):
		return "The Linear app your connection was made through was removed from this organization. Connect your Linear again to act on issues."
	case errors.Is(err, linear.ErrLinearUserCredentialRefused):
		return "Linear no longer accepts your connection: it was revoked or expired. Connect your Linear again to act on issues."
	default:
		return "connect your Linear to act on issues"
	}
}

// reassignClaim is the claim route's target_user_id arm: the user↔user
// handoff. It's a race-safe CAS on the *expected* current claimant —
// ReassignClaimToUserSystem refuses unless the task is presently claimed by
// exactly that user AND the target is an admin or member of a team associated
// with the task — so two concurrent handoffs (or one racing a takeover) can't
// clobber each other, and a claim can never land on someone who'd then be
// unable to even see the row (tasks_select RLS requires team membership on a
// claimed task) or able to see it and not act on it (a viewer).
//
// Refuses (in order): a missing task (404), a task past the version the
// caller read (409 VERSION_CONFLICT), a closed one (409), a task that isn't
// currently held by a user — unclaimed or bot-claimed tasks aren't a handoff
// target; self-claim/takeover instead (409), the caller being neither the
// current claimant nor an admin of the task's owning team (403), a task that
// changed between that read and the CAS (409 VERSION_CONFLICT), and a target
// who is an admin or member of no team that can see the task (422). Handing
// off to the current claimant is treated as an idempotent no-op, matching the
// self-claim idempotency in selfClaim. Returns the version the handoff left,
// and false when it already wrote an error response.
//
// Why the mutation runs on the admin pool (ReassignClaimToUserSystem, not
// ReassignClaimToUser): every other claim mutation ties "who's authorized to
// write" to "whose membership the resulting team_id is derived from" — the
// acting user IS the new claimant, so Postgres's tasks_update RLS naturally
// holds (they're necessarily a member of whatever team their own membership
// derived). Handoff is the one arm where the actor and the new claimant are
// different people: an admin override authorized against the task's CURRENT
// team can legitimately hand off to a user whose team is different (and who
// the admin may not share a team with at all) — team_id then consolidates to
// the target's team, which the acting admin's RLS session may not be able to
// write. Both sides of that authorization decision (actor permission below,
// target-team membership inside the CAS) are fully made in Go before this
// call, so bypassing the per-request RLS check for the write itself is safe —
// the same "authorize explicitly, then route around RLS" shape the `...System`
// convention uses elsewhere, just reached from a request path instead of a
// claims-less background goroutine.
//
// The conversation follows the team, not the claimant. A handoff that leaves
// the task on its team keeps the task's conversation, which is told who it
// works with now (delegate.TellTaskAssigned). A handoff that moves the task to
// another team ends that conversation as `reassigned` and mints the new
// holder's, in the claim write's transaction.
//
// Deliberately out of scope: no Jira-side ticket reassignment. A self-claim
// syncs a Jira ticket's assignee to the ACTING user; a handoff moves the claim
// to a THIRD party who may not have Jira connected at all, so syncing here
// would mean acting through someone else's (possibly absent) credential. The
// Jira ticket can drift from the TF claimant until the new claimant next
// interacts with it — an accepted divergence in the same family as the
// TF/GitHub drift specs/multi-team-task-model.md already calls out,
// not a bug this method needs to close.
func (s *Server) reassignClaim(w http.ResponseWriter, r *http.Request, orgID, userID, id, targetUserID string, version int64) (int64, bool) {
	var task *domain.Task
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return 0, false
	}
	if task == nil {
		notFound(w, "task")
		return 0, false
	}
	if refuseStaleTaskRead(w, task, version) {
		return 0, false
	}
	if isTerminalTaskStatus(task.Status) {
		writeTaskTerminal(w, "claim")
		return 0, false
	}
	if task.ClaimedByUserID == "" {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "task isn't claimed by a user; claim it or take it over before handing it off",
		})
		return 0, false
	}

	// Permission: the current claimant may hand off their own claim; anyone
	// else needs to admin the task's owning team — the "claimant + team
	// admin" model. Scoped to task_id ONLY, not the wider
	// task_teams visibility set: tasks_update RLS's task_teams branch only
	// ever applies to an UNCLAIMED row (both claim cols NULL) — a handoff
	// candidate is by definition already claimed, so team_id is the only
	// team that ever governs a write here, and it's the only one meaningful
	// to gate the override on. task.TeamID is guaranteed non-nil once
	// ClaimedByUserID is non-empty (tasks_claimed_requires_team CHECK).
	if task.ClaimedByUserID != userID {
		if runmode.Current() != runmode.ModeLocal {
			if task.TeamID == nil || *task.TeamID == "" {
				forbidden(w, "only the current claimant or a team admin can reassign this task")
				return 0, false
			}
			isAdmin, err := s.az.UserIsTeamAdmin(r.Context(), userID, orgID, *task.TeamID)
			if err != nil {
				internalError(w, "tasks", err)
				return 0, false
			}
			if !isAdmin {
				forbidden(w, "only the current claimant or a team admin can reassign this task")
				return 0, false
			}
		}
	}

	if targetUserID == task.ClaimedByUserID {
		// Handed to the claimant it already has: nothing to write, and the read
		// above found the task at the version the caller sent.
		return version, true
	}

	// One admin-pool transaction: the claim write, and, when it moved the task
	// to another team, the boundary on the old team's conversation and the
	// mint of the new holder's. The conversation stays with the team that made
	// it: its transcript can quote that team's private knowledge, and its
	// spend was that team's. The mint cannot run under the caller's claims,
	// since the conversation's creator is the target and the caller is often
	// not on the team it lands on, so it shares the claim write's authority.
	var ok bool
	var written int64
	var ended []domain.Conversation
	var mintedID string
	var movedTeams bool
	if err := s.tx.SystemWithTx(r.Context(), orgID, func(ts db.SystemTxStores) error {
		var e error
		ok, e = ts.Tasks.ReassignClaimToUserSystem(r.Context(), orgID, id, task.ClaimedByUserID, targetUserID, version)
		if e != nil || !ok {
			return e
		}
		moved, e := ts.Tasks.GetSystem(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if moved == nil {
			return fmt.Errorf("task %s is not readable in the transaction that reassigned it", id)
		}
		written = moved.Version
		// The write landed at the version the read above saw, so that read's
		// team is the team the task was on before it.
		movedTeams = teamIDString(task.TeamID) != teamIDString(moved.TeamID)
		if !movedTeams {
			return nil
		}
		if ended, e = ts.Conversations.EndConversationsForTaskSystem(r.Context(), orgID, id, domain.EndedReassigned); e != nil {
			return e
		}
		minted, e := ts.Conversations.MintForPersonClaimSystem(r.Context(), orgID, id, targetUserID)
		if e != nil {
			return fmt.Errorf("mint the new holder's conversation: %w", e)
		}
		if minted != nil {
			mintedID = minted.ID
		}
		return nil
	}); errors.Is(err, db.ErrTaskVersion) {
		writeTaskVersionConflict(w)
		return 0, false
	} else if err != nil {
		taskActionLog.Error("reassign claim flip failed", "task", id, "error", err)
		httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
			Reason: httpx.ReasonInternal, Message: "reassign failed" + localDetail(err),
		})
		return 0, false
	}
	if !ok {
		// A task that moved since the read above answered ErrTaskVersion,
		// so this one is still at that version, with the claim and status
		// that read checked. What refused the CAS is the target-team
		// guard, which no retry fixes: the target is an admin or member of
		// no team that can see this task, either because they are on none
		// of those teams or because they can only view the ones they are
		// on. That is a reference to a user in the wrong team rather than a
		// state conflict. The one other way to match no row is a task
		// deleted in between, which the re-read tells apart.
		var fresh *domain.Task
		_ = s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			fresh, e = tx.Tasks.GetSystem(r.Context(), orgID, id)
			return e
		})
		if fresh == nil {
			notFound(w, "task")
			return 0, false
		}
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonCrossTeamRef,
			Message: "target user isn't a member with write access on a team that can see this task",
			Field:   "target_user_id",
		})
		return 0, false
	}

	s.ws.Broadcast(websocket.Event{
		Type:  "task_claimed",
		OrgID: orgID,
		Data:  map[string]any{},
	})
	// A move to another team is a boundary, and what follows it is what
	// follows a takeover: the old team's runs stop, and the rows the
	// transaction ended get their memory doorbell, warm-wait ring, and frames.
	// A handoff within the team keeps the conversation and stops nothing.
	if movedTeams {
		cleanupCtx := context.WithoutCancel(r.Context())
		s.stopTaskConversations(cleanupCtx, orgID, userID, id, delegate.StopCauseTaskReassigned, mintedID)
		s.afterConversationsEnded(cleanupCtx, orgID, ended)
	}
	s.publishTaskAssigned(orgID, id, "", targetUserID)
	return written, true
}

// taskDelegateRequest is the body of POST /api/tasks/{id}/delegate.
type taskDelegateRequest struct {
	// BlueprintID names the blueprint the run executes. Required: an absent
	// one is refused with the other blueprint refusals, 422 on blueprint_id.
	BlueprintID string `json:"blueprint_id,omitempty" doc:"The id of the blueprint the run carries out, which must belong to the team the task lands on. It is required."`
	// TeamID is the team the task lands on and the run belongs to, one of
	// the caller's candidates (resolveClaimTeam). It may be absent when the
	// caller has one candidate. The blueprint must be that team's.
	TeamID *string `json:"team_id,omitempty" doc:"The id of the team the task lands on and the run belongs to, one of the task's claim_team_ids; absent when there is only one."`
	// Version is the task version the caller read (taskJSON.Version).
	Version *int64 `json:"version" doc:"The version the caller's read of the task returned; the delegation is refused when the task has changed since. It is required."`
}

// taskDelegateResponse is what POST /api/tasks/{id}/delegate answers: the
// task's state after the agent's claim landed, and the run's first
// conversation.
type taskDelegateResponse struct {
	Status  string `json:"status" doc:"The task's lane after the agent claimed it."`
	Version int64  `json:"version" doc:"The task's version after the agent claimed it, which the caller's next write to the task sends."`
	// ConversationID is left off when the step's conversation could not be
	// read back, rather than naming an id that resolves to nothing.
	ConversationID string `json:"conversation_id,omitempty" doc:"The id of the conversation of the run's first step, absent when it could not be read."`
}

var opTaskDelegate = httpx.NewJSONOp[taskDelegateRequest, taskDelegateResponse](httpx.OpSpec{
	Pattern:     "POST /api/tasks/{id}/delegate",
	ID:          "delegate_task",
	Summary:     "Delegate a task to the agent",
	Description: "Hands the task to the team's agent and starts a run of the blueprint on it, stopping any run already working on the task, which keeps the deliverables it left. Returns the task's lane and version after the handoff and the run's first conversation.",
	PathParams:  map[string]string{"id": "The task's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// handleTaskDelegate hands a task to the agent and spawns its run.
//
// Every check that can refuse runs before the first write: the task's own
// state, the team the claim lands on and its bot, then the plan of the run. A
// refused delegate therefore leaves the task, its version, its claim, its
// conversations and its blueprint run as they were. Once the claim lands only
// races and faults remain, and those answer SPAWN_FAILED.
//
// POST /api/tasks/{id}/delegate
func (s *Server) handleTaskDelegate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := taskIDOr404(w, r)
	if !ok {
		return
	}

	req, ok := opTaskDelegate.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	version := taskWriteVersion(&v, req.Version)
	claimTeamField(&v, req.TeamID)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

	// A conversation an executor holds can only be stopped by that executor,
	// so the new run could not be minted in this request. Refuse before any
	// write rather than tear the old run down and then fail to replace it.
	if s.spawner != nil {
		if err := s.spawner.CheckTaskUnheld(r.Context(), orgID, id); err != nil {
			writeTaskHeld(w, err)
			return
		}
	}

	target, ok := s.readAgentClaimTarget(w, r, orgID, userID, id, req.TeamID, version)
	if !ok {
		return
	}

	// The run is planned for the team the claim will land the task on, before
	// the claim, so every refusal the spawner makes comes before anything is
	// written. It is not planned again after the claim: the firing
	// transaction refuses what it can see of the task's state (another run,
	// an archived team) as SPAWN_FAILED, and the rest of the plan stands.
	var plan delegate.DelegationPlan
	if s.spawner != nil {
		var err error
		plan, err = s.spawner.PlanDelegation(target.task, target.teamID, delegate.DelegateOpts{
			OrgID:               orgID,
			ExplicitBlueprintID: req.BlueprintID,
			TriggerType:         "manual",
			CreatorUserID:       userID,
			// The agent this route claims the task with, so the run's frozen
			// blueprint_run actor matches the task claim.
			ActorAgentID: target.agentID,
		})
		if err != nil {
			writeDelegatePlanRefusal(w, err)
			return
		}
	}

	// The handoff comes before the stop below, because its version check is
	// what keeps a stale caller from stopping another person's run.
	handed, ok := s.handOffAgentClaim(w, r, orgID, userID, id, version, target.agentID, target.teamID)
	if !ok {
		return
	}
	// Re-delegating is still a handoff off whatever was in flight: stop the
	// previous conversation before the new run starts. Its deliverables carry —
	// the next blueprint pushes to the draft PR the last one opened instead of
	// opening a second.
	//
	// The boundary is stamped here and not after the fire, and the order is
	// the invariant: step 0 of the new delegation must never be minted onto a
	// task whose prior conversation is still un-ended, or the task momentarily
	// has two live ones and nothing can say which it is about.
	//
	// Nothing holds those conversations (checked above), so their stops are
	// settled here rather than on the dispatcher's next pass: the settlement
	// cancels the blueprint run the stop marked, which is what frees the
	// task's one active run for the mint below.
	cleanupCtx := context.WithoutCancel(r.Context())
	s.stopTaskConversations(cleanupCtx, orgID, userID, id, delegate.StopCauseTaskDelegated, "")
	s.endTaskConversations(cleanupCtx, orgID, userID, id, domain.EndedDelegated)
	if s.spawner != nil {
		if err := s.spawner.SettleTaskStops(cleanupCtx, orgID, id); err != nil {
			taskActionLog.Warn("settle the task's stops before re-delegating failed; the dispatcher settles them on its next pass",
				"task", id, "error", err)
		}
	}

	// The version is the one the handoff left, so the client's next write on
	// the task names it without reading the task again.
	response := taskDelegateResponse{Status: handed.Status, Version: handed.Version}
	if s.spawner != nil {
		if !s.fireDelegation(w, r, orgID, userID, plan, &response) {
			return
		}
	}
	opTaskDelegate.Write(w, response)
}

// agentClaimTarget is what a delegate's claim will write, read before it
// writes: the task as the caller's claims read it, the team the claim lands
// the task on, and the agent that takes it.
type agentClaimTarget struct {
	task    domain.Task
	teamID  string
	agentID string
}

// readAgentClaimTarget makes every read and check the delegate's claim makes
// before its write, and writes nothing: the task exists and is at the version
// the caller read, it isn't terminal, nobody else holds it, the team the body
// names (pickedTeam, or the caller's one candidate) is one it can land on and
// the one it will be on after the handoff, and the bot is enabled for that
// team. ok=false once it has written the refusal.
func (s *Server) readAgentClaimTarget(w http.ResponseWriter, r *http.Request, orgID, userID, id string, pickedTeam *string, version int64) (agentClaimTarget, bool) {
	// Pre-load to disambiguate HandoffRefused (404 missing / 409 terminal /
	// 409 theft) and to gate the bot-enablement check on the task's own team.
	var task *domain.Task
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return agentClaimTarget{}, false
	}
	if task == nil {
		notFound(w, "task")
		return agentClaimTarget{}, false
	}
	if refuseStaleTaskRead(w, task, version) {
		return agentClaimTarget{}, false
	}
	if isTerminalTaskStatus(task.Status) {
		writeTaskTerminal(w, "delegate")
		return agentClaimTarget{}, false
	}
	// Ahead of the team, as on the claim route: a task another person holds
	// is refused whatever team the body names, so the caller is never asked
	// to select a team for a delegate the handoff would refuse.
	if task.ClaimedByUserID != "" && task.ClaimedByUserID != userID {
		writeClaimedByAnotherUser(w)
		return agentClaimTarget{}, false
	}

	// Gate on the team the claim will land the task on, not the pre-handoff
	// task.TeamID: for an unclaimed task visible to several teams the latter
	// can be a team the caller isn't in, whose team_agents row RLS hides —
	// wrongly reporting the bot disabled and rejecting a legit delegate. The
	// same read refuses a delegation with no team to land on before anything
	// is written.
	claimTeamID, ok := s.resolveClaimTeam(w, r, orgID, userID, id, pickedTeam)
	if !ok {
		return agentClaimTarget{}, false
	}
	a, enabled, err := s.agentEnabledForTeam(r.Context(), orgID, userID, claimTeamID)
	if err != nil {
		taskActionLog.Error("delegate aborted", "task", id, "error", err)
		httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
			Reason: httpx.ReasonInternal, Message: "delegate failed" + localDetail(err),
		})
		return agentClaimTarget{}, false
	}
	// A task the agent already holds stays on its team: the handoff writes
	// nothing, and the run belongs to the task's team. That team differs from
	// the one resolved above only when the caller does not write it, so the
	// run would execute under a team the caller cannot delegate for.
	if task.ClaimedByAgentID == a.ID && (task.TeamID == nil || *task.TeamID != claimTeamID) {
		writeNoClaimTeam(w)
		return agentClaimTarget{}, false
	}
	if !enabled {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "bot is disabled for this team; enable it in team settings to delegate",
		})
		return agentClaimTarget{}, false
	}
	return agentClaimTarget{task: *task, teamID: claimTeamID, agentID: a.ID}, true
}

// handOffAgentClaim moves the task to bot ownership on teamID:
// HandoffAgentClaim handles unclaimed→bot, my-claim→bot and idempotent
// bot-owns, and refuses a different-user claim, a task past the version the
// caller read, and a team that stopped being one the caller can land the
// task on, each writing nothing. Its caller has made readAgentClaimTarget's
// checks. Returns the row the handoff left, and ok=false when it already
// wrote an error response.
func (s *Server) handOffAgentClaim(w http.ResponseWriter, r *http.Request, orgID, userID, id string, version int64, agentID, teamID string) (domain.Task, bool) {
	var result db.HandoffResult
	var handed domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		result, handed, e = tx.Tasks.HandoffAgentClaim(r.Context(), orgID, id, agentID, userID, teamID, version)
		return e
	}); errors.Is(err, db.ErrTaskVersion) {
		// Changed since the caller's read. Nothing was written, and the route
		// stops and spawns nothing.
		writeTaskVersionConflict(w)
		return domain.Task{}, false
	} else if writeClaimTeamRefusal(w, err) {
		// The caller's candidates changed since readAgentClaimTarget read
		// them.
		return domain.Task{}, false
	} else if err != nil {
		taskActionLog.Error("failed to stamp agent claim", "task", id, "error", err)
		httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
			Reason: httpx.ReasonInternal, Message: "claim stamp failed" + localDetail(err),
		})
		return domain.Task{}, false
	}
	if result == db.HandoffRefused {
		// readAgentClaimTarget ruled out missing, terminal and another
		// person's claim as it read them, so what remains is a claim that
		// landed since. The TOCTOU window is narrow but real; the user
		// retries from a fresh view either way.
		writeClaimedByAnotherUser(w)
		return domain.Task{}, false
	}

	if result == db.HandoffChanged {
		s.ws.Broadcast(websocket.Event{
			Type:  "task_claimed",
			OrgID: orgID,
			Data:  map[string]any{},
		})
		s.publishTaskAssigned(orgID, id, agentID, "")
	}
	return handed, true
}

// stopTaskConversations stops every active run on a task and cancels the
// blueprint behind each. Its callers own a lifecycle one layer up and have
// already decided this attempt at the task is over, so the blueprints go
// terminal with their runs rather than freezing 'running' — the plain
// conversation stop is for a user pausing work they mean to come back to.
//
// Every gesture that moves a task off whoever was working it comes through
// here — claim, re-delegate, return to queue — so there is one stop pass
// rather than one per verb. Closing a task is not one of them: a dismiss or a
// complete stops nothing. None of them touches the task's deliverables: its
// draft PRs and staged reviews survive every one of those gestures.
//
// cause is the lifecycle event the caller is acting on. It reaches each stopped
// transcript verbatim, so it is the whole explanation a human reading that
// history gets.
//
// Best-effort per conversation: a failed stop is logged and the remaining ones
// are still attempted, because the task has already moved and leaving its
// other runs live is the worse half-state.
//
// ctx must already be detached from the request (context.WithoutCancel): a
// client disconnect after the response must not leave a live agent running.
// The detach belongs to the caller because a caller with further cleanup of its
// own needs every part of it on one context.
//
// keepConversationID names one conversation the pass leaves alone, "" for
// none: the conversation a person's claim minted, which is unsettled like the
// rest and is the one the task is about from here on.
func (s *Server) stopTaskConversations(ctx context.Context, orgID, userID, taskID string, cause delegate.StopCause, keepConversationID string) {
	if s.spawner == nil {
		return
	}
	var ids []string
	if err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		ids, e = tx.Conversations.ActiveIDsForTask(ctx, orgID, taskID)
		return e
	}); err != nil {
		taskActionLog.Error("active-conversation lookup failed", "task", taskID, "error", err)
		return
	}
	for _, conversationID := range ids {
		if keepConversationID != "" && conversationID == keepConversationID {
			continue
		}
		if err := s.spawner.StopConversationAndCancelBlueprint(orgID, conversationID, userID, cause); err != nil {
			taskActionLog.Warn("stop conversation failed", "conversation", conversationID, "task", taskID, "error", err)
		}
	}
}

// endTaskConversations stamps the task's boundary: every conversation still
// live on the task stops being the one the task is about, with the reason it
// stopped being it. One door for the two gestures that stamp after their
// write commits (requeue, delegate), because the row they each write is
// identical and only the reason differs. A takeover and a reassign across
// teams stamp inside the transaction that mints the next conversation
// instead, and share what follows the stamp (afterConversationsEnded).
//
// It runs behind stopTaskConversations, never instead of it: the stop ends the
// PROCESS and this ends the CONVERSATION, and a stop that failed (a dead
// executor, a lost signal) must still leave a row that says the task moved on.
// Which is also why it is unconditional on status — a conversation that
// concluded on its own an hour ago is just as much not this task's live one.
//
// Best-effort and logged, like every other step of these teardowns: the task
// has already flipped by the time this runs, and failing the response would
// misreport a change that landed.
func (s *Server) endTaskConversations(ctx context.Context, orgID, userID, taskID string, reason domain.EndedReason) {
	var ended []domain.Conversation
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		ended, e = tx.Conversations.EndConversationsForTask(ctx, orgID, taskID, reason)
		return e
	}); err != nil {
		taskActionLog.Error("stamp the task boundary on its conversations failed",
			"task", taskID, "ended_reason", string(reason), "error", err)
		return
	}
	s.afterConversationsEnded(ctx, orgID, ended)
}

// afterConversationsEnded is what follows a boundary stamp, for the rows the
// stamp returned: the memory doorbell, the warm-wait ring, and one
// conversation_update each. Its callers are endTaskConversations, which stamps
// after its gesture commits, and the claim and reassign routes, which stamp
// inside the transaction that also mints the next conversation. Best-effort,
// and a no-op for no rows.
func (s *Server) afterConversationsEnded(ctx context.Context, orgID string, ended []domain.Conversation) {
	// The memory doorbell, once per stamped row and before the broadcast: each
	// of these conversations may have ended without its agent having written
	// its memory, and the task's next delegation does not open until every one
	// of them has a row. Rung first, so the generation is already under way by
	// the time the station that refetches asks why its composer is disabled.
	//
	// Only the rows this call stamped — an already-ended conversation comes
	// back from neither door, and its debt (if it still has one) was rung for
	// by the boundary that actually ended it. And only the ones that ran: a
	// conversation with no turn owes no memory, so there is nothing to
	// generate and no attempt to open for it.
	for i := range ended {
		if ended[i].HasRun {
			s.kickMemoryOwed(orgID, ended[i].ID)
		}
	}
	// A warm claim on an ended conversation still holds the task's tree and
	// an executor slot. Wake it so it releases them now.
	if s.spawner != nil {
		s.spawner.RingWarmWaitsForEnded(ctx, orgID, ended)
	}
	// One conversation_update per stamped row. The rows carry no new status —
	// what changed is whether a follow-up can land — so this is an
	// invalidation signal: a run station holding one of them refetches and
	// finds the composer disabled with the rung that refused it.
	for i := range ended {
		s.frames.ConversationUpdate(ctx, orgID, ended[i].ID, conversationframe.StatusUpdate{Status: ended[i].Status})
	}
}

// syncJiraClaim assigns a claimed Jira ticket to the acting user and
// transitions it to in-progress, in a detached goroutine. Best-effort: the
// claim has already landed in TF by the time this runs, so a Jira-side failure
// is logged rather than failing the response. The claim guard skips the
// transition when the ticket is already assigned-to-self and already in any
// in-progress member, so a second claim on the same issue doesn't
// re-assign/re-transition redundantly. jiraUserClient is the acting user's own
// client, so the assignment attributes to the claimer.
func (s *Server) syncJiraClaim(r *http.Request, orgID, userID, id string, jiraUserClient *jira.Client) {
	// Fetch the task and its team's status rules in one WithTx so the rule read
	// goes through the app-pool ListForTeam under jira_rules_select RLS.
	var task *domain.Task
	var target jiraTaskTarget
	err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		orgSet, e := tx.Orgs.GetSettings(r.Context(), orgID)
		if e != nil {
			return e
		}
		target = lookupJiraTaskTarget(r.Context(), tx, orgID, domain.EntityScope("jira", orgSet), task)
		return nil
	})
	if err != nil || task == nil || task.EntitySource != "jira" {
		return
	}
	rule := target.Rule
	if rule == nil || rule.InProgressCanonical.IsZero() {
		jiraLog.Warn("claim guard: no in_progress rule configured for project, skipping transition", "ticket", task.EntitySourceID)
		return
	}
	// Detached from the request: the sync outlives the response, so it uses a
	// background context.
	go applyJiraClaim(context.Background(), jiraUserClient, orgID, target.IssueRef, task.EntitySourceID, *rule)
}

// applyJiraClaim is the write a claim makes in Jira, as the client's user:
// assign the ticket to them unless it already is, and transition it to the
// rule's in-progress canonical unless it already sits in an in-progress
// member. A failed assign stops before the transition. Every failure is
// logged rather than returned: the claim has landed in TF by the time this
// runs. The transition's outcome is recorded in orgID as the bot mirror's is
// (delegate.NoteJiraMirrorMove). Jira is addressed by issue, the ticket's id;
// issueKey only labels the log lines.
func applyJiraClaim(ctx context.Context, c *jira.Client, orgID, issue, issueKey string, rule domain.JiraProjectStatusRules) {
	state := c.GetClaimState(ctx, issue)
	ipCanonical := jira.Status{ID: rule.InProgressCanonical.ID, Name: rule.InProgressCanonical.Name}

	needAssign := state == nil || !state.AssignedToSelf
	needTransition := state == nil || !domain.ContainsStatus(rule.InProgressMembers, claimStatusRef(state))

	if !needAssign && !needTransition {
		jiraLog.Info("claim guard: already assigned to self and in in-progress, skipping", "issue", issueKey, "status", state.StatusName)
		return
	}

	if needAssign {
		if err := c.AssignToSelf(ctx, issue); err != nil {
			jiraLog.Error("failed to assign", "issue", issueKey, "error", err)
			return
		}
	}
	if needTransition {
		err := c.TransitionTo(ctx, issue, ipCanonical)
		delegate.NoteJiraMirrorMove(ctx, c, orgID, rule, issueKey, err)
		if err != nil {
			jiraLog.Error("failed to transition", "issue", issueKey, "status", ipCanonical.Name, "error", err)
		}
	}
}

// syncLinearClaim assigns a claimed Linear issue to the acting user and moves
// it to the Linear team's in-progress state, in a detached goroutine. The
// Linear sibling of syncJiraClaim, best-effort for the same reason: the claim
// has already landed in TF. linearUserClient is the acting user's own client,
// so Linear records the person who claimed the task as the one who assigned
// and moved the issue.
func (s *Server) syncLinearClaim(r *http.Request, orgID, userID, id string, linearUserClient *linear.Client) {
	// The task, its rule and the acting user's Linear identity in one read, so
	// the rule read goes through the app-pool ListForTeam under RLS.
	var task *domain.Task
	var target linearSyncTarget
	err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		if e != nil || task == nil || task.EntitySource != "linear" {
			return e
		}
		target, e = readLinearSyncTarget(r.Context(), tx, orgID, userID, task)
		return e
	})
	if err != nil {
		linearLog.Warn("claim guard: read task for sync failed, skipping", "task", id, "error", err)
		return
	}
	if task == nil || task.EntitySource != "linear" {
		return
	}
	if target.rule == nil || target.rule.InProgressCanonical.IsZero() {
		linearLog.Warn("claim guard: no in_progress rule for Linear team, skipping transition", "issue", target.identifier)
		return
	}
	// The connect flow writes the credential and the identity in one
	// transaction, so a user with a credential and no identity is not a state
	// the product produces. Without the id there is nobody to assign.
	if target.linearUserID == "" {
		linearLog.Warn("claim guard: no Linear identity stored for the acting user, skipping", "issue", target.identifier, "user", userID)
		return
	}
	// Every Linear entity carries its issue's UUID, so this is a row that
	// vanished between the claim and this read.
	if target.issueID == "" {
		linearLog.Warn("claim guard: the task's entity names no Linear issue id, skipping", "issue", target.identifier, "task", id)
		return
	}
	// Detached from the request: the sync outlives the response, so it uses a
	// background context.
	go applyLinearClaim(context.Background(), linearUserClient, orgID, target.issueID, target.identifier, target.linearUserID, *target.rule)
}

// fireDelegation fires the planned run and records the conversation_id on the
// response. The caller has already verified s.spawner != nil and landed
// the bot's claim. A failure here is a race or a fault that came after the
// claim, which stays: the client re-reads the task on SPAWN_FAILED and finds
// it in the bot's lane without a run. Returns false once an error response is
// written.
func (s *Server) fireDelegation(w http.ResponseWriter, r *http.Request, orgID, userID string, plan delegate.DelegationPlan, response *taskDelegateResponse) bool {
	blueprintRunID, err := s.spawner.FireDelegation(plan)
	if err != nil {
		writeDelegateSpawnError(w, err)
		return false
	}
	// The field names a conversation, so it carries one: FireDelegation
	// returns the blueprint_run it minted, and the run a client follows from
	// here is that blueprint's first step. The fire enqueues step 0 before it
	// returns, so the row exists by now; a read that comes back empty leaves
	// the field off rather than substituting an id that resolves against
	// nothing, and the client's own task refetch reconciles.
	if conversationID := s.firstStepConversationID(r.Context(), orgID, userID, blueprintRunID); conversationID != "" {
		response.ConversationID = conversationID
	}
	return true
}

// firstStepConversationID resolves the conversation a caller watches a fresh
// blueprint run through — its lowest-indexed step. Read under the requesting
// user's claims, like every other read this handler makes; a manual
// delegation's own creator can always see the step it just minted.
//
// Best-effort by design: it answers "" for a read failure or an empty result,
// because the delegation itself has already committed and its response must
// not turn on a lookup that only names the run.
func (s *Server) firstStepConversationID(ctx context.Context, orgID, userID, blueprintRunID string) string {
	var convs []domain.Conversation
	if err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		convs, e = tx.Blueprints.ConversationsForBlueprint(ctx, orgID, blueprintRunID)
		return e
	}); err != nil {
		taskActionLog.Warn("resolve first step conversation for delegation failed; responding without conversation_id",
			"blueprint_run", blueprintRunID, "error", err)
		return ""
	}
	if len(convs) == 0 {
		taskActionLog.Warn("delegation minted no readable step conversation; responding without conversation_id",
			"blueprint_run", blueprintRunID)
		return ""
	}
	// ConversationsForBlueprint orders by step index, so the first row is step 0.
	return convs[0].ID
}

// writeDelegatePlanRefusal answers a delegation PlanDelegation refused. The
// plan runs before the claim, so nothing was written, and the reason is never
// SPAWN_FAILED: the caller's version still names the task, and a retry after
// the fix needs no re-read.
//
// A refusal about the blueprint the body named is the request's fault, 422 on
// blueprint_id: INVALID_FIELD for one that can't be used at all, and
// CROSS_TEAM_REF for one that belongs to another team than the task lands
// on, which is what a trigger naming another team's blueprint answers. The
// rest are 409s with no field, because nothing in the body names a model or
// a cap: NOT_CONFIGURED for setup the org or team has not
// done, CONFLICT for a decision somebody made (a model taken out of the
// team's set, a spend cap reached). Their messages are TF's own and name the
// model, the provider, or the setting to change, so they go out verbatim; a
// spend cap's is the exception, below. The refusals are the ones
// delegate.RefusalCause classifies, which is also how the router tells them
// from a fault.
func writeDelegatePlanRefusal(w http.ResponseWriter, err error) {
	cause, _ := delegate.RefusalCause(err)
	var capErr *delegate.CostCapError
	switch {
	case errors.Is(err, delegate.ErrBlueprintOtherTeam):
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, crossTeamBlueprintFault)
	case cause == domain.DelegationRefusedSpendCap && errors.As(err, &capErr):
		// Spend is an org admin's read, so the figures stay in the server log,
		// and in local mode, where the one person is the admin, the message.
		delegateSpawnLog.Info("delegate refused by a daily spend cap", "error", err)
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: costCapRefusal(capErr) + localDetail(err),
		})
	case cause == domain.DelegationRefusedNoDefaultModel, cause == domain.DelegationRefusedProviderNotConnected:
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonNotConfigured,
			Message: err.Error(),
		})
	case cause == domain.DelegationRefusedModelNotEnabled:
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: err.Error(),
		})
	case cause == domain.DelegationRefusedBlueprintUnusable:
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: err.Error(),
			Field:   "blueprint_id",
		})
	default:
		// A read the plan needed failed: the task's repository, the team's
		// model configuration, the blueprint, or a prompt.
		httpx.InternalError(w, "delegate", err)
	}
}

// costCapRefusal names the spend cap that refused a delegation and who can
// raise it.
func costCapRefusal(e *delegate.CostCapError) string {
	if e.Scope == delegate.CostCapScopeTeam {
		return fmt.Sprintf("Team %s's daily spend cap is reached. An org admin can raise it in Settings.", e.TeamName)
	}
	return "The organization's daily spend cap is reached. An org admin can raise it in Settings."
}

// writeDelegateSpawnError answers a FireDelegation failure. The claim landed
// before the fire, and it stays, so every arm carries reason SPAWN_FAILED:
// the reason, not the status, is what tells a client the claim survived and
// that it should re-read the task before it retries. A task that another run
// took or whose team was archived in the gap is a conflict (409); anything
// else is a spawn or DB fault (500, logged and redacted).
func writeDelegateSpawnError(w http.ResponseWriter, err error) {
	// Something else took this task's single active run between the route's
	// held check and its mint: an executor claimed the conversation being
	// replaced, a second delegate gesture minted first, or an event firing
	// did. 409 for the same fault class every other lost race on this route
	// reports, and no `field`: nothing the caller sent is wrong.
	if errors.Is(err, delegate.ErrTaskBusy) {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonSpawnFailed,
			Message: "another run became active on this task; refresh and try again",
		})
		return
	}
	// The task's team was archived, so its run would belong to a team nobody
	// can see and nothing stops. A conflict with the task's state, not a
	// fault in the body. The handoff refuses a team that is archived when it
	// reads it, so this answers an archive that committed after that read and
	// before the run insert. The bot claim that leaves stays: the task is on an
	// archived team either way, and a claimed task there is visible to nobody.
	if errors.Is(err, delegate.ErrTeamArchived) {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonSpawnFailed,
			Message: "this task's team is archived",
		})
		return
	}
	writeDelegateSpawnInternal(w, err)
}

// claimTeamField checks the shape of a claim's or a delegate's team_id: when
// present it must be a team id. Which team it may be is resolveClaimTeam's
// to answer, once the task is read.
func claimTeamField(v *httpx.Validation, teamID *string) {
	if teamID == nil {
		return
	}
	if _, err := uuid.Parse(*teamID); err != nil {
		v.Invalid("team_id", "team_id must be a team id")
	}
}

// resolveClaimTeam resolves the team a person's claim of a task, or their
// delegate of it, lands the task on, for both routes and before either
// writes. picked is the body's team_id, already checked by claimTeamField.
//
// The caller's candidates decide it (db.TaskStore.ClaimTeamCandidates): a
// picked team must be one of them, and an absent one resolves only when there
// is exactly one. Nothing else is consulted: not the caller's last-used team,
// which is a default for creating things, and the answer is not recorded as
// one. ok=false once it has written the refusal.
func (s *Server) resolveClaimTeam(w http.ResponseWriter, r *http.Request, orgID, userID, taskID string, picked *string) (string, bool) {
	var candidates []string
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		candidates, e = tx.Tasks.ClaimTeamCandidates(r.Context(), orgID, taskID, userID)
		if e != nil || len(candidates) > 0 {
			return e
		}
		return tx.Tasks.NoClaimTeamReason(r.Context(), orgID, taskID, userID)
	}); writeClaimTeamRefusal(w, err) {
		return "", false
	} else if err != nil {
		internalError(w, "tasks", err)
		return "", false
	}
	switch {
	case picked != nil:
		// The candidates are spelled as stored; uuid.Parse accepts other
		// spellings of the same id.
		team := *picked
		if parsed, err := uuid.Parse(team); err == nil {
			team = parsed.String()
		}
		if !slices.Contains(candidates, team) {
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
				Reason:  httpx.ReasonCrossTeamRef,
				Message: "this task can't land on that team",
				Field:   "team_id",
			})
			return "", false
		}
		return team, true
	case len(candidates) > 1:
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: "a team is required: more than one of your teams can take this task, so select one",
			Field:   "team_id",
		})
		return "", false
	}
	return candidates[0], true
}

// writeClaimTeamRefusal answers a claim or delegation refused over the team
// the task lands on, and reports whether err was one of those refusals.
// Nothing was written for any of them:
//
//   - db.ErrNoActiveClaimTeam: the caller has no team to land the task on
//     because one is archived. 403 TEAM_ARCHIVED (writeNoActiveClaimTeam).
//   - db.ErrNoClaimTeam: the caller has no team to land the task on, and none
//     is archived. 403 FORBIDDEN (writeNoClaimTeam).
//   - db.ErrClaimTeamNotCandidate: the write refused the team
//     resolveClaimTeam had resolved, so the task's teams, or the caller's
//     membership of them, changed in between. 409 CONFLICT.
func writeClaimTeamRefusal(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, db.ErrNoActiveClaimTeam):
		writeNoActiveClaimTeam(w)
	case errors.Is(err, db.ErrNoClaimTeam):
		writeNoClaimTeam(w)
	case errors.Is(err, db.ErrClaimTeamNotCandidate):
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "the task's teams changed; read it again",
		})
	default:
		return false
	}
	return true
}

// writeClaimedByAnotherUser answers a delegate of a task another person
// holds: 409 CONFLICT, since the delegate would take the task from them.
func writeClaimedByAnotherUser(w http.ResponseWriter) {
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
		Reason: httpx.ReasonConflict, Message: "task is claimed by another user; refusing to steal",
	})
}

// writeNoClaimTeam answers a claim or delegation refused because the caller
// writes none of the teams the task can land on (db.ErrNoClaimTeam): they are
// on neither its own team nor a team that can see it, or only view the ones
// they are on. It is not the archived answer, because restoring a team would
// not change it. 403 FORBIDDEN, the answer a missing write role gets
// elsewhere, and no field: no team_id would be accepted.
func writeNoClaimTeam(w http.ResponseWriter) {
	httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{
		Reason:  httpx.ReasonForbidden,
		Message: "you aren't a member with write access on a team that can take this task",
	})
}

// writeNoActiveClaimTeam answers a claim or delegation refused because the
// caller has no team to land the task on and an archived team is why
// (db.ErrNoActiveClaimTeam): the task's team is archived, and the caller
// writes no active team that can see it. Nothing was written. 403 TEAM_ARCHIVED, the archive write-block's answer, because the
// fault is the same one: nothing lands on an archived team, and a retry gets
// the same refusal until the team is restored.
func writeNoActiveClaimTeam(w http.ResponseWriter) {
	httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{
		Reason:  httpx.ReasonTeamArchived,
		Message: "this task's team is archived, and you're on no active team it can move to",
	})
}

// writeTaskHeld answers a delegate refused because an executor holds a
// conversation on the task. 409 with CONFLICT rather than SPAWN_FAILED: the
// refusal comes before the claim stamp, so nothing landed.
func writeTaskHeld(w http.ResponseWriter, err error) {
	if errors.Is(err, delegate.ErrTaskHeld) || errors.Is(err, delegate.ErrTaskStopping) {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: err.Error(),
		})
		return
	}
	httpx.InternalError(w, "delegate", err)
}

// writeDelegateSpawnInternal is the 500 arm of the post-claim spawn failure:
// same logging and multi-mode redaction posture as httpx.InternalError, but
// under the SPAWN_FAILED reason a plain internal error must never carry.
func writeDelegateSpawnInternal(w http.ResponseWriter, err error) {
	delegateSpawnLog.Error("delegate spawn failed after claim stamp", "error", err)
	httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
		Reason:  httpx.ReasonSpawnFailed,
		Message: "delegate spawn failed" + localDetail(err),
	})
}
