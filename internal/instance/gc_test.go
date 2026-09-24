package instance

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// countingStore records DeleteStaleSystem calls. Every other InstanceStore
// method is the nil embedded interface, so a stray call panics and surfaces as
// a test failure rather than a silent no-op.
type countingStore struct {
	db.InstanceStore

	mu        sync.Mutex
	gcCalls   int
	lastStale time.Duration
}

func (s *countingStore) DeleteStaleSystem(_ context.Context, olderThan time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcCalls++
	s.lastStale = olderThan
	return 0, nil
}

func (s *countingStore) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gcCalls
}

// TestRunRegistryGC_SweepsAtStart pins the at-brain-start sweep: a leader that
// only just began holding the brain must GC the registry immediately, not wait
// out the (day-long) interval for the first tick. A huge interval guarantees
// the ticker never fires within the test, so any sweep we observe is the
// immediate one.
func TestRunRegistryGC_SweepsAtStart(t *testing.T) {
	store := &countingStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunRegistryGC(ctx, store, RegistryGCStaleAfter, time.Hour)
	}()

	deadline := time.After(2 * time.Second)
	for store.calls() == 0 {
		select {
		case <-deadline:
			t.Fatal("RunRegistryGC did not sweep at start within 2s")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if got := store.calls(); got != 1 {
		t.Fatalf("start sweep count = %d; want exactly 1 before the interval elapses", got)
	}
	if store.lastStale != RegistryGCStaleAfter {
		t.Errorf("start sweep olderThan = %v; want the %v threshold", store.lastStale, RegistryGCStaleAfter)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunRegistryGC did not return after ctx cancel")
	}
}

// TestRunRegistryGC_CancelledBeforeStartSkipsSweep guards the shutdown race:
// if the brain is torn down the instant the GC starts, the immediate sweep
// must be skipped rather than firing a doomed DELETE (and a spurious warning)
// against an already-cancelled context.
func TestRunRegistryGC_CancelledBeforeStartSkipsSweep(t *testing.T) {
	store := &countingStore{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunRegistryGC(ctx, store, RegistryGCStaleAfter, time.Hour)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunRegistryGC did not return promptly on a pre-cancelled ctx")
	}
	if got := store.calls(); got != 0 {
		t.Errorf("swept %d time(s) on a pre-cancelled ctx; want 0", got)
	}
}

// TestRunRegistryGC_NilStoreIsNoop keeps the nil-seam contract a logged no-op
// rather than a panic.
func TestRunRegistryGC_NilStoreIsNoop(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunRegistryGC(context.Background(), nil, RegistryGCStaleAfter, time.Hour)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunRegistryGC with a nil store did not return immediately")
	}
}
