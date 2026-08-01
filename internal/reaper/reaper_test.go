package reaper

import (
	"context"
	"sync"
	"testing"
	"time"
)

// countingStore is a Store seam that records DeleteStaleInstances calls. The
// other two methods are unused by RunRegistryGC and panic if reached, so a
// stray call surfaces as a test failure rather than a silent no-op.
type countingStore struct {
	mu        sync.Mutex
	gcCalls   int
	lastStale time.Duration
}

func (s *countingStore) ReapDeadExecutors(context.Context, time.Duration, int) (Counts, error) {
	panic("ReapDeadExecutors not expected from RunRegistryGC")
}

func (s *countingStore) DeleteStaleInstances(_ context.Context, staleAfter time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcCalls++
	s.lastStale = staleAfter
	return 0, nil
}

func (s *countingStore) CancelStrandedCuratorTurns(context.Context, time.Duration) (int, error) {
	panic("CancelStrandedCuratorTurns not expected from RunRegistryGC")
}

func (s *countingStore) HealClaimDesyncs(context.Context) (int, error) {
	panic("HealClaimDesyncs not expected from RunRegistryGC")
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
		RunRegistryGC(ctx, store, time.Hour, 7*24*time.Hour)
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
	if store.lastStale != 7*24*time.Hour {
		t.Errorf("start sweep staleAfter = %v; want the configured 7d threshold", store.lastStale)
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
		RunRegistryGC(ctx, store, time.Hour, 7*24*time.Hour)
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

// TestRunRegistryGC_NilStoreIsNoop keeps the nil-seam contract (local mode /
// TF_ROLE=executor wire no store) a logged no-op rather than a panic.
func TestRunRegistryGC_NilStoreIsNoop(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunRegistryGC(context.Background(), nil, time.Hour, time.Hour)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunRegistryGC with a nil store did not return immediately")
	}
}

// Ensure the fake actually satisfies the seam the loop consumes.
var _ Store = (*countingStore)(nil)
