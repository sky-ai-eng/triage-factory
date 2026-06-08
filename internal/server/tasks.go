package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
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
	teamFilter := teamscope.FilterParam(r)
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
	teamFilter := teamscope.FilterParam(r)
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
