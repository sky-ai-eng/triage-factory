package delegate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// recoveryOrder is the order one recovery pass makes its store calls in: this
// executor's own expired claims, everyone else's, the settlement, and the
// stranded-run replay.
var recoveryOrder = []string{"release_own", "takeover", "settle", "stranded"}

// recoveryRecordingQueue records the store call that starts each step of the
// recovery pass, in the order they are made.
type recoveryRecordingQueue struct {
	db.ConversationQueueStore

	mu    sync.Mutex
	calls []string
}

func (q *recoveryRecordingQueue) record(step string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls = append(q.calls, step)
}

func (q *recoveryRecordingQueue) recorded() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.calls...)
}

func (q *recoveryRecordingQueue) ExpiredClaimsOfExecutorSystem(ctx context.Context, executorID string, bootEpoch int64) ([]db.ClaimRef, error) {
	q.record("release_own")
	return q.ConversationQueueStore.ExpiredClaimsOfExecutorSystem(ctx, executorID, bootEpoch)
}

func (q *recoveryRecordingQueue) TakeOverExpiredClaimsSystem(ctx context.Context, executorID string, bootEpoch int64, limit int) ([]db.ClaimRef, error) {
	q.record("takeover")
	return q.ConversationQueueStore.TakeOverExpiredClaimsSystem(ctx, executorID, bootEpoch, limit)
}

func (q *recoveryRecordingQueue) SettleUnclaimedStopsSystem(ctx context.Context) ([]db.SettledStop, error) {
	q.record("settle")
	return q.ConversationQueueStore.SettleUnclaimedStopsSystem(ctx)
}

func (q *recoveryRecordingQueue) StrandedBlueprintRunsSystem(ctx context.Context, grace time.Duration, limit int) ([]db.StrandedRun, error) {
	q.record("stranded")
	return q.ConversationQueueStore.StrandedBlueprintRunsSystem(ctx, grace, limit)
}

// passes counts the complete recovery passes recorded, failing the test if
// any pass made its calls out of order.
func (q *recoveryRecordingQueue) passes(t *testing.T) int {
	t.Helper()
	calls := q.recorded()
	for i, step := range calls {
		if want := recoveryOrder[i%len(recoveryOrder)]; step != want {
			t.Fatalf("recovery call %d = %s, want %s; the calls so far: %v", i, step, want, calls)
		}
	}
	return len(calls) / len(recoveryOrder)
}

// TestRunDispatcher_RecoveryRunsEveryScanTickWhateverGatesTheClaims: the
// recovery pass runs on every scan tick however the claim loop is held up. A
// saturated executor is the case that matters: its claim loop waits for a free
// slot for as long as its engagements run, and a fleet in which every executor
// is full would otherwise never take over a dead executor's claims or settle a
// stop.
func TestRunDispatcher_RecoveryRunsEveryScanTickWhateverGatesTheClaims(t *testing.T) {
	const scanInterval = 10 * time.Millisecond

	run := func(t *testing.T, gate func(s *Spawner)) {
		t.Helper()
		database := newDelegateTestDB(t)
		stores := testSpawnerStores(database)
		queue := &recoveryRecordingQueue{ConversationQueueStore: stores.ConversationQueue}
		stores.ConversationQueue = queue
		s := NewSpawner(database, stores, nil, nil, "")
		s.SetExecutorID("exec-recovery-loop", 1)
		gate(s)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); s.RunDispatcher(ctx, scanInterval) }()
		t.Cleanup(func() { cancel(); <-done })

		waitUntil(t, 5*time.Second, "three recovery passes", func() bool { return queue.passes(t) >= 3 })
	}

	t.Run("every_slot_held", func(t *testing.T) {
		run(t, func(s *Spawner) {
			s.SetMaxConcurrentClaims(1)
			// An engagement that does not finish holds the only slot.
			s.semaphore() <- struct{}{}
		})
	})

	// None of the claim loop's gates gates recovery: each pass releases,
	// settles or replays work no slot and no fresh claim is needed for, and a
	// fenced or draining executor's own expired claims are released by no one
	// else short of a takeover a lease later.
	t.Run("draining_fenced_and_memory_gated", func(t *testing.T) {
		run(t, func(s *Spawner) {
			s.SetDraining(true)
			s.identityFenced.Store(true)
			s.partitionFenced.Store(true)
			s.mu.Lock()
			s.memFloorMB = 1 << 20
			s.memAvailMB = func() int { return 1 }
			s.mu.Unlock()
		})
	})
}
