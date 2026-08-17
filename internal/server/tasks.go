package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
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
	// future-snoozed entries hidden via the TaskListFilter status filter;
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

// taskIDOr404 validates the {id} path value as a UUID and writes the 404 for
// a malformed one. On Postgres tasks.id is the uuid column type, so a
// malformed id would otherwise surface as SQLSTATE 22P02 from the first store
// call → 500. Treating malformed ids as "task not found" keeps the API
// portable across SQLite (id TEXT, no parse error) and Postgres, and matches
// the disclosure rule — a malformed id names nothing the caller may learn
// about.
func taskIDOr404(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		notFound(w, "task")
		return "", false
	}
	return id, true
}

// taskListRequest is the body of POST /api/tasks/list: the filter set plus
// the shared paging fields. Every field is optional — `{}` is "every task I
// can see that isn't sleeping, first page" — but nothing about it is implicit:
// a caller that wants only the pickable queue says so, and a caller that wants
// the last week of closed work passes the window it means.
type taskListRequest struct {
	// Statuses selects the lanes. Empty = all. Validated against
	// db.TaskListStatuses; an unknown value is a client fault, not an
	// empty page.
	Statuses []string `json:"statuses"`
	// TeamIDs is the per-page multi-team view scope. Empty = the union of
	// the viewer's teams (the RLS-scoped default). Well-formedness is
	// checked strictly: a corrupt filter must not silently widen back to
	// the union.
	TeamIDs []string `json:"team_ids"`
	// OnlyUnclaimed keeps just the rows nobody has taken.
	OnlyUnclaimed bool `json:"only_unclaimed"`
	// IncludeSnoozed keeps rows still inside their snooze window.
	IncludeSnoozed bool `json:"include_snoozed"`
	// ClosedSince (RFC3339) windows done/dismissed rows by closed_at.
	// Absent = no window. The board asks for the seven days it wants to
	// render; the server no longer applies one behind the caller's back.
	ClosedSince string `json:"closed_since"`

	httpx.PageRequest
}

// taskListFilterKey is the canonicalized form of a taskListRequest's filters —
// sorted, deduped, and normalized — that the page token is fingerprinted
// against. Two requests that mean the same query fingerprint identically, and
// a token minted for one filter set is refused for another.
type taskListFilterKey struct {
	Statuses       []string `json:"statuses"`
	TeamIDs        []string `json:"team_ids"`
	OnlyUnclaimed  bool     `json:"only_unclaimed"`
	IncludeSnoozed bool     `json:"include_snoozed"`
	ClosedSince    string   `json:"closed_since"`
}

// handleTaskList is the tasks resource's list read — every task-list surface
// (the triage deck, each board column) is a filter set over this one route.
//
// It is a POST because the filters are a body, not a query string: repeated
// ?status=/?team_id= params were how the old GET pair drifted into two
// spellings of the same read with different hidden defaults. A body-carrying
// read registers through apiMutating like any other POST so the CSRF
// same-origin check applies symmetrically — the method is what a browser
// preflights on, not the intent. The read itself has no side effects.
func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req taskListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	var v httpx.Validation
	statuses := canonicalStrings(req.Statuses)
	for _, st := range statuses {
		if !slices.Contains(db.TaskListStatuses, st) {
			v.Invalid("statuses", fmt.Sprintf("unknown status %q; must be one of: %s",
				st, strings.Join(db.TaskListStatuses, ", ")))
		}
	}
	teamIDs := canonicalStrings(req.TeamIDs)
	for _, id := range teamIDs {
		if _, err := uuid.Parse(id); err != nil {
			v.Invalid("team_ids", fmt.Sprintf("team id %q is not a valid team id", id))
		}
	}
	var closedSince *time.Time
	closedSinceKey := ""
	if req.ClosedSince != "" {
		ts, err := time.Parse(time.RFC3339, req.ClosedSince)
		if err != nil {
			v.Invalid("closed_since", "closed_since must be an RFC3339 timestamp")
		} else {
			ts = ts.UTC()
			closedSince = &ts
			closedSinceKey = ts.Format(time.RFC3339Nano)
		}
	}
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(taskListFilterKey{
		Statuses:       statuses,
		TeamIDs:        teamIDs,
		OnlyUnclaimed:  req.OnlyUnclaimed,
		IncludeSnoozed: req.IncludeSnoozed,
		ClosedSince:    closedSinceKey,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var tasks []domain.Task
	var total int
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		tasks, total, e = tx.Tasks.List(r.Context(), orgID, db.TaskListFilter{
			Statuses:       statuses,
			TeamIDs:        teamIDs,
			OnlyUnclaimed:  req.OnlyUnclaimed,
			IncludeSnoozed: req.IncludeSnoozed,
			ClosedSince:    closedSince,
		}, db.ListOpts{Limit: page.Limit, Offset: page.Offset})
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	items := make([]taskJSON, len(tasks))
	for i, t := range tasks {
		items[i] = taskToJSON(t)
	}
	httpx.WriteList(w, page, items, total)
}

// canonicalStrings sorts and dedups a filter list so the same query always
// produces the same page-token fingerprint however the client ordered it, and
// so a repeated value can't multiply an IN-list. Nil in, nil out — an absent
// filter and an empty one mean the same thing (no narrowing) and must
// fingerprint the same.
func canonicalStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	out = slices.Compact(out)
	return out
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := taskIDOr404(w, r)
	if !ok {
		return
	}
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
	id, ok := taskIDOr404(w, r)
	if !ok {
		return
	}

	var req snoozeRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	// Snooze mutates the task (lifecycle axis) — a viewer can't (TFAC-447).
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

	if !validHesitationField(w, req.HesitationMs) {
		return
	}

	until, ok := parseSnoozeUntilField(w, req.Until)
	if !ok {
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
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "can't snooze a claimed task; requeue or complete it first",
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
	id, ok := taskIDOr404(w, r)
	if !ok {
		return
	}

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
// drag-to-Queue gesture and the AgentCard's "Return to queue" button
// on a run awaiting approval of its artifact. Both of those
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
	id, ok := taskIDOr404(w, r)
	if !ok {
		return
	}

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
	id, ok := taskIDOr404(w, r)
	if !ok {
		return
	}

	var body struct {
		To string `json:"to"`
	}
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}
	if body.To != "in_progress" && body.To != "in_review" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: "to must be one of: in_progress, in_review",
			Field:   "to",
		})
		return
	}

	// Advancing a task's status is a team-scoped write — viewers can't (TFAC-447).
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
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
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "task not advanceable — must be claimed by you and currently in queued/in_progress/in_review",
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
// into conversation_memory differs — the next agent reading prior memory
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
	// finished AgentCard (failed, or parked after a cancel) by
	// dragging it to the Done column. The agent's prepared review,
	// if any, is being discarded — the user is signalling "the work
	// is finished" without applying the agent's verdict to GitHub.
	discardOutcomeCompleted
	// discardOutcomeClaimed: user claimed the task while a run was
	// awaiting approval (Board's drag-to-You from Agent/Done, or
	// the Cards swipe-right against a delegated task). The agent's
	// prepared review is being thrown away in favor of the human
	// handling the entity themselves. This case exists primarily
	// to close the race where a stale frontend conversations
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
//     write the discard verdict to conversation_memory.human_content, so a
//     returned-to-queue task leaves no stranded GitHub draft / pending
//     review. Decoupled from run lifecycle — it never flips conversations.status.
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
// Decoupled from run lifecycle (TFAC-379): this never flips
// conversations.status. A live run is cancelled by the caller
// (swipeTeardownConversations' spawner.Cancel pass) — the process teardown owns that
// transition; a terminal run simply stays terminal. Keyed on the task's
// runs (ListForTask spans the blueprint's step runs and any standalone run)
// rather than on a run status.
//
// outcome shapes the discard note baked into conversation_memory.human_content so the next
// agent reading memory can distinguish "still on the docket, the human just
// didn't like this verdict" (requeued) from "walked away from the entity"
// (dismissed) from "resolved it themselves" (completed) from "took over"
// (claimed). The note is written per run that held a resolved artifact, keyed on
// that run's dominant kind (PR precedence, matching the legacy single-artifact
// path).
//
// All-or-nothing per call: any DB error inside the closure rolls back the whole
// batch (notes + flips + audit rows), leaving the artifacts unresolved for a
// retry on the next /undo, /requeue, dismiss, or complete. UpdateConversationMemoryHumanContent
// is idempotent and the flips re-target the same predicate set, so retry is safe.
// All failures are logged, not fatal: the calling handler has already flipped the
// task to its new state.
func (s *Server) teardownTaskArtifacts(ctx context.Context, orgID, userID, taskID string, outcome discardOutcome) {
	// Draft PRs captured inside the tx (state already flipped) and closed on GitHub
	// AFTER it commits — a network call must not hold the tx open. Dismissed reviews
	// need no post-tx pass: a review is staged TF-side (TFAC-494), so the in-tx flip
	// to dismissed is the whole resolution — there is no GitHub object to retire.
	//
	// Which credential each draft PR's close is made under is classified BEFORE
	// the tx opens, for the same reason the closes themselves run after it: the
	// audit rows are composed inside that tx, and a classification can reach
	// GitHub.
	credentials := s.draftPRCredentials(ctx, orgID, userID, taskID)

	var prArtifacts []domain.Artifact
	err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		convs, err := tx.Conversations.ListForTask(ctx, orgID, taskID)
		if err != nil {
			return fmt.Errorf("list runs for task: %w", err)
		}
		for i := range convs {
			conversationID := convs[i].ID
			arts, artErr := tx.Artifacts.ListByConversation(ctx, orgID, conversationID)
			if artErr != nil {
				return fmt.Errorf("artifacts.ListByConversation(%s): %w", conversationID, artErr)
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
			if err := tx.TaskMemory.UpdateConversationMemoryHumanContent(ctx, orgID, conversationID, buildDiscardHumanContent(outcome, kind)); err != nil {
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
					githubApprovalAction(&pr, userID, domain.ActionPRClosed, domain.ArtifactStatePRDraft, domain.ArtifactStatePRClosed,
						credentialForTarget(credentials, pr.Target))); err != nil {
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

// draftPRCredentials classifies the acting GitHub credential for every repo the
// task's unresolved draft PRs live in, keyed by "owner/repo". It exists so the
// teardown's audit rows can name that credential without probing GitHub from
// inside the write tx those rows are composed into — the read pass and the
// classification are both finished before that tx opens.
//
// Best-effort: a read failure yields an empty map and every row falls back to
// the App, exactly as an unclassifiable repo does. The teardown itself is
// unaffected — it re-reads the artifacts under its own tx and is the authority
// on which ones it resolves.
func (s *Server) draftPRCredentials(ctx context.Context, orgID, userID, taskID string) map[string]string {
	type ownerRepo struct{ owner, repo string }
	repos := map[string]ownerRepo{}
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		convs, err := tx.Conversations.ListForTask(ctx, orgID, taskID)
		if err != nil {
			return err
		}
		for i := range convs {
			arts, artErr := tx.Artifacts.ListByConversation(ctx, orgID, convs[i].ID)
			if artErr != nil {
				return artErr
			}
			for _, pr := range domain.AllDraftPullRequests(arts) {
				if owner, repo, _, ok := domain.ParsePRTarget(pr.Target); ok {
					repos[owner+"/"+repo] = ownerRepo{owner: owner, repo: repo}
				}
			}
		}
		return nil
	}); err != nil {
		approvalDiscardLog.Warn("pre-read draft PRs for credential attribution failed; teardown rows will record the app",
			"task", taskID, "error", err)
		return nil
	}
	out := make(map[string]string, len(repos))
	for repoID, or := range repos {
		out[repoID] = githubCredentialFor(ctx, s.ghResolver, orgID, or.owner, or.repo)
	}
	return out
}

// credentialForTarget looks a PR target's repo up in the map draftPRCredentials
// built. A miss — an unparseable target, or a draft PR that appeared between the
// pre-pass and the tx — reports the App, the same answer an unclassifiable repo
// gets.
func credentialForTarget(credentials map[string]string, target string) string {
	owner, repo, _, ok := domain.ParsePRTarget(target)
	if !ok {
		return domain.CredentialGitHubApp
	}
	if cred, hit := credentials[owner+"/"+repo]; hit {
		return cred
	}
	return domain.CredentialGitHubApp
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

// parseSnoozeUntilField validates the "until" body field for both ways of
// snoozing — the standalone route and the swipe arm — so the two gestures
// take the same input and reject the same faults: absent (a snooze needs a
// wake time or the row parks forever) and unparseable. On failure the error
// response is already written; callers return immediately.
func parseSnoozeUntilField(w http.ResponseWriter, raw string) (time.Time, bool) {
	if raw == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "until is required: 1h, 2h, 4h, tomorrow, or an RFC3339 timestamp",
			Field:   "until",
		})
		return time.Time{}, false
	}
	until, err := parseSnoozeUntil(raw)
	if err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: "invalid snooze duration: " + err.Error(),
			Field:   "until",
		})
		return time.Time{}, false
	}
	// A wake time already in the past parks the task in a state the sweeper
	// wakes on its next pass — a snooze that reports success and does nothing.
	// The four presets are future by construction, so this only bites the
	// RFC3339 arm, which is the arm the UI never sends and an API caller does.
	if !until.After(time.Now()) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonOutOfRange,
			Message: "until must be in the future",
			Field:   "until",
		})
		return time.Time{}, false
	}
	return until, true
}

// validHesitationField rejects a negative hesitation. It is the milliseconds a
// user spent deciding before the gesture, so below zero is not a slow decision
// but a broken clock or a hand-written body — and it lands in swipe_events,
// where it skews the dwell-time aggregates nothing downstream re-validates.
// On failure the error response is already written.
func validHesitationField(w http.ResponseWriter, ms int) bool {
	if ms < 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonOutOfRange,
			Message: "hesitation_ms must be zero or greater",
			Field:   "hesitation_ms",
		})
		return false
	}
	return true
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
