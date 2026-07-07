package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
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
	// Under the "snoozed ↔ unclaimed" invariant,
	// this is only ever set on queue-lane tasks. Any claim-axis
	// transition wakes the task atomically (clears snooze_until +
	// flips status='snoozed' → 'queued'), so claimed cards on the
	// Board never carry a snooze. The Cards triage view renders
	// future-snoozed entries hidden via the QueuedTasks filter;
	// the Board's Queue lane could optionally render them at the
	// tail with a "wakes Mar 5" badge (deferred UI follow-up).
	SnoozeUntil string `json:"snooze_until,omitempty"`
	// OpenSubtaskCount lets the UI flag a task whose Jira entity has open
	// subtasks — the "consider decomposing" signal. Zero for
	// GitHub tasks and Jira tickets without subtasks.
	OpenSubtaskCount int `json:"open_subtask_count"`
	// Claim cols: exposed so the per-card assignee picker
	// can render the current assignee without a second round-trip.
	// Exactly one is set when claimed; both empty when unclaimed.
	// omitempty keeps the wire shape clean for the unclaimed-queue case.
	ClaimedByAgentID string `json:"claimed_by_agent_id,omitempty"`
	ClaimedByUserID  string `json:"claimed_by_user_id,omitempty"`
	// TeamID is the task's owning team. Exposed so the multi-team board
	// can color-code / tag rows by team. Always set; the
	// frontend only surfaces it when the viewer belongs to ≥2 teams.
	// TODO: board row color-coding consumes this.
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
	// ?include_snoozed=true keeps future-snoozed rows in the
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

	// Snooze mutates the task (lifecycle axis) — a viewer can't (TFAC-447).
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
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
		// Snooze is queue-only ("snoozed ↔ both claim
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
// State-driven requeue (Board's drag-to-Queue, the "Return
// to queue" button) lives at /requeue and skips the swipe row.
// Same finalizer, same observable outcome — different audit shape.
func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	// Undo reverses a swipe — a task mutation a viewer can't make (TFAC-447).
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

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
// drag-to-Queue gesture and the AgentCard's
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

	// Requeue moves a task back to the queue — a viewer can't (TFAC-447).
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

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

// handleTaskAdvance is the board's manual user transition —
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

	// Advancing a task's status is a team-scoped write — viewers can't (TFAC-447).
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

	// Validate the path id as a UUID up front. On Postgres
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
	// terminal-state AgentCard (failed, cancelled) by
	// dragging it to the Done column. The agent's prepared review,
	// if any, is being discarded — the user is signalling "the work
	// is finished" without applying the agent's verdict to GitHub.
	discardOutcomeCompleted
	// discardOutcomeClaimed: user claimed the task while it had a
	// pending_approval run (Board's drag-to-You from Agent/Done, or
	// the Cards swipe-right against a delegated task). The agent's
	// prepared review is being thrown away in favor of the human
	// handling the entity themselves. This case exists primarily
	// to close the race where a stale frontend agentRuns
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
//   - artifact teardown: resolve every unresolved artifact the task's
//     runs hold (close all draft PRs, dismiss all pending reviews) and
//     write the discard verdict to run_memory.human_content, so a
//     returned-to-queue task leaves no stranded GitHub draft / pending
//     review. Decoupled from run lifecycle — it never flips runs.status.
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
// taskID is taken separately from task because the artifact teardown
// only needs the id — running it under a nil-task short-circuit (e.g.
// when db.GetTask transiently fails or the task row was deleted
// concurrently) would silently strand the very state this helper is
// meant to clean up. Jira reversal needs the loaded row so it
// nil-guards internally.
//
// orgID + userID are captured BEFORE the goroutine launches so the
// detached cleanup context inherits the requesting user's identity
// without rereading the (possibly nil-claimed) context post-cancel.
func (s *Server) finalizeRequeue(r *http.Request, orgID, userID, taskID string, task *domain.Task) {
	// Cleanup must outlive the request — the user already committed
	// to requeueing via the surrounding /undo or /requeue handler,
	// and bailing on browser close would strand an unresolved GitHub
	// draft / pending review. WithoutCancel inherits values from
	// r.Context() (D9 will put request claims there) while breaking
	// the cancel chain.
	cleanupCtx := context.WithoutCancel(r.Context())
	s.teardownTaskArtifacts(cleanupCtx, orgID, userID, taskID, discardOutcomeRequeued)
	s.revertJiraStateIfApplicable(cleanupCtx, orgID, userID, task)
	// Requeue clears both claim cols and flips status to
	// 'queued'. Peer Board sessions need a task_updated event to
	// pull the card back into the Queued column; without this they
	// keep showing the stale claim/status until the next refresh.
	// teardownTaskArtifacts no longer broadcasts a run-status change
	// (a resolve is decoupled from run lifecycle), so this is the sole
	// board-update signal for a requeue.
	if s.ws != nil {
		s.ws.Broadcast(websocket.Event{
			Type:  "task_updated",
			OrgID: orgID,
			Data:  map[string]any{"task_id": taskID, "status": "queued"},
		})
	}
}

// teardownTaskArtifacts is the task-level "force-resolve-all" gesture: the user
// dragged a card to Done / dismissed it / claimed it / returned it to the queue
// while it still had unresolved artifacts (draft PRs, pending reviews — same or
// different repos). It resolves EVERY unresolved artifact the task's runs hold so
// nothing strands: each draft PR is closed on GitHub + flipped to closed; each
// pending review (finalized or not) has its GitHub pending review deleted +
// flipped to dismissed. Pushed branches are kept (retention is separate).
//
// Decoupled from run lifecycle (TFAC-379): this never flips runs.status. A live
// run is cancelled by the caller (swipeTeardownRuns' spawner.Cancel pass) — the
// process teardown owns that transition; a terminal run simply stays terminal.
// Keyed on the task's runs (ListForTask spans the blueprint's step runs and any
// standalone run) rather than the retired pending_approval lookup.
//
// outcome shapes the discard note baked into run_memory.human_content so the next
// agent reading memory can distinguish "still on the docket, the human just
// didn't like this verdict" (requeued) from "walked away from the entity"
// (dismissed) from "resolved it themselves" (completed) from "took over"
// (claimed). The note is written per run that held a resolved artifact, keyed on
// that run's dominant kind (PR precedence, matching the legacy single-artifact
// path).
//
// All-or-nothing per call: any DB error inside the closure rolls back the whole
// batch (notes + flips + audit rows), leaving the artifacts unresolved for a
// retry on the next /undo, /requeue, dismiss, or complete. UpdateRunMemoryHumanContent
// is idempotent and the flips re-target the same predicate set, so retry is safe.
// All failures are logged, not fatal: the calling handler has already flipped the
// task to its new state.
func (s *Server) teardownTaskArtifacts(ctx context.Context, orgID, userID, taskID string, outcome discardOutcome) {
	// Draft PRs captured inside the tx (state already flipped) and closed on GitHub
	// AFTER it commits — a network call must not hold the tx open. Dismissed reviews
	// need no post-tx pass: a review is staged TF-side (TFAC-494), so the in-tx flip
	// to dismissed is the whole resolution — there is no GitHub object to retire.
	var prArtifacts []domain.Artifact
	err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		runs, err := tx.AgentRuns.ListForTask(ctx, orgID, taskID)
		if err != nil {
			return fmt.Errorf("list runs for task: %w", err)
		}
		for i := range runs {
			runID := runs[i].ID
			arts, artErr := tx.Artifacts.ListByRun(ctx, orgID, runID)
			if artErr != nil {
				return fmt.Errorf("artifacts.ListByRun(%s): %w", runID, artErr)
			}
			draftPRs := domain.AllDraftPullRequests(arts)
			pendingReviews := domain.AllPendingReviewArtifacts(arts)
			if len(draftPRs) == 0 && len(pendingReviews) == 0 {
				continue
			}

			// Write the discard note BEFORE the flips so the next agent reading
			// memory on this entity sees the human's verdict alongside the agent's
			// self-report. Keyed on the run's dominant kind (PR precedence).
			kind := "review"
			if len(draftPRs) > 0 {
				kind = "pr"
			}
			if err := tx.TaskMemory.UpdateRunMemoryHumanContent(ctx, orgID, runID, buildDiscardHumanContent(outcome, kind)); err != nil {
				return fmt.Errorf("human_content write: %w", err)
			}

			// Abandon each pending review by flipping its artifact to dismissed. No
			// GitHub call and no audit row: the review is staged TF-side (TFAC-494), so
			// a dismiss is a purely local state change, not an org-credential write
			// (external_actions records only writes). The flip is the whole teardown;
			// the proposed snapshot is preserved on the row.
			for j := range pendingReviews {
				rv := pendingReviews[j]
				dismissed := rv
				dismissed.State = domain.ArtifactStateReviewDismissed
				if _, err := tx.Artifacts.Upsert(ctx, orgID, dismissed); err != nil {
					return fmt.Errorf("artifacts.Upsert(dismissed): %w", err)
				}
			}
			// Abandon each draft PR by flipping its artifact to closed (the GitHub
			// close runs after the tx). The pushed branch and the proposed snapshot
			// are preserved — abandonment retires the PR object, not the work.
			for j := range draftPRs {
				pr := draftPRs[j]
				closed := pr
				closed.State = domain.ArtifactStatePRClosed
				if _, err := tx.Artifacts.Upsert(ctx, orgID, closed); err != nil {
					return fmt.Errorf("artifacts.Upsert(closed): %w", err)
				}
				if err := tx.ExternalActions.Record(ctx, orgID,
					githubApprovalAction(&pr, userID, domain.ActionPRClosed, domain.ArtifactStatePRDraft, domain.ArtifactStatePRClosed)); err != nil {
					return fmt.Errorf("external_actions.Record(closed): %w", err)
				}
				prArtifacts = append(prArtifacts, pr)
			}
		}
		return nil
	})
	if err != nil {
		approvalDiscardLog.Error("task artifact teardown failed; artifacts left unresolved for retry", "task", taskID, "error", err)
		return
	}

	// Resolve on GitHub (best-effort, outside the tx). The artifacts are already
	// marked closed/dismissed; a GitHub failure leaves the object for reconciliation
	// to retire later and must never fail the requeue/complete. Branches stay.
	for i := range prArtifacts {
		closeDraftPRBestEffort(ctx, s.ghResolver, orgID, &prArtifacts[i])
	}
	// Dismissed reviews need no post-tx pass — they were staged TF-side and the
	// in-tx flip is their whole resolution (TFAC-494).
}

// closeDraftPRBestEffort closes the abandoned draft PR on GitHub. Best-effort:
// every failure is logged, never returned — the artifact is already marked
// closed and the run/task already resolved, so a GitHub hiccup mustn't unwind
// that. owner/repo/number come from the artifact's target; the per-repo client
// resolves App-installation-token → PAT like every other PR mutation. A free
// function (taking the resolver) so both the task-level teardown and the
// per-artifact dismiss endpoint share one GitHub-resolution path. The pushed
// branch is never touched (retention is a separate concern).
func closeDraftPRBestEffort(ctx context.Context, resolver ghclient.Resolver, orgID string, art *domain.Artifact) {
	owner, repo, number, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		approvalDiscardLog.Warn("draft PR artifact has a malformed target; skipping GitHub close", "artifact", art.ID, "target", art.Target)
		return
	}
	gh, err := resolver.ClientForRepo(ctx, orgID, owner, repo)
	if err != nil {
		approvalDiscardLog.Warn("resolve github client for draft PR close failed", "artifact", art.ID, "owner", owner, "repo", repo, "error", err)
		return
	}
	if err := gh.ClosePR(ctx, owner, repo, number); err != nil {
		approvalDiscardLog.Warn("close draft PR on github failed (artifact already marked closed)", "artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
	}
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
	// The requeue/undo reversal (unassign + transition back) reverses
	// the user's own claim, so it must act as that user, not the org service
	// account. Resolve their Jira client. Best-effort: the task is already
	// requeued by the time we get here, so a missing/again-unresolvable user
	// credential is logged and skipped rather than failing the response or
	// degrading to the bot.
	jiraUserClient, jerr := s.jiraResolver.ForUser(ctx, orgID, userID)
	if jerr != nil {
		if errors.Is(jerr, jira.ErrNoJiraUserCredential) {
			jiraLog.Warn("requeue revert: no jira credential for user, skipping ticket", "user", userID, "ticket", task.EntitySourceID)
		} else {
			jiraLog.Warn("requeue revert: resolve user client failed, skipping", "ticket", task.EntitySourceID, "error", jerr)
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
		jiraLog.Warn("requeue rule lookup failed, skipping revert", "error", err)
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
			jiraLog.Warn("requeue guard: reassigned to someone else, skipping", "issue", issueKey)
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
				jiraLog.Warn("requeue guard: status not in in-progress members, skipping", "issue", issueKey, "status", state.StatusName, "in_progress_members", ipMembers)
				return
			}
		}

		if state == nil || state.AssignedToSelf {
			if err := jiraUserClient.Unassign(bgCtx, issueKey); err != nil {
				jiraLog.Error("failed to unassign on requeue", "issue", issueKey, "error", err)
			}
		}
		if err := jiraUserClient.TransitionTo(bgCtx, issueKey, originalStatus); err != nil {
			jiraLog.Error("failed to transition back on requeue", "issue", issueKey, "status", originalStatus, "error", err)
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
