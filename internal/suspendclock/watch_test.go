package suspendclock

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a suspend reading a test moves by hand. reads counts the
// readings taken, so a test can wait until Watch has certainly seen a value
// rather than sleeping and hoping.
type fakeClock struct {
	suspended atomic.Int64
	reads     atomic.Int64
	ok        bool
}

func installFake(t *testing.T, start time.Duration, ok bool) *fakeClock {
	t.Helper()
	c := &fakeClock{ok: ok}
	c.suspended.Store(int64(start))
	SetSourceForTest(t, func() (time.Duration, bool) {
		d := time.Duration(c.suspended.Load())
		c.reads.Add(1)
		return d, c.ok
	})
	return c
}

// awaitReads blocks until the source has been read n more times than when it
// was called. Three reads after a change guarantee Watch has acted on it: the
// first may have loaded the value before the change, the second loaded it
// after, and the third starts only once the second's fn call has returned.
func (c *fakeClock) awaitReads(t *testing.T, n int64) {
	t.Helper()
	target := c.reads.Load() + n
	deadline := time.Now().Add(5 * time.Second)
	for c.reads.Load() < target {
		if time.Now().After(deadline) {
			t.Fatalf("Watch took fewer than %d readings in 5s", n)
		}
		time.Sleep(time.Millisecond)
	}
}

// startWatch runs Watch on a fast interval and returns the channel its fn
// reports on and a channel closed when it returns. The ctx is cancelled and
// the return awaited on cleanup, so a test never leaks the goroutine into the
// next one's fake.
func startWatch(t *testing.T, threshold time.Duration) (<-chan time.Duration, <-chan struct{}, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan time.Duration, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watch(ctx, time.Millisecond, threshold, func(slept time.Duration) { calls <- slept })
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return calls, done, cancel
}

func TestWatch_ReportsAdvanceAtThreshold(t *testing.T) {
	clock := installFake(t, 10*time.Second, true)
	calls, _, _ := startWatch(t, time.Second)
	clock.awaitReads(t, 1) // the base reading

	clock.suspended.Add(int64(2 * time.Second))
	clock.awaitReads(t, 3)
	select {
	case slept := <-calls:
		if slept != 2*time.Second {
			t.Errorf("fn got slept = %s, want the 2s advance", slept)
		}
	default:
		t.Fatal("fn was not called after the reading advanced past the threshold")
	}

	clock.awaitReads(t, 3)
	if len(calls) != 0 {
		t.Errorf("fn called %d more times with the reading unchanged, want once in all", len(calls))
	}
}

func TestWatch_IgnoresAdvanceBelowThreshold(t *testing.T) {
	clock := installFake(t, 10*time.Second, true)
	calls, _, _ := startWatch(t, time.Second)
	clock.awaitReads(t, 1)

	clock.suspended.Add(int64(500 * time.Millisecond))
	clock.awaitReads(t, 3)
	if len(calls) != 0 {
		t.Errorf("fn called on a %s advance under a 1s threshold", <-calls)
	}
}

func TestWatch_ReturnsAtOnceWhenUnsupported(t *testing.T) {
	installFake(t, 0, false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watch(context.Background(), time.Millisecond, time.Second, func(time.Duration) {
			t.Error("fn called on a platform that cannot report suspended time")
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return with ok=false")
	}
}

func TestWatch_StopsOnCancel(t *testing.T) {
	clock := installFake(t, 0, true)
	_, done, cancel := startWatch(t, time.Second)
	clock.awaitReads(t, 2)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after its ctx was cancelled")
	}
}
