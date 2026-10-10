package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// testLockSalt is an arbitrary hashtextextended salt used only by this file's
// generic acquireKeyedLock exercises — not a registered production salt (see
// advisorylock.go's salt registry), and chosen well outside that keyspace so
// it can never collide with one.
const testLockSalt int64 = 90001

// TestAcquireKeyedLock_Multi_SerializesSameKey is the pgtest acceptance
// criterion for TFAC-579 item 6: two concurrent acquireKeyedLock calls for
// the SAME key must serialize — the second blocks until the first releases —
// even though each comes from a distinct connection, mirroring two control
// pods each with their own in-process sync.Map. This is the primitive the
// github-app-registration RMW guard (and, historically, the projects RMW
// guard) is built on, so proving it here with a generic map+salt covers
// every caller without needing to drive the full app-registration handler
// (which calls out to GitHub's API).
func TestAcquireKeyedLock_Multi_SerializesSameKey(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)
	ctx := context.Background()

	// One map per simulated pod: callers sharing a map queue on its
	// in-process gate, so only separate maps reach the advisory lock.
	var podA, podB sync.Map
	const key = "same-id"

	releaseA, err := s.acquireKeyedLock(ctx, &podA, testLockSalt, key)
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}

	// B must block until A releases. Race it against a short timeout: if B
	// acquires before A releases, the lock isn't actually serializing.
	acquiredB := make(chan struct{})
	go func() {
		releaseB, err := s.acquireKeyedLock(ctx, &podB, testLockSalt, key)
		if err != nil {
			t.Errorf("acquire B: %v", err)
			return
		}
		close(acquiredB)
		releaseB()
	}()

	select {
	case <-acquiredB:
		t.Fatal("B acquired the same key while A still held it — lock isn't serializing")
	case <-time.After(200 * time.Millisecond):
		// Expected: B is still blocked.
	}

	releaseA()

	select {
	case <-acquiredB:
		// B acquired after A released — correct.
	case <-time.After(5 * time.Second):
		t.Fatal("B never acquired after A released — lock leaked")
	}
}

// TestAcquireKeyedLock_Multi_DifferentKeysDoNotBlock confirms the lock is
// keyed, not a single global mutex: two different keys must acquire
// concurrently without waiting on each other.
func TestAcquireKeyedLock_Multi_DifferentKeysDoNotBlock(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)
	ctx := context.Background()

	var mu sync.Map
	releaseA, err := s.acquireKeyedLock(ctx, &mu, testLockSalt, "id-a")
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	defer releaseA()

	done := make(chan struct{})
	go func() {
		releaseB, err := s.acquireKeyedLock(ctx, &mu, testLockSalt, "id-b")
		if err != nil {
			t.Errorf("acquire B: %v", err)
			return
		}
		releaseB()
		close(done)
	}()

	select {
	case <-done:
		// Expected: a different key acquires immediately.
	case <-time.After(5 * time.Second):
		t.Fatal("different-key acquire blocked on an unrelated key's lock")
	}
}

// TestAcquireKeyedLock_Multi_DifferentSaltsDoNotCollide confirms two distinct
// lock domains (distinct salts, same key value) never contend with each
// other — a generic caller's key must not block an unrelated
// github-app-registration lock for the "same" string value.
func TestAcquireKeyedLock_Multi_DifferentSaltsDoNotCollide(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)
	ctx := context.Background()

	var mu sync.Map
	const key = "shared-id-value"
	releaseGeneric, err := s.acquireKeyedLock(ctx, &mu, testLockSalt, key)
	if err != nil {
		t.Fatalf("acquire generic lock: %v", err)
	}
	defer releaseGeneric()

	done := make(chan struct{})
	go func() {
		releaseApp, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, key)
		if err != nil {
			t.Errorf("acquire github-app lock: %v", err)
			return
		}
		releaseApp()
		close(done)
	}()

	select {
	case <-done:
		// Expected: distinct salt, no collision.
	case <-time.After(5 * time.Second):
		t.Fatal("github-app-registration lock blocked behind the generic lock's identical key value")
	}
}

// TestAcquireKeyedLock_Local_UsesInProcessMutex confirms local mode never
// touches Postgres — it falls back to the existing per-process keyed
// mutex, and that mutex genuinely serializes concurrent callers.
func TestAcquireKeyedLock_Local_UsesInProcessMutex(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := &Server{}
	ctx := context.Background()

	var mu sync.Map
	const key = "local-key"
	var counter int
	var wg sync.WaitGroup
	const n = 20
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			release, err := s.acquireKeyedLock(ctx, &mu, testLockSalt, key)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer release()
			// Non-atomic read-increment-write: only safe if truly serialized.
			cur := counter
			counter = cur + 1
		}()
	}
	wg.Wait()
	if counter != n {
		t.Errorf("counter = %d, want %d (concurrent increments weren't serialized)", counter, n)
	}
}

// TestAcquireKeyedLock_Multi_WaitersDoNotDrainThePool pins that callers queued
// on one key hold no pool connection while they wait. The holder needs a
// second connection for the work the lock guards; were each waiter blocked on
// pg_advisory_lock over a connection of its own, a queue as long as the pool
// would leave the holder none, and nothing would move until a ctx expired.
func TestAcquireKeyedLock_Multi_WaitersDoNotDrainThePool(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)
	h.AdminDB.SetMaxOpenConns(2)
	t.Cleanup(func() { h.AdminDB.SetMaxOpenConns(0) })
	ctx := context.Background()

	var mu sync.Map
	const key = "queued-id"
	release, err := s.acquireKeyedLock(ctx, &mu, testLockSalt, key)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	waitCtx, cancelWaiters := context.WithCancel(ctx)
	var waiters sync.WaitGroup
	for i := 0; i < 5; i++ {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			if r, err := s.acquireKeyedLock(waitCtx, &mu, testLockSalt, key); err == nil {
				r()
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)

	workCtx, cancelWork := context.WithTimeout(ctx, 5*time.Second)
	defer cancelWork()
	var one int
	if err := h.AdminDB.QueryRowContext(workCtx, `SELECT 1`).Scan(&one); err != nil {
		t.Errorf("the holder could not get a connection while 5 callers waited on its key: %v", err)
	}
	cancelWaiters()
	release()
	waiters.Wait()
}

// TestAcquireKeyedLock_Local_WaitHonorsCtx pins that a local-mode waiter gives
// up when its ctx does, rather than waiting out the holder.
func TestAcquireKeyedLock_Local_WaitHonorsCtx(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := &Server{}
	var mu sync.Map
	release, err := s.acquireKeyedLock(context.Background(), &mu, testLockSalt, "held")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.acquireKeyedLock(ctx, &mu, testLockSalt, "held"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the waiter's deadline", err)
	}
}
