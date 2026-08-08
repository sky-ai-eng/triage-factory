package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// TaskCloseCancelIntentFactory is what a per-backend test file hands to
// RunTaskCloseCancelIntentConformance. It returns the wired TaskStore, the
// orgID every method takes, and a seeder staging the fixture shapes.
//
// This has its own factory rather than riding TaskStoreFactory because the
// method under test writes across three tables: the fixtures it needs are a
// task, an event, a blueprint_run, and a conversation linking them, and the
// backends disagree on nearly every column of the latter two.
type TaskCloseCancelIntentFactory func(t *testing.T) (store db.TaskStore, orgID string, seed TaskCloseCancelIntentSeeder)

// TaskCloseCancelIntentSeeder stages the (task, event, blueprint, run) graph
// the close transaction spans, and reads back what it wrote. Production mints
// these rows through several stores at once; the callbacks write them directly
// against each backend's own schema so the suite stays schema-blind.
type TaskCloseCancelIntentSeeder struct {
	// Task stages a fresh entity + queued task and returns the task id.
	// Each call must produce a distinct entity so the tasks dedup index
	// doesn't collapse independent subtests.
	Task func(t *testing.T) (taskID string)

	// Event stages one event on the task's entity and returns its id — the
	// closing event the audit row references.
	Event func(t *testing.T, taskID string) (eventID string)

	// Run stages a blueprint_run in blueprintStatus with one child
	// conversation on taskID whose STORED status is convStatus (empty string
	// = SQL NULL, the mid-flight state). Returns both ids.
	Run func(t *testing.T, taskID, blueprintStatus, convStatus string) (blueprintRunID, convID string)

	// BareRun stages a conversation on taskID with NO blueprint parent, in
	// the given stored status. Returns its id.
	BareRun func(t *testing.T, taskID, convStatus string) (convID string)

	// CancelRequested reads a blueprint_run's cancel_requested flag.
	CancelRequested func(t *testing.T, blueprintRunID string) bool

	// TaskStatus reads a task's status column.
	TaskStatus func(t *testing.T, taskID string) string

	// CloseAuditCount counts the task's task_events rows of kind 'closed'.
	CloseAuditCount func(t *testing.T, taskID string) int
}

// RunTaskCloseCancelIntentConformance is the shared suite for
// TaskStore.CloseWithRunCancelIntentSystem — the close that carries the stop
// intent for its own runs.
//
// The negative space is where the pressure is. The stamp is what makes a
// failed kill self-healing, but it is also permanent: a stamped run is one the
// claim gate will never drive again. So the two subtests that assert nothing
// happened — a finished blueprint left alone, and a replayed close on a task
// that is already terminal — are load-bearing in a way the happy path isn't.
// A conversation may legitimately be resumed under a closed task (a finished
// blueprint's final step), and a close that reached back and stamped one would
// kill work the user just asked for.
func RunTaskCloseCancelIntentConformance(t *testing.T, mk TaskCloseCancelIntentFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Closes_stamps_active_runs_and_returns_them", func(t *testing.T) {
		s, orgID, seed := mk(t)
		taskID := seed.Task(t)
		eventID := seed.Event(t, taskID)
		brID, convID := seed.Run(t, taskID, "running", "")

		closed, runIDs, err := s.CloseWithRunCancelIntentSystem(ctx, orgID, taskID, "entity_closed", "github:pr:merged", eventID)
		if err != nil {
			t.Fatalf("CloseWithRunCancelIntentSystem: %v", err)
		}
		if !closed {
			t.Error("closed = false, want true — the task was active")
		}
		if len(runIDs) != 1 || runIDs[0] != convID {
			t.Errorf("run ids = %v, want exactly [%s] — the caller stops what the tx stamped", runIDs, convID)
		}
		if !seed.CancelRequested(t, brID) {
			t.Error("cancel_requested = false; the close must carry the stop intent for its active runs")
		}
		if got := seed.TaskStatus(t, taskID); got != "done" {
			t.Errorf("task status = %q, want done", got)
		}
		if n := seed.CloseAuditCount(t, taskID); n != 1 {
			t.Errorf("close-audit rows = %d, want 1", n)
		}
	})

	t.Run("Leaves_a_finished_blueprint_alone", func(t *testing.T) {
		// A blueprint that completed before its task closed is not this
		// close's to call off: a follow-up may resume its final step
		// afterwards, and a stamp here would refuse that resume forever.
		s, orgID, seed := mk(t)
		taskID := seed.Task(t)
		brID, _ := seed.Run(t, taskID, "completed", "completed")

		closed, runIDs, err := s.CloseWithRunCancelIntentSystem(ctx, orgID, taskID, "entity_closed", "github:pr:merged", "")
		if err != nil {
			t.Fatalf("CloseWithRunCancelIntentSystem: %v", err)
		}
		if !closed {
			t.Error("closed = false, want true")
		}
		if len(runIDs) != 0 {
			t.Errorf("run ids = %v, want none — a terminal conversation is not stopped", runIDs)
		}
		if seed.CancelRequested(t, brID) {
			t.Error("cancel_requested = true on a finished blueprint; the post-close resume flow is now refused forever")
		}
	})

	t.Run("Already_terminal_task_stamps_nothing", func(t *testing.T) {
		// The replay case. The second close finds the task done and must be
		// inert — the run active by then may be one a user resumed after the
		// first close landed.
		s, orgID, seed := mk(t)
		taskID := seed.Task(t)
		eventID := seed.Event(t, taskID)

		if closed, _, err := s.CloseWithRunCancelIntentSystem(ctx, orgID, taskID, "entity_closed", "github:pr:merged", eventID); err != nil || !closed {
			t.Fatalf("first close = (%v, %v), want (true, nil)", closed, err)
		}
		brID, _ := seed.Run(t, taskID, "running", "")

		closed, runIDs, err := s.CloseWithRunCancelIntentSystem(ctx, orgID, taskID, "entity_closed", "github:pr:merged", eventID)
		if err != nil {
			t.Fatalf("replayed close: %v", err)
		}
		if closed {
			t.Error("closed = true on an already-terminal task, want false")
		}
		if len(runIDs) != 0 {
			t.Errorf("run ids = %v, want none — a replay must not re-stop anything", runIDs)
		}
		if seed.CancelRequested(t, brID) {
			t.Error("cancel_requested = true; a replayed close reached back and stamped a run started after it")
		}
		if n := seed.CloseAuditCount(t, taskID); n != 1 {
			t.Errorf("close-audit rows after the replay = %d, want 1 — the audit row is INSERT-or-nothing", n)
		}
	})

	t.Run("Task_with_no_runs_closes_plainly", func(t *testing.T) {
		s, orgID, seed := mk(t)
		taskID := seed.Task(t)
		eventID := seed.Event(t, taskID)

		closed, runIDs, err := s.CloseWithRunCancelIntentSystem(ctx, orgID, taskID, "auto_closed_by_event", "github:pr:ci_check_passed", eventID)
		if err != nil {
			t.Fatalf("CloseWithRunCancelIntentSystem: %v", err)
		}
		if !closed || len(runIDs) != 0 {
			t.Errorf("= (closed %v, ids %v), want (true, none)", closed, runIDs)
		}
		if got := seed.TaskStatus(t, taskID); got != "done" {
			t.Errorf("task status = %q, want done", got)
		}
		if n := seed.CloseAuditCount(t, taskID); n != 1 {
			t.Errorf("close-audit rows = %d, want 1", n)
		}
	})

	t.Run("Blueprintless_run_is_returned_and_stamps_nothing", func(t *testing.T) {
		// A conversation with no blueprint parent still gets stopped — it is
		// in the returned set — but there is no sequence to call off, so the
		// stamp has nothing to write and must not error trying.
		s, orgID, seed := mk(t)
		taskID := seed.Task(t)
		convID := seed.BareRun(t, taskID, "")

		closed, runIDs, err := s.CloseWithRunCancelIntentSystem(ctx, orgID, taskID, "entity_closed", "github:pr:merged", "")
		if err != nil {
			t.Fatalf("CloseWithRunCancelIntentSystem: %v", err)
		}
		if !closed {
			t.Error("closed = false, want true")
		}
		if len(runIDs) != 1 || runIDs[0] != convID {
			t.Errorf("run ids = %v, want exactly [%s]", runIDs, convID)
		}
	})

	t.Run("Empty_closing_event_writes_no_audit_row", func(t *testing.T) {
		// The terminal reconciler's close: not event-driven, so there is no
		// event to record and close_event_type stays NULL.
		s, orgID, seed := mk(t)
		taskID := seed.Task(t)

		closed, _, err := s.CloseWithRunCancelIntentSystem(ctx, orgID, taskID, "reconciled_terminal_entity", "", "")
		if err != nil {
			t.Fatalf("CloseWithRunCancelIntentSystem: %v", err)
		}
		if !closed {
			t.Error("closed = false, want true")
		}
		if n := seed.CloseAuditCount(t, taskID); n != 0 {
			t.Errorf("close-audit rows = %d, want 0", n)
		}
	})
}
