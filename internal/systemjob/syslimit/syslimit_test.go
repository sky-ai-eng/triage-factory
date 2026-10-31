package syslimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLimiter_BoundsConcurrency pins the core contract: at most n runs hold a
// slot at once. We launch more goroutines than slots, each recording the live
// count on entry, and assert the observed peak never exceeds n.
func TestLimiter_BoundsConcurrency(t *testing.T) {
	const n = 3
	l := New(n)

	var (
		live    int32
		peak    int32
		wg      sync.WaitGroup
		release = make(chan struct{})
	)
	// Gate every worker on a shared release channel so they pile up against
	// the limiter rather than slipping through one at a time.
	for i := 0; i < n*4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Acquire(context.Background()); err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer l.Release()
			cur := atomic.AddInt32(&live, 1)
			for {
				old := atomic.LoadInt32(&peak)
				if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
					break
				}
			}
			<-release
			atomic.AddInt32(&live, -1)
		}()
	}

	// Let the first wave saturate the limiter, then release everyone.
	if !waitFor(time.Second, func() bool { return atomic.LoadInt32(&live) == int32(n) }) {
		t.Fatalf("limiter never reached its cap of %d (live=%d)", n, atomic.LoadInt32(&live))
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&peak); got > n {
		t.Errorf("peak concurrency = %d; want <= %d", got, n)
	}
}

// TestLimiter_AcquireReturnsCtxErr pins that Acquire honors a cancelled
// context: once the limiter is saturated, a further Acquire blocks until ctx
// is done and then returns ctx.Err() rather than hanging forever.
func TestLimiter_AcquireReturnsCtxErr(t *testing.T) {
	l := New(1)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer l.Release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := l.Acquire(ctx)
	if err != context.Canceled {
		t.Errorf("Acquire on saturated limiter = %v; want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("Acquire took %s to observe cancellation; should return promptly", time.Since(start))
	}
}

// TestLimiter_NilIsUnlimitedNoOp pins the nil-safety contract: a nil *Limiter
// acquires and releases without blocking or panicking, so call sites that want
// no cap (local mode) need no nil-check.
func TestLimiter_NilIsUnlimitedNoOp(t *testing.T) {
	var l *Limiter // nil

	for i := 0; i < 100; i++ {
		if err := l.Acquire(context.Background()); err != nil {
			t.Fatalf("nil Acquire returned %v; want nil", err)
		}
	}
	// 100 acquires with no releases would deadlock a real limiter; a nil one
	// is a no-op. Releasing more times than acquired must also be safe.
	for i := 0; i < 100; i++ {
		l.Release()
	}
}

// TestNew_NonPositiveReturnsNil pins that New(n<=0) yields the unlimited nil
// limiter rather than a zero-capacity channel that would deadlock on Acquire.
func TestNew_NonPositiveReturnsNil(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		if l := New(n); l != nil {
			t.Errorf("New(%d) = %v; want nil (unlimited)", n, l)
		}
	}
}

// TestLimiter_ReleaseWithoutAcquirePanics pins that an unmatched Release is a
// loud panic, not a silent block — a mismatched release is a caller bug we'd
// rather crash on than leak a goroutine over. (A nil limiter stays a no-op;
// only a real limiter with no outstanding Acquire panics.)
func TestLimiter_ReleaseWithoutAcquirePanics(t *testing.T) {
	l := New(2)
	defer func() {
		if recover() == nil {
			t.Errorf("Release without a prior Acquire did not panic")
		}
	}()
	l.Release()
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}
