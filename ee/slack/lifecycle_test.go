package slack

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
)

// TestLifecycleAdapter_Run_SubscribesFilteredAndUnsubscribesOnShutdown
// exercises run() itself (the pgtest suite in lifecycle_pg_test.go calls
// dispatch directly and never goes through the real bus): it subscribes with
// the "system:run:"/"system:routing:" prefix filter, and returns promptly
// once ctx is cancelled. Publishes a matching-but-undispatched event type
// (proves the subscription + prefix filter work) and a non-matching one
// (must never reach dispatch — stores is nil here, so a mis-routed event
// would panic).
func TestLifecycleAdapter_Run_SubscribesFilteredAndUnsubscribesOnShutdown(t *testing.T) {
	bus := eventbus.New()
	adapter := newLifecycleAdapter(db.Stores{}, slackHTTPClient, staticURL(""), func() *eventbus.Bus { return bus })

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		adapter.run(ctx)
		close(runDone)
	}()
	time.Sleep(20 * time.Millisecond) // let run() reach bus.Subscribe before publishing

	bus.Publish(domain.Event{OrgID: "org-1", EventType: "system:run:unused-for-test"})
	bus.Publish(domain.Event{OrgID: "org-1", EventType: "system:poll:completed"})
	time.Sleep(20 * time.Millisecond)

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return within 2s of ctx cancellation")
	}
}

// TestLifecycleAdapter_Run_NilBus_ReturnsImmediately covers the defensive
// guard for a bus accessor that resolves to nil (shouldn't happen in
// production post-OnReady, but run() must not block forever if it does).
func TestLifecycleAdapter_Run_NilBus_ReturnsImmediately(t *testing.T) {
	adapter := newLifecycleAdapter(db.Stores{}, slackHTTPClient, staticURL(""), func() *eventbus.Bus { return nil })
	done := make(chan struct{})
	go func() {
		adapter.run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run() with a nil bus should return immediately")
	}
}

// withLoweredLifecycleCacheBounds swaps in small cache-size tunables for a
// test, restoring the originals on cleanup — mirrors socket_test.go's
// withLoweredCap convention.
func withLoweredLifecycleCacheBounds(t *testing.T, max, target int) {
	t.Helper()
	origMax, origTarget := slackLifecycleCacheMaxEntries, slackLifecycleCachePruneTarget
	slackLifecycleCacheMaxEntries, slackLifecycleCachePruneTarget = max, target
	t.Cleanup(func() {
		slackLifecycleCacheMaxEntries, slackLifecycleCachePruneTarget = origMax, origTarget
	})
}

// TestPruneRunCache_EvictsOldestDownToTarget pins the actual size cap: once
// over slackLifecycleCacheMaxEntries, pruneRunCache evicts oldest-first
// (regardless of how recently they were cached) until back at
// slackLifecycleCachePruneTarget — the bug the prior TTL-only sweep had was
// that a cache full of entries younger than the TTL was never capped at all.
func TestPruneRunCache_EvictsOldestDownToTarget(t *testing.T) {
	withLoweredLifecycleCacheBounds(t, 5, 3)

	base := time.Now()
	runs := map[string]*runEntry{}
	for i := 0; i < 6; i++ {
		runs[fmt.Sprintf("run-%d", i)] = &runEntry{cachedAt: base.Add(time.Duration(i) * time.Minute)}
	}

	pruneRunCache(runs)

	if len(runs) != 3 {
		t.Fatalf("len(runs) = %d, want 3 (pruned down to target)", len(runs))
	}
	for i := 0; i < 3; i++ {
		if _, ok := runs[fmt.Sprintf("run-%d", i)]; ok {
			t.Errorf("run-%d should have been evicted (one of the 3 oldest)", i)
		}
	}
	for i := 3; i < 6; i++ {
		if _, ok := runs[fmt.Sprintf("run-%d", i)]; !ok {
			t.Errorf("run-%d should have survived (one of the 3 newest)", i)
		}
	}
}

// TestPruneRunCache_NeverEvictsLiveWorker pins that a run with a live
// worker is never evicted even when it's the OLDEST entry in the cache —
// losing it would leak the worker, since the dispatcher would no longer
// know to route that run's future sentinels to it.
func TestPruneRunCache_NeverEvictsLiveWorker(t *testing.T) {
	withLoweredLifecycleCacheBounds(t, 2, 1)

	base := time.Now()
	runs := map[string]*runEntry{
		"old-with-worker": {cachedAt: base, worker: &runStatusWorker{}},
		"newer-idle":      {cachedAt: base.Add(1 * time.Minute)},
		"newest-idle":     {cachedAt: base.Add(2 * time.Minute)},
	}

	pruneRunCache(runs)

	if _, ok := runs["old-with-worker"]; !ok {
		t.Error("a run with a live worker must never be evicted, even when it's the oldest entry")
	}
	if len(runs) != 1 {
		t.Errorf("len(runs) = %d, want 1 (both idle entries evicted trying to reach target=1; the worker entry can't be evicted so the loop stops there)", len(runs))
	}
}

// TestPruneRunCache_UnderCap_NoOp pins that pruning does nothing while the
// cache is at or under the cap — the common-case fast path.
func TestPruneRunCache_UnderCap_NoOp(t *testing.T) {
	withLoweredLifecycleCacheBounds(t, 5, 3)

	runs := map[string]*runEntry{
		"a": {cachedAt: time.Now()},
		"b": {cachedAt: time.Now()},
	}
	pruneRunCache(runs)

	if len(runs) != 2 {
		t.Errorf("len(runs) = %d, want 2 (under the cap, nothing pruned)", len(runs))
	}
}
