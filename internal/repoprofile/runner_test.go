package repoprofile

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cycleRecorder captures every cycle invocation (orgID + force) and lets a
// test block the cycle body to exercise the single-flight gate. changed is
// the value returned to the Runner (whether a row was written); it defaults
// to false, so tests exercising onCycleComplete set it true.
type cycleRecorder struct {
	mu      sync.Mutex
	calls   []cycleCall
	changed bool
	gate    chan struct{} // if non-nil, each cycle blocks on a receive
	started chan struct{} // if non-nil, each cycle signals on entry
}

type cycleCall struct {
	orgID string
	force bool
}

func (c *cycleRecorder) fn(_ context.Context, orgID string, force bool) (bool, error) {
	if c.started != nil {
		c.started <- struct{}{}
	}
	if c.gate != nil {
		<-c.gate
	}
	c.mu.Lock()
	c.calls = append(c.calls, cycleCall{orgID: orgID, force: force})
	changed := c.changed
	c.mu.Unlock()
	return changed, nil
}

func (c *cycleRecorder) snapshot() []cycleCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cycleCall(nil), c.calls...)
}

// waitForCalls polls until at least n cycle calls are recorded or the
// deadline elapses, returning the snapshot. Avoids a fixed sleep.
func waitForCalls(rec *cycleRecorder, n int, d time.Duration) []cycleCall {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := rec.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	return rec.snapshot()
}

// TestRunner_ForcePropagates pins that a force=true trigger reaches the
// cycle as force=true (re-profile button bypassing the TTL) and a plain
// trigger as force=false (the per-poll TTL-gated pass).
func TestRunner_ForcePropagates(t *testing.T) {
	for _, force := range []bool{true, false} {
		rec := &cycleRecorder{}
		r := newRunner(rec.fn, "org-1", nil)
		r.Start()
		defer r.Stop()

		r.Trigger(force)

		got := waitForCalls(rec, 1, time.Second)
		if len(got) != 1 {
			t.Fatalf("force=%v: got %d cycles; want 1", force, len(got))
		}
		if got[0].force != force {
			t.Errorf("force=%v: cycle saw force=%v", force, got[0].force)
		}
		if got[0].orgID != "org-1" {
			t.Errorf("cycle saw org %q; want org-1", got[0].orgID)
		}
	}
}

// TestRunner_SingleFlightCoalesces pins that triggers arriving while a cycle
// is in flight coalesce: with one cycle blocked, a burst of triggers yields
// exactly one more cycle (the merged pending signal), not one per trigger.
func TestRunner_SingleFlightCoalesces(t *testing.T) {
	rec := &cycleRecorder{gate: make(chan struct{}), started: make(chan struct{}, 1)}
	r := newRunner(rec.fn, "org-1", nil)
	r.Start()
	defer r.Stop()

	// Kick the first cycle and wait until it's executing (blocked on the gate).
	r.Trigger(false)
	<-rec.started

	// Burst of triggers while the first cycle is blocked. The buffered
	// trigger channel holds at most one, so these collapse to a single
	// pending run.
	for i := 0; i < 10; i++ {
		r.Trigger(false)
	}

	// Release the first cycle; it records, then the loop picks up the single
	// coalesced pending trigger and runs a second cycle (which also needs a
	// gate token).
	rec.gate <- struct{}{} // unblock cycle 1
	<-rec.started          // cycle 2 entered
	rec.gate <- struct{}{} // unblock cycle 2

	got := waitForCalls(rec, 2, time.Second)
	if len(got) != 2 {
		t.Fatalf("got %d cycles for 11 triggers; want exactly 2 (single-flight coalescing)", len(got))
	}
}

// TestRunner_ForceStickyAcrossCoalesce pins that a force trigger landing
// while a non-forced cycle is in flight is not downgraded: the coalesced
// follow-up cycle runs with force=true.
func TestRunner_ForceStickyAcrossCoalesce(t *testing.T) {
	rec := &cycleRecorder{gate: make(chan struct{}), started: make(chan struct{}, 1)}
	r := newRunner(rec.fn, "org-1", nil)
	r.Start()
	defer r.Stop()

	r.Trigger(false)
	<-rec.started // cycle 1 (force=false) executing

	r.Trigger(true) // force arrives mid-cycle; must stick to the next run

	rec.gate <- struct{}{} // finish cycle 1
	<-rec.started          // cycle 2 entered
	rec.gate <- struct{}{} // finish cycle 2

	got := waitForCalls(rec, 2, time.Second)
	if len(got) != 2 {
		t.Fatalf("got %d cycles; want 2", len(got))
	}
	if got[0].force {
		t.Errorf("cycle 1 force=true; want false")
	}
	if !got[1].force {
		t.Errorf("cycle 2 force=false; want true (force must survive coalescing)")
	}
}

// TestRunner_OnCycleCompleteFires pins that the post-cycle hook (the
// bare-clone bootstrap chain) fires with the org when the cycle wrote a row.
func TestRunner_OnCycleCompleteFires(t *testing.T) {
	rec := &cycleRecorder{changed: true}
	done := make(chan string, 1)
	r := newRunner(rec.fn, "org-1", func(orgID string) {
		select {
		case done <- orgID:
		default:
		}
	})
	r.Start()
	defer r.Stop()

	r.Trigger(false)

	select {
	case orgID := <-done:
		if orgID != "org-1" {
			t.Errorf("onCycleComplete org = %q; want org-1", orgID)
		}
	case <-time.After(time.Second):
		t.Fatal("onCycleComplete did not fire")
	}
}

// TestRunner_OnCycleCompleteSkippedWhenNoChange pins that a no-op cycle
// (changed=false, the steady-state TTL-skip case) does NOT fire the
// post-cycle bootstrap — re-warming clones that didn't change would be
// wasted work and spurious WS broadcasts.
func TestRunner_OnCycleCompleteSkippedWhenNoChange(t *testing.T) {
	rec := &cycleRecorder{changed: false}
	var completed int32
	r := newRunner(rec.fn, "org-1", func(string) {
		atomic.AddInt32(&completed, 1)
	})
	r.Start()
	defer r.Stop()

	r.Trigger(false)

	// Wait until the cycle has run, then confirm the hook never fired.
	if got := waitForCalls(rec, 1, time.Second); len(got) != 1 {
		t.Fatalf("got %d cycles; want 1", len(got))
	}
	time.Sleep(20 * time.Millisecond) // give any erroneous async hook a chance
	if n := atomic.LoadInt32(&completed); n != 0 {
		t.Errorf("onCycleComplete fired %d times on a no-change cycle; want 0", n)
	}
}
