package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// TestAcquireTurnSlot_SharesDispatcherSemaphore pins that an off-queue turn
// occupies the same slot pool the dispatcher drains: with a cap of 1, a held
// turn slot blocks the next acquire until released, and the occupancy is
// visible on the shared channel — exactly what the instance heartbeat
// reports as active_runs.
func TestAcquireTurnSlot_SharesDispatcherSemaphore(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetMaxConcurrentClaims(1)

	release, err := s.AcquireTurnSlot(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if got := len(s.semaphore()); got != 1 {
		t.Fatalf("semaphore occupancy = %d, want 1 (turn slots must be heartbeat-visible)", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.AcquireTurnSlot(ctx); err == nil {
		t.Fatal("second acquire at cap succeeded; want a ctx timeout")
	}

	release()
	release() // a double release must not free a slot it doesn't hold
	if got := len(s.semaphore()); got != 0 {
		t.Fatalf("semaphore occupancy after release = %d, want 0", got)
	}

	release2, err := s.AcquireTurnSlot(context.Background())
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

// TestAcquireTurnSlot_CancelledCtxNeverHoldsSlot pins the race the select
// alone would lose ~half the time: with a free slot AND an already-done ctx,
// select picks arbitrarily, so without the post-acquire re-check a cancelled
// caller could walk away holding a slot. Looped so the pseudo-random pick is
// actually exercised on both arms.
func TestAcquireTurnSlot_CancelledCtxNeverHoldsSlot(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetMaxConcurrentClaims(1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 100; i++ {
		if _, err := s.AcquireTurnSlot(ctx); err == nil {
			t.Fatalf("iteration %d: acquire with a cancelled ctx succeeded", i)
		}
		if got := len(s.semaphore()); got != 0 {
			t.Fatalf("iteration %d: cancelled acquire left %d slot(s) held", i, got)
		}
	}
}

// TestAcquireTurnSlot_WaitsOutMemoryGate pins the gate ordering: a
// memory-gated host holds the turn without taking a slot (so a starved host
// parks with nothing held, same as the dispatcher), and admits it once a
// re-probe sees the guardrail clear.
func TestAcquireTurnSlot_WaitsOutMemoryGate(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetMaxConcurrentClaims(1)

	recovered := make(chan struct{})
	s.mu.Lock()
	s.memFloorMB = 512
	s.memAvailMB = func() int {
		select {
		case <-recovered:
			return 8192
		default:
			return 100
		}
	}
	s.turnGateRecheck = 5 * time.Millisecond
	s.mu.Unlock()

	// Gated + cancelled: the wait must end on ctx without a slot held.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := s.AcquireTurnSlot(ctx); err == nil {
		t.Fatal("gated acquire succeeded; want a ctx timeout")
	}
	if got := len(s.semaphore()); got != 0 {
		t.Fatalf("gated wait held %d slot(s), want 0", got)
	}

	// Guardrail clears → the next re-probe admits.
	close(recovered)
	admitCtx, admitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer admitCancel()
	release, err := s.AcquireTurnSlot(admitCtx)
	if err != nil {
		t.Fatalf("acquire after the gate cleared: %v", err)
	}
	release()
}
