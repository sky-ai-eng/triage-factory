package routing

import (
	"context"
	"database/sql"
	"testing"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// hookedEventStore runs a side effect at the moment the drain worker loads
// a claimed row's event — the narrowest place to stand in for "a lease
// demotion landed while this row was mid-flight," since it is inside the
// unit and before any of its writes.
type hookedEventStore struct {
	dbpkg.EventStore
	onLoad func()
}

func (h hookedEventStore) GetSystem(ctx context.Context, orgID, eventID string) (*domain.Event, error) {
	if h.onLoad != nil {
		h.onLoad()
	}
	return h.EventStore.GetSystem(ctx, orgID, eventID)
}

// panicOnLoadEventStore panics where a routing bug would: inside the
// claimed unit, after the row is already 'processing'.
type panicOnLoadEventStore struct{ dbpkg.EventStore }

func (panicOnLoadEventStore) GetSystem(context.Context, string, string) (*domain.Event, error) {
	panic("routing exploded")
}

// seedTwoQueuedEvents enqueues a ci_check_failed event for each of two
// fresh entities and returns their ids, so a test can assert on the row
// the worker claimed AND on the one it must not reach.
func seedTwoQueuedEvents(t *testing.T, database *sql.DB) (first, second string) {
	t.Helper()
	st := sqlitestore.New(database)
	ids := make([]string, 0, 2)
	for _, key := range []string{"owner/repo#unit-1", "owner/repo#unit-2"} {
		entity, _, err := st.Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
			"github", key, "pr", "PR", "https://example.com")
		if err != nil {
			t.Fatalf("create entity %s: %v", key, err)
		}
		ids = append(ids, entity.ID)
		enqueueCIFailed(t, database, entity.ID)
	}
	return ids[0], ids[1]
}

// TestProcessQueuedEvent_CancelMidUnit_FinishesTheRow is the claimed-row-
// as-unit contract: a cancellation that lands after the claim must not
// abandon the row half-routed. The whole unit — the event load, routing,
// and the terminal MarkDone — runs on a context that carries the drain's
// values but cannot be cancelled, so the row reaches 'done' and its task
// exists even though the drain context died mid-flight.
//
// The alternative (threading the cancellable ctx straight through) fails
// here on MarkDone: the routing writes would already have committed, and
// the row would sit 'processing' with nothing but a future boot of THIS
// instance to rescue it — which a demoted pod never performs.
func TestProcessQueuedEvent_CancelMidUnit_FinishesTheRow(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.events = hookedEventStore{EventStore: st.Events, onLoad: cancel}

	firstEntity, _ := seedTwoQueuedEvents(t, database)

	if err := r.drainEventQueue(ctx); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	// The cancellation is real, not a no-op the assertions below could pass
	// through: any store call on the drain's own context now fails.
	if _, err := st.Events.GetSystem(ctx, runmode.LocalDefaultOrgID, "any"); err == nil {
		t.Fatal("the drain context is still usable; this test proves nothing unless cancellation actually stops store calls")
	}

	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != 1 {
		t.Errorf("done rows = %d, want 1 — the claimed row must reach its terminal mark", done)
	}
	if stuck := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='processing'`); stuck != 0 {
		t.Errorf("%d rows left 'processing'; only a boot of this same instance would ever reset them, and a demoted pod does not boot", stuck)
	}
	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, firstEntity)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("entity %s has %d active tasks, want 1 — routing must complete, not abort", firstEntity, len(active))
	}

	// ...and the worker stops at the claim gate rather than draining on:
	// the second row is untouched, attempts still 0.
	var status string
	var attempts int
	if err := database.QueryRow(`SELECT status, attempts FROM event_queue WHERE status != 'done' ORDER BY id LIMIT 1`).Scan(&status, &attempts); err != nil {
		t.Fatalf("read the un-drained row: %v", err)
	}
	if status != domain.QueuedEventStatusPending || attempts != 0 {
		t.Errorf("next row = (%s, attempts %d), want (pending, 0) — a cancelled drain must claim nothing further", status, attempts)
	}
}

// TestDrainEventQueue_CancelBetweenRows_StopsPromptly is the other half of
// the contract: cancellation between rows stops the worker before it takes
// on any new debt. Nothing is claimed, so nothing needs finishing.
func TestDrainEventQueue_CancelBetweenRows_StopsPromptly(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	seedTwoQueuedEvents(t, database)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := r.drainEventQueue(ctx); err != nil {
		t.Fatalf("drainEventQueue on a cancelled context = %v, want nil (a clean stop is not a claim failure)", err)
	}
	if pending := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='pending' AND attempts=0`); pending != 2 {
		t.Errorf("unclaimed pending rows = %d, want 2 — an already-cancelled drain must claim nothing", pending)
	}
	if tasks := countRows(t, database, `SELECT COUNT(*) FROM tasks`); tasks != 0 {
		t.Errorf("tasks created = %d, want 0", tasks)
	}
}

// TestProcessQueuedEvent_PanicParksOrRequeues pins that the poison-pill
// guard still works now that the unit runs on a detached context: a panic
// inside the claimed unit is recovered and the row resolved — requeued
// while it has attempts left, parked 'failed' once it has burned through
// them — rather than killing the single worker goroutine and freezing the
// queue behind one bad event.
func TestProcessQueuedEvent_PanicParksOrRequeues(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)
	r.events = panicOnLoadEventStore{EventStore: st.Events}

	seedTwoQueuedEvents(t, database)

	// First attempt on the oldest row: recovered, requeued, reason recorded.
	qe, err := st.EventQueue.ClaimNext(context.Background(), "panic-instance", 1)
	if err != nil || qe == nil {
		t.Fatalf("claim: qe=%v err=%v", qe, err)
	}
	r.processQueuedEvent(context.Background(), qe)

	var status, lastErr string
	var attempts int
	if err := database.QueryRow(`SELECT status, attempts, COALESCE(last_error, '') FROM event_queue WHERE id = ?`, qe.ID).
		Scan(&status, &attempts, &lastErr); err != nil {
		t.Fatalf("read the panicked row: %v", err)
	}
	if status != domain.QueuedEventStatusPending || attempts != 1 {
		t.Errorf("row after a panic = (%s, attempts %d), want (pending, 1) — a first panic is a retry, not a park", status, attempts)
	}
	if lastErr == "" {
		t.Error("last_error is empty; a requeue after a panic must record why")
	}

	// Same row once its retry budget is spent: parked 'failed' so the
	// worker stops spinning on it.
	if _, err := database.Exec(`UPDATE event_queue SET attempts = ? WHERE id = ?`, maxEventAttempts, qe.ID); err != nil {
		t.Fatalf("age the row to its attempt budget: %v", err)
	}
	spent, err := st.EventQueue.ClaimNext(context.Background(), "panic-instance", 1)
	if err != nil || spent == nil {
		t.Fatalf("re-claim: qe=%v err=%v", spent, err)
	}
	r.processQueuedEvent(context.Background(), spent)

	if err := database.QueryRow(`SELECT status FROM event_queue WHERE id = ?`, qe.ID).Scan(&status); err != nil {
		t.Fatalf("read the parked row: %v", err)
	}
	if status != domain.QueuedEventStatusFailed {
		t.Errorf("row after exhausting its attempts = %q, want failed", status)
	}
}
