package routing

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
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

// drainUntilParked drives a persistently failing row through its whole
// attempt budget: one pass per attempt, ripening the row's backoff between
// passes so the test spends its time on attempts rather than on waiting.
func drainUntilParked(t *testing.T, r *Router, database *sql.DB) {
	t.Helper()
	for i := 0; i < eventQueueKind.Policy.MaxAttempts; i++ {
		ripenQueue(t, database)
		if err := r.drainEventQueue(context.Background()); err != nil {
			t.Fatalf("drainEventQueue attempt %d: %v", i+1, err)
		}
	}
}

// TestParkedEvent_ListedThenRedriven_RoutesExactlyOnce is the end-to-end
// contract behind the operator surface: an outage that outlasts the retry
// budget parks the event, the parked row is what an operator sees (with the
// reason it gave up), and redriving it — once the dependency is back — routes
// the event that was dropped, exactly once.
//
// The two halves matter for different reasons. The park half is the reason the
// surface exists at all: nothing re-drives a parked row, because the tracker's
// snapshot advanced when the event was minted and the transition will not
// re-emit. The redrive half is the reason it is safe: the replay runs the FULL
// routing pass again, and the tasks dedup index is what keeps that from
// minting a second task.
func TestParkedEvent_ListedThenRedriven_RoutesExactlyOnce(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)
	budget := eventQueueKind.Policy.MaxAttempts

	// An outage exactly as long as the retry budget: every attempt fails, so
	// the last one parks the row instead of requeueing it again.
	o := &outage{remaining: budget}
	r.tasks = outageTaskStore{TaskStore: testTaskStore(database), o: o}

	entityID := newEntity(t, database, "owner/repo#parked")
	enqueueCIFailed(t, database, entityID)

	drainUntilParked(t, r, database)
	status, attempt, _ := queueRow(t, database)
	if status != domain.QueuedEventStatusParked {
		t.Fatalf("row after %d failed attempts = %q, want parked", budget, status)
	}
	if attempt != budget {
		t.Errorf("attempt = %d, want %d", attempt, budget)
	}
	if n := activeTaskCount(t, database, entityID); n != 0 {
		t.Fatalf("tasks after the park = %d, want 0 — the event's routing never ran", n)
	}

	// What the operator sees.
	parked, _, err := st.EventQueue.ListParked(t.Context(), runmode.LocalDefaultOrgID, db.ListOpts{Limit: 50})
	if err != nil {
		t.Fatalf("ListParked: %v", err)
	}
	if len(parked) != 1 {
		t.Fatalf("parked rows = %d, want 1", len(parked))
	}
	if parked[0].EventType != domain.EventGitHubPRCICheckFailed {
		t.Errorf("event_type = %q, want %q", parked[0].EventType, domain.EventGitHubPRCICheckFailed)
	}
	if parked[0].EntityID != entityID || parked[0].EntitySourceID != "owner/repo#parked" {
		t.Errorf("entity = {%q %q}, want the PR the event was about", parked[0].EntityID, parked[0].EntitySourceID)
	}
	// The row has to name the failure AND the exhausted budget: "it kept
	// failing" and "it stopped trying" are different things to an operator.
	// The cause is the last attempt's error; the budget is the attempt
	// count against the maximum, under the typed outcome that spent it.
	if !strings.Contains(parked[0].LastError, errOutage.Error()) {
		t.Errorf("last_error = %q, want the outage that parked it", parked[0].LastError)
	}
	if parked[0].Attempt != budget || parked[0].MaxAttempts != budget || parked[0].LastOutcome != string(workitem.OutcomeTransient) {
		t.Errorf("parked row = attempt %d of %d under %q, want the whole budget spent under transient", parked[0].Attempt, parked[0].MaxAttempts, parked[0].LastOutcome)
	}

	// The dependency is back (the outage's budget is spent) and the operator
	// redrives.
	n, err := st.EventQueue.Redrive(t.Context(), runmode.LocalDefaultOrgID, []int64{parked[0].ID}, "operator")
	if err != nil {
		t.Fatalf("Redrive: %v", err)
	}
	if n != 1 {
		t.Fatalf("Redrive moved %d rows, want 1", n)
	}

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after redrive: %v", err)
	}
	status, attempt, _ = queueRow(t, database)
	if status != domain.QueuedEventStatusDone {
		t.Errorf("row after the redriven pass = %q, want done", status)
	}
	if attempt != 1 {
		t.Errorf("attempt = %d, want 1 — the redrive granted a fresh budget", attempt)
	}
	if n := activeTaskCount(t, database, entityID); n != 1 {
		t.Errorf("tasks after the redriven pass = %d, want exactly 1", n)
	}
	if left, _, _ := st.EventQueue.ListParked(t.Context(), runmode.LocalDefaultOrgID, db.ListOpts{Limit: 50}); len(left) != 0 {
		t.Errorf("parked rows after recovery = %d, want 0", len(left))
	}
}

// TestParkedEvent_RedriveAfterTaskArrivedByOtherMeans pins the convergence
// claim the handler makes: redriving an event whose task has since arrived
// another way is a no-op, not a duplicate. It is the case an operator is most
// likely to hit — they redrive a batch without knowing which rows the system
// has already covered.
func TestParkedEvent_RedriveAfterTaskArrivedByOtherMeans(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	o := &outage{remaining: eventQueueKind.Policy.MaxAttempts}
	r.tasks = outageTaskStore{TaskStore: testTaskStore(database), o: o}

	entityID := newEntity(t, database, "owner/repo#converge")
	enqueueCIFailed(t, database, entityID)
	drainUntilParked(t, r, database)
	parked, _, _ := st.EventQueue.ListParked(t.Context(), runmode.LocalDefaultOrgID, db.ListOpts{Limit: 50})
	if len(parked) != 1 {
		t.Fatalf("parked rows = %d, want 1", len(parked))
	}

	// A later event of the same type on the same entity routes normally and
	// mints the task the parked one would have.
	enqueueCIFailed(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue for the later event: %v", err)
	}
	if n := activeTaskCount(t, database, entityID); n != 1 {
		t.Fatalf("tasks after the later event = %d, want 1", n)
	}

	// Redriving the parked row now replays a pass whose work is already done.
	if n, err := st.EventQueue.Redrive(t.Context(), runmode.LocalDefaultOrgID, []int64{parked[0].ID}, "operator"); err != nil || n != 1 {
		t.Fatalf("Redrive: n=%d err=%v", n, err)
	}
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after redrive: %v", err)
	}
	if status := queueRowStatus(t, database, parked[0].ID); status != domain.QueuedEventStatusDone {
		t.Errorf("redriven row = %q, want done", status)
	}
	if n := activeTaskCount(t, database, entityID); n != 1 {
		t.Errorf("tasks after the redundant replay = %d, want still exactly 1 (the dedup index absorbs it)", n)
	}
}
