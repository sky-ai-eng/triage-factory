package reachcache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The Manager's job is per-org isolation and single flight, and single flight is
// the rate-limit bound the picker depends on: N opens inside one TTL must cost
// one enumeration, not N. These drive the trigger machinery with a fake cycle so
// the coalescing is observable without a GitHub to count against.

// blockingCycle is a cycle that parks until released, so a test can hold one
// pass open and pile triggers behind it.
type blockingCycle struct {
	started chan struct{}
	release chan struct{}
	runs    atomic.Int32
	forced  atomic.Int32
}

func newBlockingCycle() *blockingCycle {
	return &blockingCycle{started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (c *blockingCycle) run(_ context.Context, _ string, force bool) (bool, error) {
	c.runs.Add(1)
	if force {
		c.forced.Add(1)
	}
	c.started <- struct{}{}
	<-c.release
	return true, nil
}

func TestManagerCoalescesTriggersIntoOneCycle(t *testing.T) {
	cycle := newBlockingCycle()
	m := managerWithCycle(cycle.run)
	t.Cleanup(m.Stop)

	m.Trigger("org-1", false)
	waitFor(t, cycle.started, "the first trigger never started a cycle")

	// Five more opens while the first pass is still in flight. The buffered
	// trigger channel holds at most one, so they merge into a single follow-up —
	// this is what turns "the picker was opened six times" into one enumeration.
	for range 5 {
		m.Trigger("org-1", false)
	}
	close(cycle.release)
	waitFor(t, cycle.started, "the merged triggers never produced a follow-up cycle")

	// Give any (incorrectly) queued extra cycles a chance to start before
	// asserting there are none.
	time.Sleep(50 * time.Millisecond)
	if got := cycle.runs.Load(); got != 2 {
		t.Errorf("%d cycles for six triggers; want 2 (the one in flight plus one merged follow-up)", got)
	}
}

func TestManagerKeepsAForcePendingAcrossACoincidentPlainTrigger(t *testing.T) {
	cycle := newBlockingCycle()
	m := managerWithCycle(cycle.run)
	t.Cleanup(m.Stop)

	// Hold a first pass open so the force and the plain trigger both land while
	// it runs and merge into the same follow-up.
	m.Trigger("org-1", false)
	waitFor(t, cycle.started, "the first trigger never started a cycle")
	m.Trigger("org-1", true)
	m.Trigger("org-1", false)
	close(cycle.release)
	waitFor(t, cycle.started, "the merged triggers never produced a follow-up cycle")

	if cycle.forced.Load() != 1 {
		t.Errorf("%d forced cycles; want 1 — a coincident TTL-gated trigger must not downgrade a pending force",
			cycle.forced.Load())
	}
}

func TestManagerRunsDistinctOrgsConcurrently(t *testing.T) {
	cycle := newBlockingCycle()
	m := managerWithCycle(cycle.run)
	t.Cleanup(m.Stop)

	// One org's slow pass must not hold another's: a 3,000-repo account behind a
	// sluggish GHES cannot be allowed to block a different tenant's picker.
	m.Trigger("org-1", false)
	waitFor(t, cycle.started, "org-1 never started")
	m.Trigger("org-2", false)
	waitFor(t, cycle.started, "org-2 was blocked behind org-1's in-flight cycle")
	close(cycle.release)
}

func TestManagerDropsAnEmptyOrg(t *testing.T) {
	var runs atomic.Int32
	m := managerWithCycle(func(context.Context, string, bool) (bool, error) {
		runs.Add(1)
		return false, nil
	})
	t.Cleanup(m.Stop)

	m.Trigger("", false)
	time.Sleep(50 * time.Millisecond)
	if runs.Load() != 0 {
		t.Error("an empty org started a cycle; it must be dropped rather than routed to a sentinel")
	}
}

func TestManagerStopIsIdempotentAndSilencesTriggers(t *testing.T) {
	var runs atomic.Int32
	m := managerWithCycle(func(context.Context, string, bool) (bool, error) {
		runs.Add(1)
		return false, nil
	})
	m.Stop()
	m.Stop() // a second Stop must not panic on an already-closed runner
	m.Trigger("org-1", true)
	time.Sleep(50 * time.Millisecond)
	if runs.Load() != 0 {
		t.Error("a trigger after Stop started a cycle")
	}
}

// managerWithCycle builds a Manager whose runners drive cycle instead of a real
// Refresher, which is the seam newRunnerFor exists for.
func managerWithCycle(cycle cycleFunc) *Manager {
	m := &Manager{runners: map[string]*Runner{}}
	m.newRunnerFor = func(orgID string) *Runner { return newRunner(cycle, orgID) }
	return m
}

func waitFor(t *testing.T, ch chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(msg)
	}
}

// Guard against a Runner that reports itself running forever after a panic-free
// cycle — the flag is what stops two concurrent enumerations of one account.
func TestRunnerClearsItsRunningFlag(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	r := newRunner(func(context.Context, string, bool) (bool, error) {
		wg.Done()
		return true, nil
	}, "org-1")
	r.Start()
	t.Cleanup(r.Stop)
	r.Trigger(false)
	wg.Wait()

	// The flag is cleared in a defer, so give the deferred write a moment before
	// reading it.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		running := r.running
		r.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the runner still reports itself running after its cycle returned")
}
