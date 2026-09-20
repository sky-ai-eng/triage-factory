package routing

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
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
// claimed unit, after the row is already leased.
type panicOnLoadEventStore struct{ dbpkg.EventStore }

func (panicOnLoadEventStore) GetSystem(context.Context, string, string) (*domain.Event, error) {
	panic("routing exploded")
}

// seedTwoQueuedEvents enqueues a ci_check_failed event for each of two
// fresh entities and returns their ids, so a test can assert on the row
// the worker claimed AND on the one it must not reach.
func seedTwoQueuedEvents(t *testing.T, database *sql.DB) (first, second string) {
	t.Helper()
	ids := make([]string, 0, 2)
	for _, key := range []string{"owner/repo#unit-1", "owner/repo#unit-2"} {
		id := newEntity(t, database, key)
		ids = append(ids, id)
		enqueueCIFailed(t, database, id)
	}
	return ids[0], ids[1]
}

// TestProcessQueuedEvent_CancelMidUnit_FinishesTheBatch is the claimed-row-
// as-unit contract: a cancellation that lands after the claim must not
// abandon a row half-routed. Every row of the batch already claimed — the
// event load, routing, and the terminal MarkDone — runs on a context that
// carries the drain's values but cannot be cancelled, so each reaches
// 'done' and its task exists even though the drain context died mid-flight.
//
// The alternative (threading the cancellable ctx straight through) fails
// here on MarkDone: the routing writes would already have committed, and
// the row would sit leased for a minute until the next claim replayed it —
// a long detour to save nothing.
func TestProcessQueuedEvent_CancelMidUnit_FinishesTheBatch(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.events = hookedEventStore{EventStore: st.Events, onLoad: cancel}

	firstEntity, secondEntity := seedTwoQueuedEvents(t, database)

	if err := r.drainEventQueue(ctx); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	// The cancellation is real, not a no-op the assertions below could pass
	// through: any store call on the drain's own context now fails.
	if _, err := st.Events.GetSystem(ctx, runmode.LocalDefaultOrgID, "any"); err == nil {
		t.Fatal("the drain context is still usable; this test proves nothing unless cancellation actually stops store calls")
	}

	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != 2 {
		t.Errorf("done rows = %d, want 2 — every row of the claimed batch must reach its terminal mark", done)
	}
	if stuck := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='leased'`); stuck != 0 {
		t.Errorf("%d rows left leased; a cancelled drain must finish the debt it already took on", stuck)
	}
	for _, entityID := range []string{firstEntity, secondEntity} {
		active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
		if err != nil {
			t.Fatalf("list active tasks: %v", err)
		}
		if len(active) != 1 {
			t.Errorf("entity %s has %d active tasks, want 1 — routing must complete, not abort", entityID, len(active))
		}
	}
}

// TestDrainEventQueue_CancelMidBatch_ClaimsNoFurtherBatch pins where the
// gate is: between batches. With one more row than a batch holds, a
// cancellation landing inside the first batch finishes that batch and never
// claims the row behind it.
func TestDrainEventQueue_CancelMidBatch_ClaimsNoFurtherBatch(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.events = hookedEventStore{EventStore: st.Events, onLoad: cancel}

	for i := 0; i <= eventClaimBatch; i++ {
		enqueueCIFailed(t, database, newEntity(t, database, fmt.Sprintf("owner/repo#batch-%d", i)))
	}
	if err := r.drainEventQueue(ctx); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != eventClaimBatch {
		t.Errorf("done rows = %d, want the first batch of %d", done, eventClaimBatch)
	}
	var status string
	var attempt int
	if err := database.QueryRow(`SELECT status, attempt FROM event_queue WHERE status != 'done' ORDER BY id LIMIT 1`).Scan(&status, &attempt); err != nil {
		t.Fatalf("read the un-drained row: %v", err)
	}
	if status != domain.QueuedEventStatusReady || attempt != 0 {
		t.Errorf("next row = (%s, attempt %d), want (ready, 0) — a cancelled drain must claim no further batch", status, attempt)
	}
}

// TestDrainEventQueue_CancelBetweenRows_StopsPromptly is the other half of
// the contract: cancellation before a claim stops the worker before it takes
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
	if ready := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='ready' AND attempt=0`); ready != 2 {
		t.Errorf("unclaimed ready rows = %d, want 2 — an already-cancelled drain must claim nothing", ready)
	}
	if tasks := countRows(t, database, `SELECT COUNT(*) FROM tasks`); tasks != 0 {
		t.Errorf("tasks created = %d, want 0", tasks)
	}
}

// TestProcessQueuedEvent_PanicRequeuesThenParks pins that the poison-pill
// guard still works now that the unit runs on a detached context: a panic
// inside the claimed unit is recovered and the row resolved under the
// poison outcome — requeued while it has budget left, parked once the
// budget is spent — rather than killing the single worker goroutine and
// freezing the queue behind one bad event.
func TestProcessQueuedEvent_PanicRequeuesThenParks(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)
	r.events = panicOnLoadEventStore{EventStore: st.Events}

	seedTwoQueuedEvents(t, database)

	// First attempt on the oldest row: recovered, requeued, reason recorded.
	claimed := claimOneRow(t, database, testExecutorID)
	r.processQueuedEvent(context.Background(), claimed)

	got := queueRowByID(t, database, claimed.Event.ID)
	if got.Status != domain.QueuedEventStatusReady || got.Attempt != 1 || got.LastOutcome != string(workitem.OutcomePoisonSuspected) {
		t.Errorf("row after a panic = %+v, want ready at attempt 1 under poison_suspected — a first panic is a retry, not a park", got)
	}
	if got.LastError == "" {
		t.Error("last_error is empty; a requeue after a panic must record why")
	}

	// Same row on its last attempt: the requeue parks it so the worker
	// stops spinning on it.
	budget := eventQueueKind.Policy.MaxAttempts
	if _, err := database.Exec(`UPDATE event_queue SET attempt = ?, next_attempt_at = NULL WHERE id = ?`, budget-1, claimed.Event.ID); err != nil {
		t.Fatalf("age the row to its last attempt: %v", err)
	}
	spent := claimOneRow(t, database, testExecutorID)
	if spent.Event.ID != claimed.Event.ID || spent.Receipt.Attempt != budget {
		t.Fatalf("re-claim = row %d attempt %d, want row %d at attempt %d", spent.Event.ID, spent.Receipt.Attempt, claimed.Event.ID, budget)
	}
	r.processQueuedEvent(context.Background(), spent)

	if got := queueRowByID(t, database, claimed.Event.ID); got.Status != domain.QueuedEventStatusParked || got.LastOutcome != string(workitem.OutcomePoisonSuspected) {
		t.Errorf("row after exhausting its attempts = %+v, want parked under poison_suspected", got)
	}
}
