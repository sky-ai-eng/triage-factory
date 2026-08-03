package server

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

type swipeRequest struct {
	Action       string `json:"action"`
	HesitationMs int    `json:"hesitation_ms"`
	BlueprintID  string `json:"blueprint_id,omitempty"`
	// TargetUserID is the reassign action's handoff target (TFAC-561) —
	// the user the claim moves to. Unused by every other action.
	TargetUserID string `json:"target_user_id,omitempty"`
}

// handleSwipe applies a swipe gesture to a task. It validates the action,
// dispatches to the per-action handler, then runs the cross-cutting side
// effects an accepted swipe may need: tearing down a pending run, syncing a
// Jira claim, and firing a delegation.
//
// swipe_events is a "state-change log," not a
// "user-gesture log." For lifecycle actions (dismiss/snooze/complete) the
// swipe IS the state change, so the audit + lifecycle UPDATE land together.
// For responsibility-axis actions (claim/delegate) the real state change is a
// separate guarded UPDATE that can refuse the gesture; audit only lands after
// that UPDATE accepts, so a refused gesture leaves no trace. The per-action
// handlers own their own error responses and report completion via an ok bool.
func (s *Server) handleSwipe(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var req swipeRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}

	switch req.Action {
	case "claim", "dismiss", "snooze", "delegate", "complete", "reassign":
		// valid
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid action: must be claim, dismiss, snooze, delegate, complete, or reassign"})
		return
	}

	// Viewers can't act on a team's tasks (TFAC-447). Every swipe action is a
	// team-scoped write (claim/delegate stamp the responsibility axis;
	// dismiss/snooze/complete the lifecycle axis), so gate the whole family with
	// one clean 403 here rather than letting each per-action store UPDATE fall
	// off the tasks_update RLS policy as a confusing 409/500. A task the viewer
	// can't see at all still 404s downstream — RequireTaskWrite doesn't mask it.
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

	// newStatus is the task status reported back to the client. jiraUserClient
	// is set only by the claim path for Jira-backed tasks and is
	// consumed by the post-dispatch claim sync; nil otherwise.
	var (
		newStatus      string
		jiraUserClient *jira.Client
	)
	switch req.Action {
	case "claim":
		ns, jc, ok := s.swipeClaim(w, r, orgID, userID, id, req)
		if !ok {
			return
		}
		newStatus, jiraUserClient = ns, jc
	case "delegate":
		ns, ok := s.swipeDelegate(w, r, orgID, userID, id, req)
		if !ok {
			return
		}
		newStatus = ns
	case "dismiss", "snooze", "complete":
		ns, ok := s.swipeLifecycle(w, r, orgID, userID, id, req)
		if !ok {
			return
		}
		newStatus = ns
	case "reassign":
		ns, ok := s.swipeReassign(w, r, orgID, userID, id, req)
		if !ok {
			return
		}
		newStatus = ns
	}

	// Any user gesture that takes a task off the agent's hands — dismiss,
	// complete, claim, or delegate — resolves every unresolved artifact the task
	// holds and cancels any in-flight run. Reassign (TFAC-561) is
	// deliberately excluded: it's a user→user handoff on a task that's already
	// off the agent's hands, and run ownership is per-run, not per-claim — an
	// active delegated run (if the frozen actor is somehow still running
	// against a now-reassigned task) keeps executing untouched.
	if req.Action == "dismiss" || req.Action == "complete" || req.Action == "claim" || req.Action == "delegate" {
		s.swipeTeardownRuns(r, orgID, userID, id, req.Action)
	}

	response := map[string]any{"status": newStatus}

	if req.Action == "claim" && jiraUserClient != nil {
		s.swipeJiraClaimSync(r, orgID, userID, id, jiraUserClient)
	}

	if req.Action == "delegate" && s.spawner != nil {
		s.swipeTriggerDelegation(r, orgID, userID, id, req, response)
	}

	writeJSON(w, http.StatusOK, response)
}

// swipeClaim handles the claim action: a race-safe transition to user
// ownership with three accept paths (idempotent same-user, takeover from bot,
// claim from unclaimed) and one refuse path (different user owns it → 409). For
// a Jira-backed task it resolves the acting user's Jira client up front so a
// user with no connected Jira is refused BEFORE the claim lands (acting as the
// bot here would mis-assign the ticket to the service account). Returns the new
// status, the resolved Jira client (nil for GitHub tasks), and ok=false when it
// already wrote an error response.
func (s *Server) swipeClaim(w http.ResponseWriter, r *http.Request, orgID, userID, id string, req swipeRequest) (string, *jira.Client, bool) {
	var task *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "swipe", err)
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
	if task.Status == "done" || task.Status == "dismissed" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "task is closed; claim transitions aren't allowed past close",
		})
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
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "connect your Jira to act on tickets",
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
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "task is already claimed by another user",
		})
		return "", nil, false
	case task.ClaimedByAgentID != "":
		var claimOK bool
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			claimOK, e = tx.Tasks.TakeoverClaimFromAgent(r.Context(), orgID, id, userID)
			return e
		}); err != nil {
			swipeLog.Error("takeover claim flip failed", "task", id, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed" + localDetail(err)})
			return "", nil, false
		}
		if !claimOK {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "claim race lost; refetch task and retry",
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
			swipeLog.Error("user claim stamp failed", "task", id, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed" + localDetail(err)})
			return "", nil, false
		}
		if !claimOK {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "claim race lost; refetch task and retry",
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
		newStatus, e = tx.Swipes.RecordSwipe(r.Context(), orgID, id, req.Action, req.HesitationMs)
		return e
	})
	if swipeErr != nil {
		swipeLog.Warn("audit write failed for claim, claim mutation already landed", "task", id, "error", swipeErr)
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

// swipeDelegate handles the delegate action: HandoffAgentClaim transitions the
// task to bot ownership (unclaimed→bot, my-claim→bot, idempotent bot-owns) and
// refuses a different-user claim. It re-checks team_agents.enabled at swipe
// time, gating on the team the claim consolidates onto rather than
// the pre-handoff task.TeamID. Returns the new status and ok=false
// when it already wrote an error response.
func (s *Server) swipeDelegate(w http.ResponseWriter, r *http.Request, orgID, userID, id string, req swipeRequest) (string, bool) {
	// Pre-load to disambiguate HandoffRefused (404 missing / 409 terminal /
	// 409 theft) and to gate the bot-enablement check on the task's own team.
	var task *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "swipe", err)
		return "", false
	}
	if task == nil {
		notFound(w, "task")
		return "", false
	}
	if task.Status == "done" || task.Status == "dismissed" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "task is closed; delegate transitions aren't allowed past close",
		})
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
		swipeLog.Error("delegate aborted", "task", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed" + localDetail(err)})
		return "", false
	}
	a, enabled, err := s.agentEnabledForTeam(r.Context(), orgID, userID, claimTeamID)
	if err != nil {
		swipeLog.Error("delegate aborted", "task", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed" + localDetail(err)})
		return "", false
	}
	if !enabled {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "bot is disabled for this team; enable it in team settings to delegate",
		})
		return "", false
	}
	var result db.HandoffResult
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		result, e = tx.Tasks.HandoffAgentClaim(r.Context(), orgID, id, a.ID, userID)
		return e
	}); err != nil {
		swipeLog.Error("failed to stamp agent claim", "task", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed" + localDetail(err)})
		return "", false
	}
	if result == db.HandoffRefused {
		// Pre-load ruled out missing + terminal, so the only remaining refusal
		// is "different user owns the claim." The TOCTOU window is narrow but
		// real; the user retries from a fresh view either way.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "task is claimed by another user; refusing to steal",
		})
		return "", false
	}

	// Accepted (Changed or NoOp). Audit post-mutation, best-effort.
	var newStatus string
	swipeErr := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		newStatus, e = tx.Swipes.RecordSwipe(r.Context(), orgID, id, req.Action, req.HesitationMs)
		return e
	})
	if swipeErr != nil {
		swipeLog.Warn("audit write failed for delegate, claim mutation already landed", "task", id, "error", swipeErr)
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

// swipeReassign handles the reassign action (TFAC-561): the user↔user
// handoff arm the claim model didn't previously support. It's a race-safe CAS
// on the *expected* current claimant — ReassignClaimToUserSystem refuses
// unless the task is presently claimed by exactly that user AND the target is
// a member of a team associated with the task — so two concurrent reassigns
// (or a reassign racing a takeover) can't clobber each other, and a claim can
// never land on someone who'd then be unable to even see the row (tasks_select
// RLS requires team membership on a claimed task).
//
// Refuses (in order): missing target_user_id (400), missing/closed task
// (404/409), a task that isn't currently held by a user — unclaimed or
// bot-claimed tasks aren't a reassign target; use claim/takeover instead
// (409), the caller being neither the current claimant nor an admin of the
// task's owning team (403), and a lost CAS — either a genuine race or the
// target-team-membership guard (409). Reassigning to the current claimant is
// treated as an idempotent no-op, matching the self-claim idempotency in
// swipeClaim.
//
// Why the mutation runs on the admin pool (ReassignClaimToUserSystem, not
// ReassignClaimToUser): every other claim mutation ties "who's authorized to
// write" to "whose membership the resulting team_id is derived from" — the
// acting user IS the new claimant, so Postgres's tasks_update RLS naturally
// holds (they're necessarily a member of whatever team their own membership
// derived). Reassign is the first arm where the actor and the new claimant
// are different people: an admin override authorized against the task's
// CURRENT team can legitimately hand off to a user whose team is different
// (and who the admin may not share a team with at all) — team_id then
// consolidates to the target's team, which the acting admin's RLS session
// may not be able to write. Both sides of that authorization decision
// (actor permission below, target-team membership inside the CAS) are fully
// made in Go before this call, so bypassing the per-request RLS check for
// the write itself is safe — the same "authorize explicitly, then route
// around RLS" shape the `...System` convention uses elsewhere, just reached
// from a request path instead of a claims-less background goroutine.
//
// Deliberately out of scope: no Jira-side ticket reassignment. Claim syncs
// a Jira ticket's assignee to the ACTING user on self-claim; reassign moves
// the claim to a THIRD party who may not have Jira connected at all, so
// syncing here would mean acting through someone else's (possibly absent)
// credential. The Jira ticket can drift from the TF claimant until the new
// claimant next interacts with it — an accepted divergence in the same
// family as the TF/GitHub drift docs/for-agents/multi-team-task-model.md
// already calls out, not a bug this method needs to close.
func (s *Server) swipeReassign(w http.ResponseWriter, r *http.Request, orgID, userID, id string, req swipeRequest) (string, bool) {
	targetUserID := req.TargetUserID
	if targetUserID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target_user_id is required"})
		return "", false
	}

	var task *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "swipe", err)
		return "", false
	}
	if task == nil {
		notFound(w, "task")
		return "", false
	}
	if task.Status == "done" || task.Status == "dismissed" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "task is closed; reassign transitions aren't allowed past close",
		})
		return "", false
	}
	if task.ClaimedByUserID == "" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "task isn't claimed by a user; claim it or take it over instead of reassigning",
		})
		return "", false
	}

	// Permission: the current claimant may hand off their own claim; anyone
	// else needs to admin the task's owning team (TFAC-561's suggested
	// "claimant + team admin" model). Scoped to task_id ONLY, not the wider
	// task_teams visibility set: tasks_update RLS's task_teams branch only
	// ever applies to an UNCLAIMED row (both claim cols NULL) — a reassign
	// candidate is by definition already claimed, so team_id is the only
	// team that ever governs a write here, and it's the only one meaningful
	// to gate the override on. task.TeamID is guaranteed non-nil once
	// ClaimedByUserID is non-empty (tasks_claimed_requires_team CHECK).
	if task.ClaimedByUserID != userID {
		if runmode.Current() != runmode.ModeLocal {
			if task.TeamID == nil || *task.TeamID == "" {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "only the current claimant or a team admin can reassign this task",
				})
				return "", false
			}
			isAdmin, err := s.az.UserIsTeamAdmin(r.Context(), userID, orgID, *task.TeamID)
			if err != nil {
				internalError(w, "swipe", err)
				return "", false
			}
			if !isAdmin {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "only the current claimant or a team admin can reassign this task",
				})
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
			swipeLog.Error("reassign claim flip failed", "task", id, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "reassign failed" + localDetail(err)})
			return "", false
		}
		if !ok {
			// Disambiguate the two guards the CAS collapses into one ok=false:
			// a genuine race (someone else changed the claim/status since our
			// read above) vs. the target-team-membership guard tripping (which
			// no retry will fix). Re-reading the task tells them apart — if
			// the claim + status we read earlier are unchanged, nothing raced;
			// the target just isn't on a team that can see this task.
			var fresh *domain.Task
			_ = s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				var e error
				fresh, e = tx.Tasks.Get(r.Context(), orgID, id)
				return e
			})
			if fresh != nil && fresh.ClaimedByUserID == task.ClaimedByUserID && fresh.Status == task.Status {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "target user isn't on a team that can see this task",
				})
			} else {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "reassign race lost; refetch task and retry",
				})
			}
			return "", false
		}
	}

	// Audit post-mutation, best-effort — mirrors swipeClaim/swipeDelegate.
	var newStatus string
	swipeErr := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		newStatus, e = tx.Swipes.RecordSwipe(r.Context(), orgID, id, req.Action, req.HesitationMs)
		return e
	})
	if swipeErr != nil {
		swipeLog.Warn("audit write failed for reassign, claim mutation already landed", "task", id, "error", swipeErr)
		// Mirror ReassignClaimToUser(System)'s own status transformation
		// (SET status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE
		// status END) rather than assuming the pre-mutation snapshot still
		// matches: task.Status can't actually be "snoozed" here today
		// (SnoozeTask refuses to snooze an already-claimed row, so a
		// reassign candidate — always user-claimed — is never snoozed),
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

// swipeLifecycle handles dismiss/snooze/complete: the swipe IS the state
// change, so RecordSwipe lands the audit + UPDATE in one tx with no refuse
// path. (snooze here is defensive — the FE routes snoozing through
// /api/tasks/{id}/snooze.) Returns the new status and ok=false on a write
// error.
func (s *Server) swipeLifecycle(w http.ResponseWriter, r *http.Request, orgID, userID, id string, req swipeRequest) (string, bool) {
	var newStatus string
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		newStatus, e = tx.Swipes.RecordSwipe(r.Context(), orgID, id, req.Action, req.HesitationMs)
		return e
	}); err != nil {
		internalError(w, "swipe", err)
		return "", false
	}
	// Broadcast on the status axis so peer sessions refetch — without this a
	// dismissed/completed/snoozed task stays in its old lane on other browsers
	// until the next refresh.
	s.ws.Broadcast(websocket.Event{
		Type:  "task_updated",
		OrgID: orgID,
		Data:  map[string]any{"task_id": id, "status": newStatus},
	})
	return newStatus, true
}

// swipeTeardownRuns stops any run a task is handed off from. It resolves every
// unresolved artifact the task holds (teardownTaskArtifacts — closes all draft
// PRs, dismisses all pending reviews, a no-op when none exist) and cancels
// in-flight runs. The discard memory note differs per action so the next agent
// reading conversation_memory can tell apart "human walked away" (dismiss) from "human
// resolved it" (complete) from "human took over" (claim) from "re-delegate".
// Best-effort.
func (s *Server) swipeTeardownRuns(r *http.Request, orgID, userID, id, action string) {
	outcome := discardOutcomeDismissed
	switch action {
	case "complete":
		outcome = discardOutcomeCompleted
	case "claim":
		outcome = discardOutcomeClaimed
	case "delegate":
		outcome = discardOutcomeRedelegated
	}
	// Teardown runs detached from r.Context() so a client disconnect after the
	// swipe response doesn't strand work: both the artifact teardown and the
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
		swipeLog.Error("active-run lookup failed", "task", id, "error", err)
		return
	}
	// A swipe is the task's own disposition, so the blueprints behind these
	// runs go terminal with them rather than freezing 'running' — the plain
	// conversation stop is for a user pausing work they mean to come back to.
	for _, runID := range ids {
		if err := s.spawner.StopAndCancelBlueprint(orgID, runID, userID, delegate.StopCauseTaskDispositioned); err != nil {
			swipeLog.Warn("stop run failed", "run", runID, "action", action, "task", id, "error", err)
		}
	}
}

// swipeJiraClaimSync assigns a claimed Jira ticket to the acting user and
// transitions it to in-progress, in a detached goroutine. The claim guard
// skips the transition when the ticket is already assigned-to-self and already
// in any in-progress member, so a second claim on the same issue doesn't
// re-assign/re-transition redundantly. jiraUserClient is the acting user's own
// client, so the assignment attributes to the claimer.
func (s *Server) swipeJiraClaimSync(r *http.Request, orgID, userID, id string, jiraUserClient *jira.Client) {
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
		// Detached from the request: this claim guard outlives the swipe
		// response, so it uses a background context.
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

// swipeTriggerDelegation fires the delegation run for a delegate swipe and
// records the run_id (or delegate_error) on the response map. The caller has
// already verified s.spawner != nil.
func (s *Server) swipeTriggerDelegation(r *http.Request, orgID, userID, id string, req swipeRequest, response map[string]any) {
	var task *domain.Task
	err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	})
	if err != nil || task == nil {
		return
	}
	// The actor is the agent this swipe just claimed the task with (swipeDelegate
	// stamped claimed_by_agent_id before this re-read, and Tasks.Get hydrates it).
	// Pass it so the run's frozen blueprint_run actor matches the task claim.
	runID, err := s.spawner.Delegate(*task, delegate.DelegateOpts{
		OrgID:               orgID,
		ExplicitBlueprintID: req.BlueprintID,
		TriggerType:         "manual",
		CreatorUserID:       userID,
		ActorAgentID:        task.ClaimedByAgentID,
	})
	if err != nil {
		response["delegate_error"] = err.Error()
	} else {
		response["conversation_id"] = runID
	}
}
