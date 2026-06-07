package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// taskJSON is the API representation of a task. Maps entity-joined fields
// to the frontend's expected shape for backward compatibility.
type taskJSON struct {
	ID                  string   `json:"id"`
	EntityID            string   `json:"entity_id"`   // FK to entities.id — lets callers correlate tasks back to their entity
	Source              string   `json:"source"`      // from entity
	SourceID            string   `json:"source_id"`   // from entity
	SourceURL           string   `json:"source_url"`  // from entity
	Title               string   `json:"title"`       // from entity
	EntityKind          string   `json:"entity_kind"` // "pr" | "issue"
	EventType           string   `json:"event_type"`
	DedupKey            string   `json:"dedup_key,omitempty"`
	Severity            string   `json:"severity,omitempty"`
	RelevanceReason     string   `json:"relevance_reason,omitempty"`
	ScoringStatus       string   `json:"scoring_status"`
	CreatedAt           string   `json:"created_at"`
	Status              string   `json:"status"`
	PriorityScore       *float64 `json:"priority_score"`
	AutonomySuitability *float64 `json:"autonomy_suitability"`
	AISummary           string   `json:"ai_summary,omitempty"`
	PriorityReasoning   string   `json:"priority_reasoning,omitempty"`
	CloseReason         string   `json:"close_reason,omitempty"`
	// SnoozeUntil — populated when the task is in a snoozed state.
	// Under the SKY-261 v0.7 invariant ("snoozed ↔ unclaimed"),
	// this is only ever set on queue-lane tasks. Any claim-axis
	// transition wakes the task atomically (clears snooze_until +
	// flips status='snoozed' → 'queued'), so claimed cards on the
	// Board never carry a snooze. The Cards triage view renders
	// future-snoozed entries hidden via the QueuedTasks filter;
	// the Board's Queue lane could optionally render them at the
	// tail with a "wakes Mar 5" badge (deferred UI follow-up).
	SnoozeUntil string `json:"snooze_until,omitempty"`
	// OpenSubtaskCount lets the UI flag a task whose Jira entity has open
	// subtasks — the "consider decomposing" signal (SKY-173). Zero for
	// GitHub tasks and Jira tickets without subtasks.
	OpenSubtaskCount int `json:"open_subtask_count"`
	// Claim cols (SKY-330): exposed so the per-card assignee picker
	// can render the current assignee without a second round-trip.
	// Exactly one is set when claimed; both empty when unclaimed.
	// omitempty keeps the wire shape clean for the unclaimed-queue case.
	ClaimedByAgentID string `json:"claimed_by_agent_id,omitempty"`
	ClaimedByUserID  string `json:"claimed_by_user_id,omitempty"`
	// TeamID is the task's owning team. Exposed so the multi-team board
	// can color-code / tag rows by team. Always set; the
	// frontend only surfaces it when the viewer belongs to ≥2 teams.
	// TODO(SKY-379): board row color-coding consumes this.
	TeamID string `json:"team_id,omitempty"`
}

func taskToJSON(t domain.Task) taskJSON {
	snoozeUntil := ""
	if t.SnoozeUntil != nil {
		snoozeUntil = t.SnoozeUntil.Format(time.RFC3339)
	}
	return taskJSON{
		ID:                  t.ID,
		EntityID:            t.EntityID,
		Source:              t.EntitySource,
		SourceID:            t.EntitySourceID,
		SourceURL:           t.SourceURL,
		Title:               t.Title,
		EntityKind:          t.EntityKind,
		EventType:           t.EventType,
		DedupKey:            t.DedupKey,
		Severity:            t.Severity,
		RelevanceReason:     t.RelevanceReason,
		ScoringStatus:       t.ScoringStatus,
		CreatedAt:           t.CreatedAt.Format(time.RFC3339),
		Status:              t.Status,
		PriorityScore:       t.PriorityScore,
		AutonomySuitability: t.AutonomySuitability,
		AISummary:           t.AISummary,
		PriorityReasoning:   t.PriorityReasoning,
		CloseReason:         t.CloseReason,
		SnoozeUntil:         snoozeUntil,
		OpenSubtaskCount:    t.OpenSubtaskCount,
		ClaimedByAgentID:    t.ClaimedByAgentID,
		ClaimedByUserID:     t.ClaimedByUserID,
		TeamID:              teamIDString(t.TeamID),
	}
}

// teamIDString flattens a task's nullable owning team to "" when unresolved
// (team_id NULL) so the response keeps emitting a plain string; the
// json:"team_id,omitempty" tag then omits the field entirely for an unowned
// task rather than sending an empty-string team.
func teamIDString(teamID *string) string {
	if teamID == nil {
		return ""
	}
	return *teamID
}

// teamFilterParam extracts the per-page multi-team read filter from the
// request — the set of non-empty, deduped, *valid-UUID* ?team_id= values
// (a repeated query param). Returns nil when none are present, which the
// stores treat as "the union of the viewer's teams."
//
// Malformed values are dropped, not errored: these params are
// user-controlled and also come from possibly-stale/corrupt localStorage
// (the device-local read filter), so a junk id should degrade to "no
// narrow for that value" rather than 500 the queue/board/factory when the
// raw string hits a Postgres ::uuid cast. A team the caller isn't in is
// already filtered out downstream by the RLS-mirroring predicate, so the
// only validation needed here is well-formedness.
func teamFilterParam(r *http.Request) []string {
	raw := r.URL.Query()["team_id"]
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if v == "" {
			continue
		}
		if _, err := uuid.Parse(v); err != nil {
			continue // drop malformed ids (stale localStorage, hand-edited URL)
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// singleTeamParam reads a single ?team_id= value for the single-team read
// surfaces (the prompts page narrowed to one team). Like teamFilterParam
// it drops a malformed id rather than erroring — the value is device-local
// view state (possibly stale localStorage) and junk should degrade to
// "unfiltered", not 500 the page. Returns "" when absent or malformed; the
// stores treat "" as no team narrow.
func singleTeamParam(r *http.Request) string {
	v := r.URL.Query().Get("team_id")
	if v == "" {
		return ""
	}
	if _, err := uuid.Parse(v); err != nil {
		return "" // drop malformed (stale localStorage, hand-edited URL)
	}
	return v
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	// SKY-330: ?include_snoozed=true keeps future-snoozed rows in the
	// response so the Board's "show snoozed" toggle can render them
	// at the tail of the Queued column. Default = false so /api/queue
	// stays the canonical "pickable right now" projection for the
	// Cards triage view.
	includeSnoozed := r.URL.Query().Get("include_snoozed") == "true"
	// Optional per-page team filter — a multi-team view scope, supplied as
	// repeated ?team_id= params. Empty = the union of the viewer's teams
	// (the RLS-scoped default); a non-empty set narrows to those teams'
	// owned/visible tasks. Teams the caller isn't in yield nothing under
	// RLS rather than leaking, so no extra validation is needed here — the
	// narrow is additive on top of the visibility scope.
	teamFilter := teamFilterParam(r)
	var tasks []domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		if includeSnoozed {
			tasks, e = tx.Tasks.QueuedIncludingSnoozed(r.Context(), orgID, teamFilter)
		} else {
			tasks, e = tx.Tasks.Queued(r.Context(), orgID, teamFilter)
		}
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	result := make([]taskJSON, len(tasks))
	for i, t := range tasks {
		result[i] = taskToJSON(t)
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	status := r.URL.Query().Get("status")
	// Optional per-page team filter — see handleQueue.
	teamFilter := teamFilterParam(r)
	var tasks []domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		if status != "" {
			tasks, e = tx.Tasks.ByStatus(r.Context(), orgID, status, teamFilter)
		} else {
			tasks, e = tx.Tasks.Queued(r.Context(), orgID, teamFilter)
		}
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	result := make([]taskJSON, len(tasks))
	for i, t := range tasks {
		result[i] = taskToJSON(t)
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	var task *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if task == nil {
		notFound(w, "task")
		return
	}
	writeJSON(w, http.StatusOK, taskToJSON(*task))
}

type swipeRequest struct {
	Action       string `json:"action"`
	HesitationMs int    `json:"hesitation_ms"`
	BlueprintID  string `json:"blueprint_id,omitempty"`
}

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

	// SKY-261 v0.7 audit contract: swipe_events = "state-change log,"
	// not "user-gesture log." For lifecycle actions (dismiss/snooze/
	// complete) the swipe IS the state change, so RecordSwipe at the
	// top is correct — audit + lifecycle UPDATE land in one tx. For
	// responsibility-axis actions (claim/delegate) the actual state
	// change lives in a separate guarded UPDATE that can refuse the
	// gesture (different user owns it, race lost); audit must only
	// land after that UPDATE accepts. A refused gesture leaves no
	// trace — no swipe_events row, no status flip, no snooze clear.
	//
	// The audit insert is best-effort once the claim mutation has
	// committed: if RecordSwipe fails after a successful claim flip,
	// the state change stands and we log the audit miss. Better an
	// audit gap than a refused-into-mutated state where the user gets
	// 500 on a state change that already happened.
	var newStatus string

	// jiraUserClient is resolved in the claim case for Jira-backed tasks
	// (SKY-463) and consumed by the post-switch claim-sync block. nil for
	// GitHub tasks and non-claim actions.
	var jiraUserClient *jira.Client

	switch req.Action {
	case "claim":
		// Race-safe claim: branch on the task's current claim state
		// and use the guarded helpers. Three accept paths
		// (idempotent same-user, takeover from bot, claim from
		// unclaimed) and one refuse path (different user owns it).
		// Refused → 409 with NO state mutation and NO audit row.
		var task *domain.Task
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			task, e = tx.Tasks.Get(r.Context(), orgID, id)
			return e
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if task == nil {
			notFound(w, "task")
			return
		}
		// Terminal-status refusal: claim transitions on done/
		// dismissed rows are meaningless (sticky claim past close
		// is audit-only) AND letting them fall through would tip
		// RecordSwipe's vestigial status='queued' write — reopening
		// a closed task as a side effect of the audit. Refuse here
		// so neither the helper paths nor the same-user idempotent
		// fall-through reach the lifecycle write.
		if task.Status == "done" || task.Status == "dismissed" {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "task is closed; claim transitions aren't allowed past close",
			})
			return
		}
		// SKY-463: a Jira-backed claim assigns the ticket to the claiming
		// user and transitions it — a write that must act as THAT user, not
		// the org service account. Resolve the acting user's Jira credential
		// up front so a user with no connected Jira is refused BEFORE the
		// claim lands; acting as the bot here would mis-assign the ticket to
		// the service account. The RequireJiraIdentity gate guarantees
		// presence in the normal flow, so this 409 is the defense-in-depth
		// boundary. The resolved client is consumed by the post-switch sync.
		if task.EntitySource == "jira" {
			c, jerr := s.jiraResolver.ForUser(r.Context(), orgID, userID)
			if errors.Is(jerr, jira.ErrNoJiraUserCredential) {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "connect your Jira to act on tickets",
				})
				return
			}
			if jerr != nil {
				internalError(w, "tasks", jerr)
				return
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
			return
		case task.ClaimedByAgentID != "":
			var claimOK bool
			if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				var e error
				claimOK, e = tx.Tasks.TakeoverClaimFromAgent(r.Context(), orgID, id, userID)
				return e
			}); err != nil {
				log.Printf("[swipe] takeover claim flip failed on task %s: %v", id, err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed: " + err.Error()})
				return
			}
			if !claimOK {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "claim race lost; refetch task and retry",
				})
				return
			}
			claimChanged = true
		default:
			var claimOK bool
			if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				var e error
				claimOK, e = tx.Tasks.ClaimQueuedForUser(r.Context(), orgID, id, userID)
				return e
			}); err != nil {
				log.Printf("[swipe] user claim stamp failed on task %s: %v", id, err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed: " + err.Error()})
				return
			}
			if !claimOK {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "claim race lost; refetch task and retry",
				})
				return
			}
			claimChanged = true
		}
		// Audit post-mutation. Under the v0.7 invariant the claim
		// helpers (TakeoverClaimFromAgent / ClaimQueuedTaskForUser)
		// already cleared snooze_until and flipped status →
		// 'queued' atomically if the pre-state was snoozed, so
		// RecordSwipe's UPDATE is a no-op on lifecycle and the
		// load-bearing effect at this point is the swipe_events
		// row. Best-effort: if the insert fails, the claim still
		// landed; log and continue rather than 500-ing on a
		// committed state change.
		var ns string
		swipeErr := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			ns, e = tx.Swipes.RecordSwipe(r.Context(), orgID, id, req.Action, req.HesitationMs)
			return e
		})
		if swipeErr != nil {
			log.Printf("[swipe] audit write failed for task %s claim: %v (claim mutation already landed)", id, swipeErr)
			newStatus = "queued"
		} else {
			newStatus = ns
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
	case "delegate":
		// HandoffAgentClaim covers three legitimate user→bot
		// transitions (unclaimed → bot; user-claimed-by-me → bot
		// transfer; bot-already-owns idempotent no-op) and three
		// refusal modes (missing task, terminal task, different-user
		// claim). The helper collapses all three refusals into
		// HandoffRefused, so we pre-load the task to disambiguate
		// for the response — 404 for missing, 409 + closed-task
		// message for terminal, 409 + theft message for different
		// user. Matches the claim path's load-and-branch shape.
		//
		// Loaded BEFORE the bot-enablement check so that check gates on
		// the task's own team (task.TeamID), not the org default — a
		// multi-team user delegating a team-B task is governed by B's
		// bot setting (SKY-378).
		var task *domain.Task
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			task, e = tx.Tasks.Get(r.Context(), orgID, id)
			return e
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if task == nil {
			notFound(w, "task")
			return
		}
		if task.Status == "done" || task.Status == "dismissed" {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "task is closed; delegate transitions aren't allowed past close",
			})
			return
		}
		// SKY-261 acceptance: swipe-delegate re-checks
		// team_agents.enabled at swipe time. A team admin can
		// toggle the bot off; trigger-spawned tasks landed
		// unclaimed (router's auto-fire skipped), and the user
		// can't manually delegate to a disabled bot either.
		// Refuse with 409 — clear error the FE can surface as
		// "bot is off; enable it in team settings."
		//
		// Gate on the team HandoffAgentClaim will consolidate onto, not
		// the pre-handoff task.TeamID: for an unclaimed task visible to
		// several teams the latter can be a team the caller isn't in,
		// whose team_agents row RLS hides — wrongly reporting the bot
		// disabled and rejecting a legitimate delegate.
		var claimTeamID string
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			claimTeamID, e = tx.Tasks.ResolveClaimTeam(r.Context(), orgID, id, userID)
			return e
		}); err != nil {
			log.Printf("[swipe] delegate aborted on task %s: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed: " + err.Error()})
			return
		}
		a, enabled, err := s.agentEnabledForTeam(r.Context(), orgID, userID, claimTeamID)
		if err != nil {
			log.Printf("[swipe] delegate aborted on task %s: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed: " + err.Error()})
			return
		}
		if !enabled {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "bot is disabled for this team; enable it in team settings to delegate",
			})
			return
		}
		var result db.HandoffResult
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			result, e = tx.Tasks.HandoffAgentClaim(r.Context(), orgID, id, a.ID, userID)
			return e
		}); err != nil {
			log.Printf("[swipe] failed to stamp agent claim on task %s: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed: " + err.Error()})
			return
		}
		if result == db.HandoffRefused {
			// Pre-load above ruled out missing + terminal, so the
			// only remaining HandoffRefused path is "different user
			// owns the claim." The TOCTOU window between GetTask and
			// HandoffAgentClaim is narrow but real — if the task
			// transitioned to terminal during it, the error message
			// is slightly wrong but the user retries from a fresh
			// view either way.
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "task is claimed by another user; refusing to steal",
			})
			return
		}
		// Accepted path (Changed or NoOp). Audit post-mutation,
		// best-effort.
		var ns string
		swipeErr := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			ns, e = tx.Swipes.RecordSwipe(r.Context(), orgID, id, req.Action, req.HesitationMs)
			return e
		})
		if swipeErr != nil {
			log.Printf("[swipe] audit write failed for task %s delegate: %v (claim mutation already landed)", id, swipeErr)
			newStatus = "queued"
		} else {
			newStatus = ns
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
	case "dismiss", "snooze", "complete":
		// Lifecycle actions: the swipe IS the state change. Audit
		// + UPDATE in one tx via RecordSwipe. No refuse path.
		// ('snooze' here is defensive — the FE routes snoozing
		// through /api/tasks/{id}/snooze; if it ever lands here
		// the status would flip to 'snoozed' without snooze_until,
		// which the FE doesn't produce and we don't bother
		// guarding against beyond defaulting newStatus.)
		var ns string
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			ns, e = tx.Swipes.RecordSwipe(r.Context(), orgID, id, req.Action, req.HesitationMs)
			return e
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		newStatus = ns
		// Lifecycle changed — broadcast on the status axis so peer
		// sessions refetch. Post-SKY-261 the Board listens for
		// task_updated separately from task_claimed; without this
		// broadcast a dismissed/completed/snoozed task would stay
		// in its old lane on other browsers until the next refresh.
		s.ws.Broadcast(websocket.Event{
			Type:  "task_updated",
			OrgID: orgID,
			Data:  map[string]any{"task_id": id, "status": newStatus},
		})
	}

	// Dismiss is a terminal state — if the user swipes away a task mid-run
	// (rare, but possible on a delegated card via the Board gesture rather
	// than the AgentCard cancel button), the run must stop. Mirrors the
	// inline-close and entity-close cascades: task state is authoritative;
	// runs follow.
	//
	// Two complementary paths here:
	//   - ActiveRunIDsForTask + spawner.Cancel covers in-flight runs
	//     (running, agent_starting, etc.) that still have a goroutine in
	//     s.cancels.
	//   - cleanupPendingApprovalRun covers pending_approval runs, which
	//     ActiveRunIDsForTask deliberately excludes (the agent process
	//     has exited, there's nothing to cancel — but the DB cleanup
	//     is still needed). SKY-206 closed the gap that left
	//     pending_reviews + a phantom run on a dismissed task.
	// Any user gesture that takes a task off the agent's hands —
	// dismiss, complete, or claim — must tear down a pending_approval
	// review if one exists and cancel any in-flight run. Otherwise a
	// race in the frontend (agentRuns map briefly stale during a
	// fetchTasks refresh) lets the user issue /swipe claim against a
	// pending_approval card without going through /requeue first,
	// stranding the prepared review row and the phantom
	// pending_approval run that SKY-206 closed.
	//
	// Backend-authoritative is the right shape here: the swipe
	// handler already loaded the task; cleanupPendingApprovalRun is
	// idempotent (filters on status='pending_approval') and a no-op
	// when no review exists. The discard memory note differs per
	// action so the next agent reading run_memory can tell apart
	// "human walked away from this entity" (dismiss) from "human
	// resolved it themselves" (complete) from "human took over and
	// will handle it manually" (claim) — three distinct
	// recalibration signals.
	if req.Action == "dismiss" || req.Action == "complete" || req.Action == "claim" || req.Action == "delegate" {
		outcome := discardOutcomeDismissed
		switch req.Action {
		case "complete":
			outcome = discardOutcomeCompleted
		case "claim":
			outcome = discardOutcomeClaimed
		case "delegate":
			// SKY-330: re-delegate cancels the prior in-flight run
			// before spawning the new one. Pre-330 the prior run was
			// allowed to keep running in parallel — the guards in
			// processCompletion + advanceTaskFromRunStatus prevent
			// its stale terminal events from corrupting state, but
			// the parallel work itself was wasteful (two cloned
			// worktrees, two API budgets). The assignee picker makes
			// re-delegate a routine gesture (clicking the bot row on
			// an already-bot-claimed task), so cancel-first is now
			// load-bearing for cost control rather than an edge case.
			outcome = discardOutcomeRedelegated
		}
		// Cleanup runs detached from r.Context() so a client
		// disconnect after the swipe response doesn't strand the
		// run in pending_approval. WithoutCancel inherits request
		// values (claims, eventually).
		cleanupCtx := context.WithoutCancel(r.Context())
		s.cleanupPendingApprovalRun(cleanupCtx, orgID, userID, id, outcome)
		if s.spawner != nil {
			var ids []string
			if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				var e error
				ids, e = tx.AgentRuns.ActiveIDsForTask(r.Context(), orgID, id)
				return e
			}); err != nil {
				log.Printf("[swipe] active-run lookup for task %s failed: %v", id, err)
			} else {
				for _, runID := range ids {
					if err := s.spawner.Cancel(orgID, runID, userID); err != nil {
						log.Printf("[swipe] cancel run %s on %s of task %s: %v", runID, req.Action, id, err)
					}
				}
			}
		}
	}

	response := map[string]any{"status": newStatus}

	// On claim: if Jira task, assign to self and transition to in-progress.
	// Claim guard: with multiple tasks per entity, a second claim on the same
	// Jira issue would re-assign + re-transition redundantly (and probably
	// error). Skip the transition when the ticket is already in ANY member of
	// the in-progress rule — if the user (or an earlier claim) moved it to
	// "In Review" while canonical is "In Progress", transitioning back to the
	// canonical would be a spurious status change that would confuse watchers.
	// SKY-463: jiraUserClient (resolved in the claim case above) is the acting
	// user's own Jira client, so AssignToSelf assigns the ticket to the
	// claimer — not the org service account — and GetClaimState's
	// AssignedToSelf check is relative to the user. nil for non-Jira tasks.
	if req.Action == "claim" && jiraUserClient != nil {
		// Fetch the task and (if Jira-backed) its team's status rules
		// inside a single WithTx so the rule read goes through the
		// app-pool ListForTeam — jira_rules_select RLS gates by team
		// membership, matching the user's claim path. Per-team rule
		// lookup on the swipe hot path is O(projects) and paced by
		// human swipe rate; sub-millisecond cost in practice.
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
		if err == nil && task != nil && task.EntitySource == "jira" {
			if rule != nil && rule.InProgressCanonical != "" {
				go func(issueKey string, ipMembers []string, ipCanonical string) {
					// Detached from the request: this claim guard outlives the
					// swipe response, so it uses a background context rather than
					// r.Context(), which is cancelled once the handler returns.
					bgCtx := context.Background()
					state := jiraUserClient.GetClaimState(bgCtx, issueKey)

					needAssign := state == nil || !state.AssignedToSelf
					needTransition := state == nil || !slices.Contains(ipMembers, state.StatusName)

					if !needAssign && !needTransition {
						log.Printf("[jira] claim guard: %s already assigned to self and already in in-progress (%q), skipping", issueKey, state.StatusName)
						return
					}

					if needAssign {
						if err := jiraUserClient.AssignToSelf(bgCtx, issueKey); err != nil {
							log.Printf("[jira] failed to assign %s: %v", issueKey, err)
							return
						}
					}
					if needTransition {
						if err := jiraUserClient.TransitionTo(bgCtx, issueKey, ipCanonical); err != nil {
							log.Printf("[jira] failed to transition %s to %q: %v", issueKey, ipCanonical, err)
						}
					}
				}(task.EntitySourceID, rule.InProgressMembers, rule.InProgressCanonical)
			} else {
				log.Printf("[jira] claim guard: no in_progress rule configured for project of %s, skipping transition", task.EntitySourceID)
			}
		}
	}

	// Trigger delegation on swipe-up
	if req.Action == "delegate" && s.spawner != nil {
		var task *domain.Task
		err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			task, e = tx.Tasks.Get(r.Context(), orgID, id)
			return e
		})
		if err == nil && task != nil {
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
	}

	writeJSON(w, http.StatusOK, response)
}

type snoozeRequest struct {
	Until        string `json:"until"`
	HesitationMs int    `json:"hesitation_ms"`
}

func (s *Server) handleSnooze(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var req snoozeRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}

	until, err := parseSnoozeUntil(req.Until)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid snooze duration: " + err.Error()})
		return
	}

	// Pre-load for 404 parity with /undo and /requeue. Without this,
	// SnoozeTask's swipe_events INSERT would trip the tasks(id) FK
	// for a missing task and surface a SQLite error string as 500 —
	// leaking implementation detail and confusing legitimate 404
	// callers. The pre-check fails fast before the store gets
	// involved.
	var task *domain.Task
	var snoozed bool
	// errSnoozeRefusedSentinel is used to roll the outer tx back when
	// SnoozeTask returns (false, nil) — the swipes store relies on a
	// tx-level rollback to discard the audit row it inserted before
	// the claim-guard UPDATE refused, and a flat (no-error) return
	// here would commit that audit row.
	errSnoozeRefusedSentinel := errors.New("snooze refused")
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if task == nil {
			return nil
		}
		snoozed, e = tx.Swipes.SnoozeTask(r.Context(), orgID, id, until, req.HesitationMs)
		if e != nil {
			return e
		}
		if !snoozed {
			return errSnoozeRefusedSentinel
		}
		return nil
	}); err != nil && !errors.Is(err, errSnoozeRefusedSentinel) {
		internalError(w, "tasks", err)
		return
	}
	if task == nil {
		notFound(w, "task")
		return
	}
	if !snoozed {
		// SKY-261 v0.7: snooze is queue-only ("snoozed ↔ both claim
		// cols NULL"). The store's atomic UPDATE refused because
		// the task is currently claimed by a user or the bot.
		// Requeue first (releases the claim) then snooze.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "can't snooze a claimed task; requeue or complete it first",
		})
		return
	}

	// Lifecycle changed (status='snoozed' now). Broadcast so other
	// connected clients refetch and re-render — without this the
	// Board on a peer session keeps showing the task in its old
	// lane until the next user-driven refresh.
	s.ws.Broadcast(websocket.Event{
		Type:  "task_updated",
		OrgID: orgID,
		Data:  map[string]any{"task_id": id, "status": "snoozed"},
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "snoozed", "until": until.Format(time.RFC3339)})
}

// handleUndo backs the Cards swipe-toast UX: the user just swiped
// claim/dismiss/delegate/snooze, sees the 5s "Undo" toast (or hits
// Cmd-Z), and we reverse the swipe. This endpoint is specifically
// for undoing a discrete user gesture — it records a swipe_events
// row tagged 'undo' for the swipe analytics, then runs the same
// requeue cleanup that /requeue does.
//
// State-driven requeue (Board's drag-to-Queue, SKY-207's "Return
// to queue" button) lives at /requeue and skips the swipe row.
// Same finalizer, same observable outcome — different audit shape.
func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	// GetTask up front does double duty: existence check for the
	// 404 response AND loads the row needed for finalizeRequeue's
	// Jira reversal context. Without the explicit nil check
	// UndoLastSwipe would still fail on the swipe_events FK, but
	// we'd surface the SQLite error string as a 500 — leaking
	// implementation detail and confusing legitimate 404 callers.
	var task *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if task == nil {
			return nil
		}
		return tx.Swipes.UndoLastSwipe(r.Context(), orgID, id)
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if task == nil {
		notFound(w, "task")
		return
	}

	s.finalizeRequeue(r, orgID, userID, id, task)

	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

// handleRequeue is the state-driven counterpart to handleUndo: same
// task-back-to-queue outcome, no swipe_events row. Used by Board's
// drag-to-Queue gesture and (once SKY-207 lands) the AgentCard's
// "Return to queue" button on pending_approval runs. Both of those
// are deliberate state changes, not "reverse my last swipe," so
// audit-logging them as undo events would muddy the swipe-UX
// analytics.
//
// Belt-and-suspenders existence check: GetTask up front catches the
// common bogus-id case and returns 404 with a clean error body;
// RequeueTask's ok-bool catches the race where the task gets
// deleted between the GetTask and the UPDATE. Without the second
// check, that race would surface as a misleading 200/queued
// response for an id that no longer exists.
func (s *Server) handleRequeue(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var task *domain.Task
	var requeued bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if task == nil {
			return nil
		}
		requeued, e = tx.Swipes.RequeueTask(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if task == nil {
		notFound(w, "task")
		return
	}
	if !requeued {
		notFound(w, "task")
		return
	}

	s.finalizeRequeue(r, orgID, userID, id, task)

	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

// handleTaskAdvance is the SKY-330 board's manual user transition —
// "I'm working on this now" (in_progress) and "I've submitted this for
// review" (in_review). Refuses if the caller doesn't hold the user
// claim, if the task is bot-claimed (those transition automatically
// via the spawner), or if the requested status is anything other
// than in_progress / in_review. Done / dismissed go through swipe;
// requeue goes through /requeue.
//
// Body: {"to": "in_progress" | "in_review"}
func (s *Server) handleTaskAdvance(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var body struct {
		To string `json:"to"`
	}
	if !decodeJSON(w, r, &body, "invalid request body") {
		return
	}
	if body.To != "in_progress" && body.To != "in_review" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to must be one of: in_progress, in_review"})
		return
	}

	// SKY-330: validate the path id as a UUID up front. On Postgres
	// tasks.id is the uuid column type, so a malformed id surfaces as
	// SQLSTATE 22P02 from the store call → 500. Treating malformed
	// ids as "task not found" keeps the API portable across SQLite
	// (id TEXT, no parse error) and Postgres.
	if _, err := uuid.Parse(id); err != nil {
		notFound(w, "task")
		return
	}

	// Pre-load mirrors the /requeue + /undo shape: gives us a clean
	// 404 for genuinely missing rows. 409 on the store's ok=false
	// means "guard tripped" (task exists but isn't claimed by you,
	// or is terminal) — distinct from "task not found" rather than
	// merged like the pre-fix shape.
	var task *domain.Task
	var advanced bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if task == nil {
			return nil
		}
		advanced, e = tx.Tasks.AdvanceStatusForUser(r.Context(), orgID, id, userID, body.To)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if task == nil {
		notFound(w, "task")
		return
	}
	if !advanced {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "task not advanceable — must be claimed by you and currently in queued/in_progress/in_review",
		})
		return
	}

	// Broadcast on the status axis so peer Board sessions move the
	// card to the new column without polling.
	if s.ws != nil {
		s.ws.Broadcast(websocket.Event{
			Type:  "task_updated",
			OrgID: orgID,
			Data:  map[string]any{"task_id": id, "status": body.To},
		})
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": body.To})
}

// discardOutcome describes how the task ended up after the user
// rejected the agent's prepared review. The DB cleanup path is the
// same across all four values, but the human_content note baked
// into run_memory differs — the next agent reading prior memory
// needs to know whether the human:
//
//   - re-queued the task (still on the docket; verdict was wrong),
//   - dismissed it outright (the entity isn't worth pursuing),
//   - marked it complete (the entity was resolved, but not via the
//     agent's prepared verdict),
//   - or claimed it themselves (the human took over and will handle
//     the entity manually rather than re-attempting agent work).
//
// The distinction is the load-bearing signal in post-run memory:
// each shape implies a different recalibration for future runs.
type discardOutcome int

const (
	discardOutcomeRequeued discardOutcome = iota
	discardOutcomeDismissed
	// discardOutcomeCompleted: user marked the task done from a
	// terminal-state AgentCard (failed, cancelled, taken_over) by
	// dragging it to the Done column. The agent's prepared review,
	// if any, is being discarded — the user is signalling "the work
	// is finished" without applying the agent's verdict to GitHub.
	discardOutcomeCompleted
	// discardOutcomeClaimed: user claimed the task while it had a
	// pending_approval run (Board's drag-to-You from Agent/Done, or
	// the Cards swipe-right against a delegated task). The agent's
	// prepared review is being thrown away in favor of the human
	// handling the entity themselves. This case exists primarily
	// to close the SKY-206 race where a stale frontend agentRuns
	// map could let /swipe claim slip past without /requeue's
	// cleanup; the swipe handler now runs the cleanup on every
	// claim regardless of frontend state.
	discardOutcomeClaimed
	// discardOutcomeRedelegated: user re-delegated the task while
	// the prior run was still in flight (or had landed a pending
	// review). The bot is still on the task — the prior run's
	// artifacts are thrown away in favor of a fresh run with
	// (typically) different instructions. Distinct from
	// Requeued/Dismissed/Completed/Claimed: the agent still owns
	// the task, but the verdict it just produced is no longer the
	// right answer. Future agents reading prior memory should
	// reconsider the framing rather than the conclusion.
	discardOutcomeRedelegated
)

// finalizeRequeue runs the side-effect cleanup that both /undo and
// /requeue need after the task status flips back to queued:
//
//   - pending_approval cleanup: if the task had a delegated agent run
//     in pending_approval (review prepared, awaiting human submit),
//     write the discard verdict to run_memory.human_content, delete
//     the pending_reviews row, mark the run cancelled, and broadcast
//     the run-status change to the websocket. SKY-206 — closes the
//     bug where a discarded review left a stale pending_reviews row
//     and a phantom pending_approval run on a now-queued task.
//
//   - Jira reversal: if the task is Jira-backed and we have a
//     SourceStatus snapshot (recorded at claim time), unassign and
//     transition back. Guarded against external mutations: skip if
//     someone else now owns the ticket, or if the ticket has
//     progressed out of the in-progress rule entirely (done, back to
//     pickup, etc.).
//
// Both halves are best-effort and logged-not-failed: the task is
// already queued by the time we get here; failing the response would
// confuse callers about what actually changed.
//
// taskID is taken separately from task because the pending_approval
// cleanup only needs the id — running it under a nil-task short-
// circuit (e.g. when db.GetTask transiently fails or the task row was
// deleted concurrently) would silently strand the very state this
// helper is meant to clean up. Jira reversal needs the loaded row so
// it nil-guards internally.
//
// orgID + userID are captured BEFORE the goroutine launches so the
// detached cleanup context inherits the requesting user's identity
// without rereading the (possibly nil-claimed) context post-cancel.
func (s *Server) finalizeRequeue(r *http.Request, orgID, userID, taskID string, task *domain.Task) {
	// Cleanup must outlive the request — the user already committed
	// to requeueing via the surrounding /undo or /requeue handler,
	// and bailing on browser close would strand the run in
	// pending_approval. WithoutCancel inherits values from
	// r.Context() (D9 will put request claims there) while breaking
	// the cancel chain.
	cleanupCtx := context.WithoutCancel(r.Context())
	s.cleanupPendingApprovalRun(cleanupCtx, orgID, userID, taskID, discardOutcomeRequeued)
	s.revertJiraStateIfApplicable(cleanupCtx, orgID, userID, task)
	// SKY-330: requeue clears both claim cols and flips status to
	// 'queued'. Peer Board sessions need a task_updated event to
	// pull the card back into the Queued column; without this they
	// keep showing the stale claim/status until the next refresh.
	// The cleanupPendingApprovalRun broadcast only fires for tasks
	// with a pending_approval run — most requeues don't.
	if s.ws != nil {
		s.ws.Broadcast(websocket.Event{
			Type:  "task_updated",
			OrgID: orgID,
			Data:  map[string]any{"task_id": taskID, "status": "queued"},
		})
	}
}

// cleanupPendingApprovalRun handles the SKY-206 case: the user
// returned a task to the queue (or dismissed it) while it had a
// pending_approval agent run — i.e. the agent prepared a PR review
// and the user threw it away rather than submitting. The agent
// process has long since exited (pending_approval is reached after
// the spawner's runAgent defer ran), so there's nothing to cancel
// at the process level — this is purely a DB cleanup: write the
// discard outcome to human_content, delete the pending_reviews +
// comments, flip the run row to cancelled with a discriminating
// stop_reason.
//
// outcome shapes the human_content note baked into run_memory so
// the next agent reading memory can distinguish "still on the
// docket, the human just didn't like this verdict" (requeued) from
// "the entity is done with — the human chose to walk away from it
// entirely" (dismissed). Same on-disk DB cleanup either way.
//
// Run-status broadcast lets the AgentCard collapse and the
// requeued/dismissed TaskCard reflect the new state without a
// manual refetch.
//
// All failures here are logged, not fatal: the calling handler has
// already flipped the task to its new state and the response should
// reflect that. Idempotent — a repeat call against an already-
// cancelled run finds no pending_approval row (the lookup filters on
// status='pending_approval') and exits silently.
func (s *Server) cleanupPendingApprovalRun(ctx context.Context, orgID, userID, taskID string, outcome discardOutcome) {
	// The whole cleanup runs as one tx-bound batch under the
	// requesting user's claims: in multi-mode every store call must
	// see request.jwt.claims set so RLS gates on creator/visibility
	// pass. Callers pass a cancellation-detached cleanupCtx
	// (context.WithoutCancel of r.Context()) so a client disconnect
	// mid-handler doesn't roll the cleanup back.
	//
	// All-or-nothing semantics: any DB error inside the closure
	// rolls back the human_content write + side-table deletes + the
	// status flip. The run stays in pending_approval, and a
	// subsequent /undo, /requeue, or dismiss re-enters here to
	// retry. UpdateRunMemoryHumanContent is idempotent (UPDATE)
	// and the deletes are no-ops when no row exists, so retry is
	// safe.
	var (
		runID         string
		flippedToDone bool
	)
	err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var lookupErr error
		runID, lookupErr = tx.AgentRuns.PendingApprovalIDForTask(ctx, orgID, taskID)
		if lookupErr != nil {
			return fmt.Errorf("pending_approval lookup: %w", lookupErr)
		}
		if runID == "" {
			return nil
		}

		// Detect which side-table held the row BEFORE deleting, so
		// the stop_reason and human_content can name the right
		// kind. Without this, a discarded PR ends up tagged
		// "review_discarded_by_user" — confusing in the UI and
		// breaks any downstream logic keyed on stop_reason that
		// needs to tell the two apart.
		kind := "review"
		if pr, prErr := tx.PendingPRs.ByRunID(ctx, orgID, runID); prErr != nil {
			return fmt.Errorf("pendingPRs.ByRunID: %w", prErr)
		} else if pr != nil {
			kind = "pr"
		}

		// Write the discard outcome to run_memory.human_content
		// BEFORE the row teardown. The next agent reading memory
		// on this entity should see the human's verdict as
		// authoritative — alongside the existing agent_content
		// (the agent's self-report) — so it can recalibrate.
		humanContent := buildDiscardHumanContent(outcome, kind)
		if err := tx.TaskMemory.UpdateRunMemoryHumanContent(ctx, orgID, runID, humanContent); err != nil {
			return fmt.Errorf("human_content write: %w", err)
		}

		// Tear down the pending review by run_id directly. The
		// DELETE-by-run-id helper is transactional across
		// comments + review and is a no-op when no review exists.
		if err := tx.Reviews.DeleteByRunID(ctx, orgID, runID); err != nil {
			return fmt.Errorf("reviews.DeleteByRunID: %w", err)
		}
		// Same idempotent no-op when no pending_prs row exists.
		// A run can have at most one pending entry across the two
		// tables (the spawner flips on either), but
		// cleanupPendingApprovalRun runs against the run id without
		// first determining which kind — so we attempt both deletes.
		if err := tx.PendingPRs.DeleteByRunID(ctx, orgID, runID); err != nil {
			return fmt.Errorf("pendingPRs.DeleteByRunID: %w", err)
		}

		stopReason := "review_discarded_by_user"
		if kind == "pr" {
			stopReason = "pr_discarded_by_user"
		}

		// Flip the run row terminal. ok=false means the row was
		// already cancelled by a concurrent path (idempotent
		// re-call, rare race) — skip the broadcast in that case
		// so we don't double-fire.
		ok, markErr := tx.AgentRuns.MarkDiscarded(ctx, orgID, runID, stopReason)
		if markErr != nil {
			return fmt.Errorf("MarkDiscarded: %w", markErr)
		}
		flippedToDone = ok
		return nil
	})
	if err != nil {
		log.Printf("[approval-discard] task %s cleanup (run held in pending_approval for retry): %v", taskID, err)
		return
	}
	if !flippedToDone || runID == "" {
		return
	}

	s.ws.Broadcast(websocket.Event{
		Type:  "agent_run_update",
		OrgID: orgID,
		RunID: runID,
		Data:  map[string]string{"status": "cancelled"},
	})
}

// buildDiscardHumanContent renders the post-run human verdict
// recorded when the user rejects an agent-prepared approval. The
// four shapes — requeued, dismissed, completed, claimed — give
// the next agent on this entity different recalibration signals:
//
//   - requeued: "try again, but not like that" (verdict was wrong;
//     the task is back in the queue).
//   - dismissed: "this entity wasn't worth pursuing" (the human
//     walked away from the entity entirely).
//   - completed: "you reached the right ballpark but I resolved
//     this myself" (the human accepted the task as done without
//     applying the agent's prepared review/PR).
//   - claimed: "I'll handle this myself" (the human took over the
//     task; the entity is still being worked on, just by hand).
//
// kind is "review" or "pr" — picks the right artifact noun so the
// next agent reading memory sees text that matches what was
// actually discarded (a review verdict vs a queued PR). Defaults
// to review wording for any unknown value.
func buildDiscardHumanContent(outcome discardOutcome, kind string) string {
	artifact := "review"
	verdictNoun := "verdict"
	if kind == "pr" {
		artifact = "PR"
		verdictNoun = "PR"
	}
	switch outcome {
	case discardOutcomeDismissed:
		return fmt.Sprintf(
			"**Outcome:** Human discarded the prepared %s and dismissed the task entirely.\n"+
				"**Implication:** The %s you proposed was not accepted, and the human chose to walk away from this entity rather than re-queue it. Future runs on similar entities should reconsider whether the situation warrants action at all.",
			artifact, verdictNoun)
	case discardOutcomeCompleted:
		return fmt.Sprintf(
			"**Outcome:** Human marked the task complete without submitting the prepared %s.\n"+
				"**Implication:** The human acknowledged the task as resolved but chose not to apply your %s to the entity. They likely handled it manually or via a different framing. Future runs should consider whether the agent's path was the right one or whether the human's resolution implies a gap in the prompt's approach.",
			artifact, verdictNoun)
	case discardOutcomeClaimed:
		return fmt.Sprintf(
			"**Outcome:** Human discarded the prepared %s and claimed the task to handle it themselves.\n"+
				"**Implication:** The %s you proposed was not accepted. The human took over to work the entity manually rather than apply your %s or re-queue it for another agent attempt — a sign that automation wasn't the right fit for this case.",
			artifact, verdictNoun, artifact)
	case discardOutcomeRedelegated:
		return fmt.Sprintf(
			"**Outcome:** Human re-delegated the task to the bot while this run was in flight; the prior %s was discarded in favor of a fresh attempt.\n"+
				"**Implication:** The human kept the agent on the task but didn't accept the %s you produced — likely a prompt-fit issue or a missing-context issue rather than an automation-fit issue. Reconsider the framing or scope before producing a new %s.",
			artifact, verdictNoun, artifact)
	default: // discardOutcomeRequeued
		return fmt.Sprintf(
			"**Outcome:** Human discarded the prepared %s without submitting it; task returned to the triage queue.\n"+
				"**Implication:** The %s you proposed was not accepted. Reconsider whether this entity warrants any %s at all, or whether a different framing is needed.",
			artifact, verdictNoun, artifact)
	}
}

// revertJiraStateIfApplicable was the body of handleUndo's Jira
// reversal block. Factored so /requeue picks up the same behavior —
// dragging a claimed Jira-backed task back to Queue should unassign
// and transition the ticket the same way Cmd-Z does. The guards
// against external mutations (someone else claimed it, status has
// progressed out of the in-progress rule) apply equally to both
// entry points.
func (s *Server) revertJiraStateIfApplicable(ctx context.Context, orgID, userID string, task *domain.Task) {
	if task == nil || task.EntitySource != "jira" || task.SourceStatus == "" {
		return
	}
	// SKY-463: the requeue/undo reversal (unassign + transition back) reverses
	// the user's own claim, so it must act as that user, not the org service
	// account. Resolve their Jira client. Best-effort: the task is already
	// requeued by the time we get here, so a missing/again-unresolvable user
	// credential is logged and skipped rather than failing the response or
	// degrading to the bot.
	jiraUserClient, jerr := s.jiraResolver.ForUser(ctx, orgID, userID)
	if jerr != nil {
		if errors.Is(jerr, jira.ErrNoJiraUserCredential) {
			log.Printf("[jira] requeue revert: no Jira credential for user %s; skipping ticket %s", userID, task.EntitySourceID)
		} else {
			log.Printf("[jira] requeue revert: resolve user client for %s: %v (skipping)", task.EntitySourceID, jerr)
		}
		return
	}
	// Same hot-path note as handleSwipe: requeue/undo is human-paced
	// and rule lookup is O(projects). The rule read goes through the
	// app-pool ListForTeam inside a WithTx so jira_rules_select RLS
	// is enforced — matching the user's requeue claim path. If a
	// future profile shows real cost, cache the per-team rules on
	// Server and refresh from onJiraChanged.
	var rule *domain.JiraProjectStatusRules
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		rule = lookupJiraRuleForTask(ctx, tx, task)
		return nil
	}); err != nil {
		log.Printf("[jira] requeue rule lookup: %v (skipping revert)", err)
		return
	}
	var inProgressMembers []string
	if rule != nil {
		inProgressMembers = rule.InProgressMembers
	}
	go func(issueKey, originalStatus string, ipMembers []string) {
		// Detached from the request (see handleSwipe's claim guard): the
		// revert outlives the undo response, so use a background context.
		bgCtx := context.Background()
		state := jiraUserClient.GetClaimState(bgCtx, issueKey)

		// Three assignee cases:
		//   - assigned to someone else -> skip undo entirely (manual reassignment)
		//   - unassigned -> skip Unassign (already unassigned), still transition
		//   - assigned to self -> proceed normally (unassign + transition)
		if state != nil && !state.AssignedToSelf && !state.Unassigned {
			log.Printf("[jira] requeue guard: %s reassigned to someone else, skipping", issueKey)
			return
		}
		// Skip if the ticket has moved out of the in-progress rule
		// entirely — that means someone progressed it (to done, back to
		// pickup, etc.) and we shouldn't yank it back. Membership rather
		// than strict-canonical match, because a user moving Claim →
		// "In Review" is still "working on it on my plate" and the
		// requeue should still unwind to the original status.
		if state != nil && len(ipMembers) > 0 {
			contains := false
			for _, m := range ipMembers {
				if m == state.StatusName {
					contains = true
					break
				}
			}
			if !contains {
				log.Printf("[jira] requeue guard: %s status is %q (not in in-progress members %v), skipping", issueKey, state.StatusName, ipMembers)
				return
			}
		}

		if state == nil || state.AssignedToSelf {
			if err := jiraUserClient.Unassign(bgCtx, issueKey); err != nil {
				log.Printf("[jira] failed to unassign %s on requeue: %v", issueKey, err)
			}
		}
		if err := jiraUserClient.TransitionTo(bgCtx, issueKey, originalStatus); err != nil {
			log.Printf("[jira] failed to transition %s back to %q on requeue: %v", issueKey, originalStatus, err)
		}
	}(task.EntitySourceID, task.SourceStatus, inProgressMembers)
}

func parseSnoozeUntil(s string) (time.Time, error) {
	now := time.Now()
	switch s {
	case "1h":
		return now.Add(1 * time.Hour), nil
	case "2h":
		return now.Add(2 * time.Hour), nil
	case "4h":
		return now.Add(4 * time.Hour), nil
	case "tomorrow":
		tomorrow := now.AddDate(0, 0, 1)
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 9, 0, 0, 0, tomorrow.Location()), nil
	default:
		return time.Parse(time.RFC3339, s)
	}
}
