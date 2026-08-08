package routing

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// queueRowStatus reads one named queue row's status — the by-id sibling of
// queueRow, for the cases below that have more than one row in the table.
func queueRowStatus(t *testing.T, database *sql.DB, id int64) string {
	t.Helper()
	var status string
	if err := database.QueryRow(`SELECT status FROM event_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("read queue row %d: %v", id, err)
	}
	return status
}

// TestParkedEvent_ListedThenRequeued_RoutesExactlyOnce is the end-to-end
// contract behind the operator surface: an outage that outlasts the retry
// budget parks the event, the parked row is what an operator sees (with the
// reason it gave up), and requeueing it — once the dependency is back — routes
// the event that was dropped, exactly once.
//
// The two halves matter for different reasons. The park half is the reason the
// surface exists at all: nothing re-drives a parked row, because the tracker's
// snapshot advanced when the event was minted and the transition will not
// re-emit. The requeue half is the reason it is safe: the replay runs the FULL
// routing pass again, and the tasks dedup index is what keeps that from
// minting a second task.
func TestParkedEvent_ListedThenRequeued_RoutesExactlyOnce(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	// An outage exactly as long as the retry budget: every attempt fails, so
	// the last one parks the row instead of requeueing it again.
	o := &outage{remaining: maxEventAttempts}
	r.tasks = outageTaskStore{TaskStore: testTaskStore(database), o: o}

	entity, _, err := st.Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "owner/repo#parked", "pr", "Parked PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	enqueueCIFailed(t, database, entity.ID)

	// Burn the budget. Each pass claims the row, fails the task upsert, and
	// requeues — drainEventQueue returns after a requeue so the retry is the
	// next scan tick's, which is why this is a loop and not one call.
	for i := 0; i < maxEventAttempts; i++ {
		if err := r.drainEventQueue(context.Background()); err != nil {
			t.Fatalf("drainEventQueue attempt %d: %v", i+1, err)
		}
	}
	status, attempts, _ := queueRow(t, database)
	if status != domain.QueuedEventStatusFailed {
		t.Fatalf("row after %d failed attempts = %q, want failed", maxEventAttempts, status)
	}
	if attempts != maxEventAttempts {
		t.Errorf("attempts = %d, want %d", attempts, maxEventAttempts)
	}
	if n := activeTaskCount(t, database, entity.ID); n != 0 {
		t.Fatalf("tasks after the park = %d, want 0 — the event's routing never ran", n)
	}

	// What the operator sees.
	parked, err := st.EventQueue.ListFailedEvents(t.Context(), runmode.LocalDefaultOrgID, 0)
	if err != nil {
		t.Fatalf("ListFailedEvents: %v", err)
	}
	if len(parked) != 1 {
		t.Fatalf("parked rows = %d, want 1", len(parked))
	}
	if parked[0].EventType != domain.EventGitHubPRCICheckFailed {
		t.Errorf("event_type = %q, want %q", parked[0].EventType, domain.EventGitHubPRCICheckFailed)
	}
	if parked[0].EntityID != entity.ID || parked[0].EntitySourceID != "owner/repo#parked" {
		t.Errorf("entity = {%q %q}, want the PR the event was about", parked[0].EntityID, parked[0].EntitySourceID)
	}
	// The reason has to name the failure AND the exhausted budget: "it kept
	// failing" and "it stopped trying" are different things to an operator.
	if !strings.Contains(parked[0].LastError, errOutage.Error()) {
		t.Errorf("last_error = %q, want the outage that parked it", parked[0].LastError)
	}
	if !strings.Contains(parked[0].LastError, "after 5 attempts") {
		t.Errorf("last_error = %q, want the exhausted-budget note", parked[0].LastError)
	}
	if parked[0].Attempts != maxEventAttempts {
		t.Errorf("attempts = %d, want %d", parked[0].Attempts, maxEventAttempts)
	}

	// The dependency is back (the outage's budget is spent) and the operator
	// requeues.
	n, err := st.EventQueue.RequeueFailedEvents(t.Context(), runmode.LocalDefaultOrgID, []int64{parked[0].ID})
	if err != nil {
		t.Fatalf("RequeueFailedEvents: %v", err)
	}
	if n != 1 {
		t.Fatalf("RequeueFailedEvents moved %d rows, want 1", n)
	}

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after requeue: %v", err)
	}
	status, attempts, _ = queueRow(t, database)
	if status != domain.QueuedEventStatusDone {
		t.Errorf("row after the requeued pass = %q, want done", status)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 — the requeue granted a fresh budget", attempts)
	}
	if n := activeTaskCount(t, database, entity.ID); n != 1 {
		t.Errorf("tasks after the requeued pass = %d, want exactly 1", n)
	}
	if left, _ := st.EventQueue.ListFailedEvents(t.Context(), runmode.LocalDefaultOrgID, 0); len(left) != 0 {
		t.Errorf("parked rows after recovery = %d, want 0", len(left))
	}
}

// TestParkedEvent_RequeueAfterTaskArrivedByOtherMeans pins the convergence
// claim the handler makes: requeueing an event whose task has since arrived
// another way is a no-op, not a duplicate. It is the case an operator is most
// likely to hit — they requeue a batch without knowing which rows the system
// has already covered.
func TestParkedEvent_RequeueAfterTaskArrivedByOtherMeans(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	o := &outage{remaining: maxEventAttempts}
	r.tasks = outageTaskStore{TaskStore: testTaskStore(database), o: o}

	entity, _, err := st.Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "owner/repo#converge", "pr", "Converging PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	enqueueCIFailed(t, database, entity.ID)
	for i := 0; i < maxEventAttempts; i++ {
		if err := r.drainEventQueue(context.Background()); err != nil {
			t.Fatalf("drainEventQueue attempt %d: %v", i+1, err)
		}
	}
	parked, _ := st.EventQueue.ListFailedEvents(t.Context(), runmode.LocalDefaultOrgID, 0)
	if len(parked) != 1 {
		t.Fatalf("parked rows = %d, want 1", len(parked))
	}

	// A later event of the same type on the same entity routes normally and
	// mints the task the parked one would have.
	enqueueCIFailed(t, database, entity.ID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue for the later event: %v", err)
	}
	if n := activeTaskCount(t, database, entity.ID); n != 1 {
		t.Fatalf("tasks after the later event = %d, want 1", n)
	}

	// Requeueing the parked row now replays a pass whose work is already done.
	if n, err := st.EventQueue.RequeueFailedEvents(t.Context(), runmode.LocalDefaultOrgID, []int64{parked[0].ID}); err != nil || n != 1 {
		t.Fatalf("RequeueFailedEvents: n=%d err=%v", n, err)
	}
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after requeue: %v", err)
	}
	if status := queueRowStatus(t, database, parked[0].ID); status != domain.QueuedEventStatusDone {
		t.Errorf("requeued row = %q, want done", status)
	}
	if n := activeTaskCount(t, database, entity.ID); n != 1 {
		t.Errorf("tasks after the redundant replay = %d, want still exactly 1 (the dedup index absorbs it)", n)
	}
}
