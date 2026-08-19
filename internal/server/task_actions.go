package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// The task verb routes: claim, delegate, requeue and undo. Each earns its
// verb by carrying an effect a field write can't express — an external Jira
// write, a spawned run, artifact teardown, an audit reversal. The lifecycle
// axis (dismissed / done / snoozed / the in_progress → in_review stages) is a
// field write and lives on PATCH /api/tasks/{id} in tasks.go; requeue and undo
// live there too, next to the finalizer they share.
//
// swipe_events is a "state-change log," not a "user-gesture log." For
// lifecycle actions the write IS the state change, so the audit + lifecycle
// UPDATE land together. For responsibility-axis actions (claim / reassign /
// delegate) the real state change is a separate guarded UPDATE that can refuse
// the gesture; audit only lands after that UPDATE accepts, so a refused
// gesture leaves no trace.

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
	TargetUserID *string `json:"target_user_id,omitempty"`
	// HesitationMs is the dwell time before the gesture, recorded on the
	// swipe_events row this route writes.
	HesitationMs int `json:"hesitation_ms,omitempty"`
}

// handleTaskClaim moves a task's user claim. Self-claim (no target) takes
// ownership for the caller, tears down whatever the agent had in flight, and
// syncs a Jira-backed ticket to the caller. Handoff (a target) moves an
// existing user claim to someone else and deliberately touches neither.
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

	var req taskClaimRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	if req.TargetUserID != nil && strings.TrimSpace(*req.TargetUserID) == "" {
		v.Invalid("target_user_id", "target_user_id must name a user; omit it to claim the task yourself")
	}
	validateHesitation(&v, req.HesitationMs)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// Viewers can't act on a team's tasks (TFAC-447). A claim is a
	// team-scoped write, so gate it here with one clean 403 rather than
	// letting the store UPDATE fall off the tasks_update RLS policy as a
	// confusing 409/500. A task the viewer can't see at all still 404s
	// downstream — RequireTaskWrite doesn't mask it.
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

	if req.TargetUserID != nil {
		newStatus, ok := s.reassignClaim(w, r, orgID, userID, id, *req.TargetUserID, req.HesitationMs)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": newStatus})
		return
	}

	newStatus, jiraUserClient, ok := s.selfClaim(w, r, orgID, userID, id, req.HesitationMs)
	if !ok {
		return
	}
	// Taking a task onto a human's plate takes it off the agent's: resolve
	// every unresolved artifact the task holds and cancel any in-flight run.
	s.teardownTaskConversations(r, orgID, userID, id, discardOutcomeClaimed)
	if jiraUserClient != nil {
		s.syncJiraClaim(r, orgID, userID, id, jiraUserClient)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": newStatus})
}

// selfClaim is the claim route's no-target arm: a race-safe transition to the
// caller's ownership with three accept paths (idempotent same-user, takeover
// from bot, claim from unclaimed) and one refuse path (different user owns it
// → 409). For a Jira-backed task it resolves the acting user's Jira client up
// front so a user with no connected Jira is refused BEFORE the claim lands
// (acting as the bot here would mis-assign the ticket to the service account).
// Returns the new status, the resolved Jira client (nil for GitHub tasks), and
// ok=false when it already wrote an error response.
func (s *Server) selfClaim(w http.ResponseWriter, r *http.Request, orgID, userID, id string, hesitationMs int) (string, *jira.Client, bool) {
	var task *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return "", nil, false
	}
	if task == nil {
		notFound(w, "task")
		return "", nil, false
	}
	// Terminal-status refusal: claim transitions on done/dismissed rows are
	// meaningless, and letting them fall through would trip RecordSwipe's
	// vestigial status='queued' write — reopening a closed task as a side
	// effect of the audit.
	if isTerminalTaskStatus(task.Status) {
		writeTaskTerminal(w, "claim")
		return "", nil, false
	}

	// A Jira-backed claim assigns the ticket to the claiming user and
	// transitions it — a write that must act as THAT user. Resolve the acting
	// user's credential up front; the RequireJiraIdentity gate guarantees
	// presence in the normal flow, so this 409 is defense-in-depth.
	var jiraUserClient *jira.Client
	if task.EntitySource == "jira" {
		c, jerr := s.jiraResolver.ForUser(r.Context(), orgID, userID)
		if errors.Is(jerr, jira.ErrNoJiraUserCredential) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason:  httpx.ReasonNotConfigured,
				Message: "connect your Jira to act on tickets",
			})
			return "", nil, false
		}
		if jerr != nil {
			internalError(w, "tasks", jerr)
			return "", nil, false
		}
		jiraUserClient = c
	}

	claimChanged := false
	switch {
	case task.ClaimedByUserID == userID:
		// Idempotent: same user already owns it.
	case task.ClaimedByUserID != "":
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "task is already claimed by another user",
		})
		return "", nil, false
	case task.ClaimedByAgentID != "":
		var claimOK bool
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			claimOK, e = tx.Tasks.TakeoverClaimFromAgent(r.Context(), orgID, id, userID)
			return e
		}); err != nil {
			taskActionLog.Error("takeover claim flip failed", "task", id, "error", err)
			httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
				Reason: httpx.ReasonInternal, Message: "claim stamp failed" + localDetail(err),
			})
			return "", nil, false
		}
		if !claimOK {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason: httpx.ReasonConflict, Message: "claim race lost; refetch task and retry",
			})
			return "", nil, false
		}
		claimChanged = true
	default:
		var claimOK bool
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			claimOK, e = tx.Tasks.ClaimQueuedForUser(r.Context(), orgID, id, userID)
			return e
		}); err != nil {
			taskActionLog.Error("user claim stamp failed", "task", id, "error", err)
			httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
				Reason: httpx.ReasonInternal, Message: "claim stamp failed" + localDetail(err),
			})
			return "", nil, false
		}
		if !claimOK {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason: httpx.ReasonConflict, Message: "claim race lost; refetch task and retry",
			})
			return "", nil, false
		}
		claimChanged = true
	}

	// Audit post-mutation, best-effort: the claim helpers already cleared
	// snooze_until and flipped status atomically, so RecordSwipe is a no-op on
	// lifecycle and the load-bearing effect is the swipe_events row. If the
	// insert fails, the claim still landed — log and continue rather than
	// 500-ing on a committed state change.
	var newStatus string
	swipeErr := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		newStatus, e = tx.Swipes.RecordSwipe(r.Context(), orgID, id, swipeActionClaim, hesitationMs, nil)
		return e
	})
	if swipeErr != nil {
		taskActionLog.Warn("audit write failed for claim, claim mutation already landed", "task", id, "error", swipeErr)
		newStatus = "queued"
	}
	if claimChanged {
		s.ws.Broadcast(websocket.Event{
			Type:  "task_claimed",
			OrgID: orgID,
			Data: map[string]any{
				"task_id":             id,
				"claimed_by_agent_id": "",
				"claimed_by_user_id":  userID,
			},
		})
	}
	return newStatus, jiraUserClient, true
}

// reassignClaim is the claim route's target_user_id arm (TFAC-561): the
// user↔user handoff. It's a race-safe CAS on the *expected* current claimant —
// ReassignClaimToUserSystem refuses unless the task is presently claimed by
// exactly that user AND the target is a member of a team associated with the
// task — so two concurrent handoffs (or one racing a takeover) can't clobber
// each other, and a claim can never land on someone who'd then be unable to
// even see the row (tasks_select RLS requires team membership on a claimed
// task).
//
// Refuses (in order): missing/closed task (404/409), a task that isn't
// currently held by a user — unclaimed or bot-claimed tasks aren't a handoff
// target; self-claim/takeover instead (409), the caller being neither the
// current claimant nor an admin of the task's owning team (403), a target on
// no team that can see the task (422), and a lost CAS (409). Handing off to
// the current claimant is treated as an idempotent no-op, matching the
// self-claim idempotency in selfClaim.
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
// Deliberately out of scope: no Jira-side ticket reassignment. A self-claim
// syncs a Jira ticket's assignee to the ACTING user; a handoff moves the claim
// to a THIRD party who may not have Jira connected at all, so syncing here
// would mean acting through someone else's (possibly absent) credential. The
// Jira ticket can drift from the TF claimant until the new claimant next
// interacts with it — an accepted divergence in the same family as the
// TF/GitHub drift docs/for-agents/multi-team-task-model.md already calls out,
// not a bug this method needs to close.
func (s *Server) reassignClaim(w http.ResponseWriter, r *http.Request, orgID, userID, id, targetUserID string, hesitationMs int) (string, bool) {
	var task *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return "", false
	}
	if task == nil {
		notFound(w, "task")
		return "", false
	}
	if isTerminalTaskStatus(task.Status) {
		writeTaskTerminal(w, "claim")
		return "", false
	}
	if task.ClaimedByUserID == "" {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "task isn't claimed by a user; claim it or take it over before handing it off",
		})
		return "", false
	}

	// Permission: the current claimant may hand off their own claim; anyone
	// else needs to admin the task's owning team (TFAC-561's suggested
	// "claimant + team admin" model). Scoped to task_id ONLY, not the wider
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
				return "", false
			}
			isAdmin, err := s.az.UserIsTeamAdmin(r.Context(), userID, orgID, *task.TeamID)
			if err != nil {
				internalError(w, "tasks", err)
				return "", false
			}
			if !isAdmin {
				forbidden(w, "only the current claimant or a team admin can reassign this task")
				return "", false
			}
		}
	}

	claimChanged := targetUserID != task.ClaimedByUserID
	if claimChanged {
		var ok bool
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			ok, e = tx.Tasks.ReassignClaimToUserSystem(r.Context(), orgID, id, task.ClaimedByUserID, targetUserID)
			return e
		}); err != nil {
			taskActionLog.Error("reassign claim flip failed", "task", id, "error", err)
			httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
				Reason: httpx.ReasonInternal, Message: "reassign failed" + localDetail(err),
			})
			return "", false
		}
		if !ok {
			// Disambiguate the two guards the CAS collapses into one
			// ok=false: a genuine race (someone else changed the
			// claim/status since our read above) vs. the target-team
			// membership guard tripping (which no retry will fix).
			// Re-reading the task tells them apart — if the claim + status
			// we read earlier are unchanged, nothing raced; the target just
			// isn't on a team that can see this task, which is a reference
			// to an object in the wrong team rather than a state conflict.
			var fresh *domain.Task
			_ = s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				var e error
				fresh, e = tx.Tasks.Get(r.Context(), orgID, id)
				return e
			})
			if fresh != nil && fresh.ClaimedByUserID == task.ClaimedByUserID && fresh.Status == task.Status {
				httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
					Reason:  httpx.ReasonCrossTeamRef,
					Message: "target user isn't on a team that can see this task",
					Field:   "target_user_id",
				})
			} else {
				httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
					Reason: httpx.ReasonConflict, Message: "reassign race lost; refetch task and retry",
				})
			}
			return "", false
		}
	}

	// Audit post-mutation, best-effort — mirrors selfClaim.
	var newStatus string
	swipeErr := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		newStatus, e = tx.Swipes.RecordSwipe(r.Context(), orgID, id, swipeActionReassign, hesitationMs, nil)
		return e
	})
	if swipeErr != nil {
		taskActionLog.Warn("audit write failed for reassign, claim mutation already landed", "task", id, "error", swipeErr)
		// Mirror ReassignClaimToUser(System)'s own status transformation
		// (SET status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE
		// status END) rather than assuming the pre-mutation snapshot still
		// matches: task.Status can't actually be "snoozed" here today
		// (SnoozeTask refuses to snooze an already-claimed row, so a
		// handoff candidate — always user-claimed — is never snoozed),
		// but that's an app-level invariant, not a DB constraint, and this
		// fallback shouldn't silently depend on it holding. Every other
		// status (queued/in_progress/in_review) passes through unchanged,
		// same as the CAS.
		newStatus = task.Status
		if newStatus == "snoozed" {
			newStatus = "queued"
		}
	}
	if claimChanged {
		s.ws.Broadcast(websocket.Event{
			Type:  "task_claimed",
			OrgID: orgID,
			Data: map[string]any{
				"task_id":             id,
				"claimed_by_agent_id": "",
				"claimed_by_user_id":  targetUserID,
			},
		})
	}
	return newStatus, true
}

// taskDelegateRequest is the body of POST /api/tasks/{id}/delegate.
type taskDelegateRequest struct {
	// BlueprintID names the blueprint the run executes. Absent lets the
	// spawner resolve the team's default.
	BlueprintID string `json:"blueprint_id,omitempty"`
	// HesitationMs is the dwell time before the gesture, recorded on the
	// swipe_events row this route writes.
	HesitationMs int `json:"hesitation_ms,omitempty"`
}

// handleTaskDelegate hands a task to the agent and spawns its run.
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

	var req taskDelegateRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	validateHesitation(&v, req.HesitationMs)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

	newStatus, ok := s.stampAgentClaim(w, r, orgID, userID, id, req)
	if !ok {
		return
	}
	// Re-delegating is still a handoff off whatever was in flight: resolve the
	// task's unresolved artifacts and cancel the running conversation before
	// the new run starts.
	s.teardownTaskConversations(r, orgID, userID, id, discardOutcomeRedelegated)

	response := map[string]any{"status": newStatus}
	if s.spawner != nil {
		if !s.triggerDelegation(w, r, orgID, userID, id, req, response) {
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// stampAgentClaim moves the task to bot ownership: HandoffAgentClaim handles
// unclaimed→bot, my-claim→bot and idempotent bot-owns, and refuses a
// different-user claim. It re-checks team_agents.enabled at request time,
// gating on the team the claim consolidates onto rather than the pre-handoff
// task.TeamID. Returns the new status and ok=false when it already wrote an
// error response.
func (s *Server) stampAgentClaim(w http.ResponseWriter, r *http.Request, orgID, userID, id string, req taskDelegateRequest) (string, bool) {
	// Pre-load to disambiguate HandoffRefused (404 missing / 409 terminal /
	// 409 theft) and to gate the bot-enablement check on the task's own team.
	var task *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return "", false
	}
	if task == nil {
		notFound(w, "task")
		return "", false
	}
	if isTerminalTaskStatus(task.Status) {
		writeTaskTerminal(w, "delegate")
		return "", false
	}

	// Gate on the team HandoffAgentClaim will consolidate onto, not the
	// pre-handoff task.TeamID: for an unclaimed task visible to several teams
	// the latter can be a team the caller isn't in, whose team_agents row RLS
	// hides — wrongly reporting the bot disabled and rejecting a legit delegate.
	var claimTeamID string
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		claimTeamID, e = tx.Tasks.ResolveClaimTeam(r.Context(), orgID, id, userID)
		return e
	}); err != nil {
		taskActionLog.Error("delegate aborted", "task", id, "error", err)
		httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
			Reason: httpx.ReasonInternal, Message: "delegate failed" + localDetail(err),
		})
		return "", false
	}
	a, enabled, err := s.agentEnabledForTeam(r.Context(), orgID, userID, claimTeamID)
	if err != nil {
		taskActionLog.Error("delegate aborted", "task", id, "error", err)
		httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
			Reason: httpx.ReasonInternal, Message: "delegate failed" + localDetail(err),
		})
		return "", false
	}
	if !enabled {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "bot is disabled for this team; enable it in team settings to delegate",
		})
		return "", false
	}
	var result db.HandoffResult
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		result, e = tx.Tasks.HandoffAgentClaim(r.Context(), orgID, id, a.ID, userID)
		return e
	}); err != nil {
		taskActionLog.Error("failed to stamp agent claim", "task", id, "error", err)
		httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
			Reason: httpx.ReasonInternal, Message: "claim stamp failed" + localDetail(err),
		})
		return "", false
	}
	if result == db.HandoffRefused {
		// Pre-load ruled out missing + terminal, so the only remaining refusal
		// is "different user owns the claim." The TOCTOU window is narrow but
		// real; the user retries from a fresh view either way.
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason: httpx.ReasonConflict, Message: "task is claimed by another user; refusing to steal",
		})
		return "", false
	}

	// Accepted (Changed or NoOp). Audit post-mutation, best-effort.
	var newStatus string
	swipeErr := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		newStatus, e = tx.Swipes.RecordSwipe(r.Context(), orgID, id, swipeActionDelegate, req.HesitationMs, nil)
		return e
	})
	if swipeErr != nil {
		taskActionLog.Warn("audit write failed for delegate, claim mutation already landed", "task", id, "error", swipeErr)
		newStatus = "queued"
	}
	if result == db.HandoffChanged {
		s.ws.Broadcast(websocket.Event{
			Type:  "task_claimed",
			OrgID: orgID,
			Data: map[string]any{
				"task_id":             id,
				"claimed_by_agent_id": a.ID,
				"claimed_by_user_id":  "",
			},
		})
	}
	return newStatus, true
}

// teardownTaskConversations stops any run a task is handed off from. It
// resolves every unresolved artifact the task holds (teardownTaskArtifacts —
// closes all draft PRs, dismisses all pending reviews, a no-op when none
// exist) and cancels in-flight runs. The discard memory note differs per
// outcome so the next agent reading conversation_memory can tell apart "human
// walked away" (dismiss) from "human resolved it" (complete) from "human took
// over" (claim) from "re-delegate". Best-effort.
func (s *Server) teardownTaskConversations(r *http.Request, orgID, userID, id string, outcome discardOutcome) {
	// Teardown runs detached from r.Context() so a client disconnect after the
	// response doesn't strand work: both the artifact teardown and the
	// active-run lookup + cancellation below must complete regardless.
	cleanupCtx := context.WithoutCancel(r.Context())
	s.teardownTaskArtifacts(cleanupCtx, orgID, userID, id, outcome)
	if s.spawner == nil {
		return
	}
	var ids []string
	if err := s.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
		var e error
		ids, e = tx.Conversations.ActiveIDsForTask(cleanupCtx, orgID, id)
		return e
	}); err != nil {
		taskActionLog.Error("active-conversation lookup failed", "task", id, "error", err)
		return
	}
	// The gesture is the task's own disposition, so the blueprints behind
	// these runs go terminal with them rather than freezing 'running' — the
	// plain conversation stop is for a user pausing work they mean to come
	// back to.
	for _, conversationID := range ids {
		if err := s.spawner.StopAndCancelBlueprint(orgID, conversationID, userID, delegate.StopCauseTaskDispositioned); err != nil {
			taskActionLog.Warn("stop conversation failed", "conversation", conversationID, "task", id, "error", err)
		}
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
	var rule *domain.JiraProjectStatusRules
	err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		rule = lookupJiraRuleForTask(r.Context(), tx, task)
		return nil
	})
	if err != nil || task == nil || task.EntitySource != "jira" {
		return
	}
	if rule == nil || rule.InProgressCanonical == "" {
		jiraLog.Warn("claim guard: no in_progress rule configured for project, skipping transition", "ticket", task.EntitySourceID)
		return
	}
	go func(issueKey string, ipMembers []string, ipCanonical string) {
		// Detached from the request: this claim guard outlives the response,
		// so it uses a background context.
		bgCtx := context.Background()
		state := jiraUserClient.GetClaimState(bgCtx, issueKey)

		needAssign := state == nil || !state.AssignedToSelf
		needTransition := state == nil || !slices.Contains(ipMembers, state.StatusName)

		if !needAssign && !needTransition {
			jiraLog.Info("claim guard: already assigned to self and in in-progress, skipping", "issue", issueKey, "status", state.StatusName)
			return
		}

		if needAssign {
			if err := jiraUserClient.AssignToSelf(bgCtx, issueKey); err != nil {
				jiraLog.Error("failed to assign", "issue", issueKey, "error", err)
				return
			}
		}
		if needTransition {
			if err := jiraUserClient.TransitionTo(bgCtx, issueKey, ipCanonical); err != nil {
				jiraLog.Error("failed to transition", "issue", issueKey, "status", ipCanonical, "error", err)
			}
		}
	}(task.EntitySourceID, rule.InProgressMembers, rule.InProgressCanonical)
}

// triggerDelegation fires the delegation run and records the conversation_id on
// the response map. The caller has already verified s.spawner != nil. Failures
// are real statuses, not a 200 with a delegate_error field: a bad blueprint
// reference is a 422, everything else a 500, and both carry reason
// SPAWN_FAILED because the claim stampAgentClaim stamped stays stamped — the
// user's gesture committed at the claim axis — so the client refetches on that
// reason and finds the task in the bot's lane without a run. Returns false once
// an error response is written.
func (s *Server) triggerDelegation(w http.ResponseWriter, r *http.Request, orgID, userID, id string, req taskDelegateRequest, response map[string]any) bool {
	var task *domain.Task
	err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	})
	if err != nil {
		// The claim landed but the post-dispatch re-read failed, so no run can
		// be spawned this attempt. Failing honestly beats the former silent
		// 200 with neither conversation_id nor delegate_error — and because
		// the claim survives, it's a SPAWN_FAILED, not a bare INTERNAL.
		writeDelegateSpawnInternal(w, err)
		return false
	}
	if task == nil {
		writeDelegateSpawnInternal(w, fmt.Errorf("task %s vanished between delegate claim and spawn", id))
		return false
	}
	// The actor is the agent this route just claimed the task with
	// (stampAgentClaim stamped claimed_by_agent_id before this re-read, and
	// Tasks.Get hydrates it). Pass it so the run's frozen blueprint_run actor
	// matches the task claim.
	blueprintRunID, err := s.spawner.Delegate(*task, delegate.DelegateOpts{
		OrgID:               orgID,
		ExplicitBlueprintID: req.BlueprintID,
		TriggerType:         "manual",
		CreatorUserID:       userID,
		ActorAgentID:        task.ClaimedByAgentID,
	})
	if err != nil {
		writeDelegateSpawnError(w, err)
		return false
	}
	// TODO(TFAC-840): the field says conversation_id but Delegate returns the
	// blueprint_run id, so following this into /api/agent/conversations/{id}
	// 404s. Kept as-is here because the field name is a wire contract.
	response["conversation_id"] = blueprintRunID
	return true
}

// writeDelegateSpawnError maps a spawner.Delegate failure to its status: a
// bad or missing blueprint reference is the caller's fault (422 — the body
// was well-formed, the object it names can't back a run), anything else is a
// spawn/DB fault (500, logged + redacted). Shared by the delegate route and
// the factory drag-to-delegate so the two gestures fail identically.
//
// Both arms carry reason SPAWN_FAILED: every caller reaches here only after
// the agent claim stamped, and the reason — not the status — is what tells
// a client the claim survived. The pre-claim failure paths in these handlers
// answer plain INTERNAL 500s, so without the distinct reason a client could
// not tell "bot-claimed, retry the run" from "nothing landed".
func writeDelegateSpawnError(w http.ResponseWriter, err error) {
	if errors.Is(err, delegate.ErrBlueprintNotFound) || errors.Is(err, delegate.ErrBlueprintUnspecified) ||
		errors.Is(err, delegate.ErrPromptNotFound) || errors.Is(err, delegate.ErrPromptUnspecified) {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonSpawnFailed,
			Message: err.Error(),
			Field:   "blueprint_id",
		})
		return
	}
	writeDelegateSpawnInternal(w, err)
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
