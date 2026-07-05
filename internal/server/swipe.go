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
// SKY-261 v0.7 audit contract: swipe_events is a "state-change log," not a
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
	// is set only by the claim path for Jira-backed tasks (SKY-463) and is
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
	// holds and cancels any in-flight run (SKY-206). Reassign (TFAC-561) is
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

	// SKY-463: a Jira-backed claim assigns the ticket to the claiming user and
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
// time (SKY-261), gating on the team the claim consolidates onto rather than
// the pre-handoff task.TeamID (SKY-378). Returns the new status and ok=false
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
// on the *expected* current claimant — ReassignClaimToUser refuses unless the
// task is presently claimed by exactly that user — so two concurrent
// reassigns (or a reassign racing a takeover) can't clobber each other.
//
// Refuses (in order): missing target_user_id (400), missing/closed task
// (404/409), a task that isn't currently held by a user — unclaimed or
// bot-claimed tasks aren't a reassign target; use claim/takeover instead
// (409), the caller being neither the current claimant nor a team admin of
// one of the task's teams (403), and a lost CAS race (409). Reassigning to
// the current claimant is treated as an idempotent no-op, matching the
// self-claim idempotency in swipeClaim.
//
// Deliberately out of scope: no Jira-side ticket reassignment. SKY-463 syncs
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
	// else needs the team-admin override (TFAC-561's suggested "claimant +
	// team admin" model).
	if task.ClaimedByUserID != userID {
		isAdmin, err := s.callerIsAdminOfTaskTeam(r.Context(), orgID, userID, task)
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

	claimChanged := targetUserID != task.ClaimedByUserID
	if claimChanged {
		var ok bool
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			ok, e = tx.Tasks.ReassignClaimToUser(r.Context(), orgID, id, task.ClaimedByUserID, targetUserID)
			return e
		}); err != nil {
			swipeLog.Error("reassign claim flip failed", "task", id, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "reassign failed" + localDetail(err)})
			return "", false
		}
		if !ok {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "reassign race lost; refetch task and retry",
			})
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
		newStatus = task.Status
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

// callerIsAdminOfTaskTeam reports whether userID holds the 'admin' role on
// any team the task belongs to — its owning team_id or any task_teams
// visibility row. This is the team-admin override arm of swipeReassign's
// permission model (the other arm, "you're the current claimant," is checked
// by the caller). Local mode short-circuits true — N=1's sole implicit owner
// admins its one team, mirroring authz.Checker's other local-mode gates.
func (s *Server) callerIsAdminOfTaskTeam(ctx context.Context, orgID, userID string, task *domain.Task) (bool, error) {
	if runmode.Current() == runmode.ModeLocal {
		return true, nil
	}
	teamIDs := map[string]struct{}{}
	if task.TeamID != nil && *task.TeamID != "" {
		teamIDs[*task.TeamID] = struct{}{}
	}
	var vis []string
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		vis, e = tx.Tasks.VisibilityTeams(ctx, orgID, task.ID)
		return e
	}); err != nil {
		return false, err
	}
	for _, t := range vis {
		teamIDs[t] = struct{}{}
	}
	for teamID := range teamIDs {
		isAdmin, err := s.az.UserIsTeamAdmin(ctx, userID, orgID, teamID)
		if err != nil {
			return false, err
		}
		if isAdmin {
			return true, nil
		}
	}
	return false, nil
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
// reading run_memory can tell apart "human walked away" (dismiss) from "human
// resolved it" (complete) from "human took over" (claim) from "re-delegate"
// (SKY-330). Best-effort.
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
		ids, e = tx.AgentRuns.ActiveIDsForTask(cleanupCtx, orgID, id)
		return e
	}); err != nil {
		swipeLog.Error("active-run lookup failed", "task", id, "error", err)
		return
	}
	for _, runID := range ids {
		if err := s.spawner.Cancel(orgID, runID, userID); err != nil {
			swipeLog.Warn("cancel run failed", "run", runID, "action", action, "task", id, "error", err)
		}
	}
}

// swipeJiraClaimSync assigns a claimed Jira ticket to the acting user and
// transitions it to in-progress, in a detached goroutine. The claim guard
// skips the transition when the ticket is already assigned-to-self and already
// in any in-progress member, so a second claim on the same issue doesn't
// re-assign/re-transition redundantly. jiraUserClient is the acting user's own
// client (SKY-463), so the assignment attributes to the claimer.
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
		response["run_id"] = runID
	}
}
