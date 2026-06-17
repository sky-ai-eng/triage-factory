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
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

type swipeRequest struct {
	Action       string `json:"action"`
	HesitationMs int    `json:"hesitation_ms"`
	BlueprintID  string `json:"blueprint_id,omitempty"`
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
	case "claim", "dismiss", "snooze", "delegate", "complete":
		// valid
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid action: must be claim, dismiss, snooze, delegate, or complete"})
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
	}

	// Any user gesture that takes a task off the agent's hands — dismiss,
	// complete, claim, or delegate — tears down a pending_approval review and
	// cancels any in-flight run (SKY-206).
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed: " + err.Error()})
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
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed: " + err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed: " + err.Error()})
		return "", false
	}
	a, enabled, err := s.agentEnabledForTeam(r.Context(), orgID, userID, claimTeamID)
	if err != nil {
		swipeLog.Error("delegate aborted", "task", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed: " + err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed: " + err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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

// swipeTeardownRuns stops any run a task is handed off from. It tears down a
// pending_approval review (cleanupPendingApprovalRun, idempotent and a no-op
// when no review exists) and cancels in-flight runs. The discard memory note
// differs per action so the next agent reading run_memory can tell apart
// "human walked away" (dismiss) from "human resolved it" (complete) from
// "human took over" (claim) from "re-delegate" (SKY-330). Best-effort.
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
	// swipe response doesn't strand the run: both the pending_approval cleanup
	// and the active-run lookup + cancellation below must complete regardless.
	cleanupCtx := context.WithoutCancel(r.Context())
	s.cleanupPendingApprovalRun(cleanupCtx, orgID, userID, id, outcome)
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
	runID, err := s.spawner.Delegate(*task, delegate.DelegateOpts{
		OrgID:               orgID,
		ExplicitBlueprintID: req.BlueprintID,
		TriggerType:         "manual",
		CreatorUserID:       userID,
	})
	if err != nil {
		response["delegate_error"] = err.Error()
	} else {
		response["run_id"] = runID
	}
}
