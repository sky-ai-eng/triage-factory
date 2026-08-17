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
// the "system:conversation:"/"system:routing:" prefix filter, and returns promptly
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

	bus.Publish(domain.Event{OrgID: "org-1", EventType: "system:conversation:unused-for-test"})
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

// TestPruneConversationCache_EvictsOldestDownToTarget pins the actual size cap: once
// over slackLifecycleCacheMaxEntries, pruneConversationCache evicts oldest-first
// (regardless of how recently they were cached) until back at
// slackLifecycleCachePruneTarget — the bug the prior TTL-only sweep had was
// that a cache full of entries younger than the TTL was never capped at all.
func TestPruneConversationCache_EvictsOldestDownToTarget(t *testing.T) {
	withLoweredLifecycleCacheBounds(t, 5, 3)

	base := time.Now()
	convs := map[string]*conversationEntry{}
	for i := 0; i < 6; i++ {
		convs[fmt.Sprintf("run-%d", i)] = &conversationEntry{cachedAt: base.Add(time.Duration(i) * time.Minute)}
	}

	pruneConversationCache(convs)

	if len(convs) != 3 {
		t.Fatalf("len(convs) = %d, want 3 (pruned down to target)", len(convs))
	}
	for i := 0; i < 3; i++ {
		if _, ok := convs[fmt.Sprintf("run-%d", i)]; ok {
			t.Errorf("run-%d should have been evicted (one of the 3 oldest)", i)
		}
	}
	for i := 3; i < 6; i++ {
		if _, ok := convs[fmt.Sprintf("run-%d", i)]; !ok {
			t.Errorf("run-%d should have survived (one of the 3 newest)", i)
		}
	}
}

// TestPruneConversationCache_NeverEvictsLiveWorker pins that a run with a live
// worker is never evicted even when it's the OLDEST entry in the cache —
// losing it would leak the worker, since the dispatcher would no longer
// know to route that run's future sentinels to it.
func TestPruneConversationCache_NeverEvictsLiveWorker(t *testing.T) {
	withLoweredLifecycleCacheBounds(t, 2, 1)

	base := time.Now()
	convs := map[string]*conversationEntry{
		"old-with-worker": {cachedAt: base, worker: &conversationStatusWorker{}},
		"newer-idle":      {cachedAt: base.Add(1 * time.Minute)},
		"newest-idle":     {cachedAt: base.Add(2 * time.Minute)},
	}

	pruneConversationCache(convs)

	if _, ok := convs["old-with-worker"]; !ok {
		t.Error("a run with a live worker must never be evicted, even when it's the oldest entry")
	}
	if len(convs) != 1 {
		t.Errorf("len(convs) = %d, want 1 (both idle entries evicted trying to reach target=1; the worker entry can't be evicted so the loop stops there)", len(convs))
	}
}

// TestPruneConversationCache_UnderCap_NoOp pins that pruning does nothing while the
// cache is at or under the cap — the common-case fast path.
func TestPruneConversationCache_UnderCap_NoOp(t *testing.T) {
	withLoweredLifecycleCacheBounds(t, 5, 3)

	convs := map[string]*conversationEntry{
		"a": {cachedAt: time.Now()},
		"b": {cachedAt: time.Now()},
	}
	pruneConversationCache(convs)

	if len(convs) != 2 {
		t.Errorf("len(convs) = %d, want 2 (under the cap, nothing pruned)", len(convs))
	}
}

// TestPruneConversationCache_KeepsMidTeardownEntry pins that an entry whose prevDone
// hasn't closed yet — a retiring worker still mid-flight on its trailing
// setStatus("") — is never evicted, even as the oldest entry: evicting it
// would drop the run's only reference to the ordering handle, so a resume
// after eviction would rebuild the entry with a nil predecessor and skip
// the gate, silently reintroducing the stale-clear-clobbers-resume race.
// Once the teardown finishes (prevDone closes), the same entry becomes
// ordinary and a later prune may evict it.
func TestPruneConversationCache_KeepsMidTeardownEntry(t *testing.T) {
	withLoweredLifecycleCacheBounds(t, 3, 1)

	base := time.Now()
	teardown := make(chan struct{}) // open = retiring worker still mid-flight
	convs := map[string]*conversationEntry{
		"oldest-mid-teardown": {cachedAt: base, prevDone: teardown},
		"idle-1":              {cachedAt: base.Add(1 * time.Minute)},
		"idle-2":              {cachedAt: base.Add(2 * time.Minute)},
		"idle-3":              {cachedAt: base.Add(3 * time.Minute)},
	}

	pruneConversationCache(convs)

	if _, ok := convs["oldest-mid-teardown"]; !ok {
		t.Fatal("an entry with an unclosed prevDone must never be evicted, even as the oldest")
	}
	if len(convs) != 1 {
		t.Errorf("len(convs) = %d, want 1 (all three idle entries evicted; the mid-teardown one protected)", len(convs))
	}

	// Teardown completes — the entry is ordinary now; a later over-cap prune
	// evicts it like any other idle entry.
	close(teardown)
	convs["idle-4"] = &conversationEntry{cachedAt: base.Add(4 * time.Minute)}
	convs["idle-5"] = &conversationEntry{cachedAt: base.Add(5 * time.Minute)}
	convs["idle-6"] = &conversationEntry{cachedAt: base.Add(6 * time.Minute)}

	pruneConversationCache(convs)

	if _, ok := convs["oldest-mid-teardown"]; ok {
		t.Error("once prevDone has closed, the entry is evictable again — it should have gone first as the oldest")
	}
}

// TestHandleConversationStatus_CacheHit_RefreshesLRUStamp pins the LRU touch: a
// run-status sentinel for an already-cached run bumps cachedAt (via
// slackLifecycleNow), so pruneConversationCache's oldest-first eviction tracks
// sentinel activity, not just when the run was first resolved. Uses a
// non-Slack cached entry — the hit path touches no store and no Slack, so
// no pg harness or fake server is needed.
func TestHandleConversationStatus_CacheHit_RefreshesLRUStamp(t *testing.T) {
	resolvedAt := time.Now().Add(-time.Hour)
	touchedAt := time.Now()
	origNow := slackLifecycleNow
	slackLifecycleNow = func() time.Time { return touchedAt }
	t.Cleanup(func() { slackLifecycleNow = origNow })

	adapter := newLifecycleAdapter(db.Stores{}, slackHTTPClient, staticURL(""), nil)
	convs := map[string]*conversationEntry{
		"run-1": {isSlack: false, cachedAt: resolvedAt},
	}

	adapter.dispatch(context.Background(), conversationStatusEvent("org-1", "run-1", "running"), convs)

	if got := convs["run-1"].cachedAt; !got.Equal(touchedAt) {
		t.Errorf("cachedAt = %v, want the touch time %v (refreshed on cache hit)", got, touchedAt)
	}
}

// TestPreRunIndicatorText_CoversEveryClaimPhase pins the setup indicator's
// coverage to the canonical phase vocabulary. The ok=false fallthrough is
// deliberate for a status this build has never heard of, but a phase that
// exists TODAY and reaches no arm is a silent gap: the thread either sits on
// the previous phase's copy or shows nothing at all, with no error anywhere.
// That is exactly how awaiting_credentials went unrendered. Deriving the
// coverage from domain.AllClaimPhases means a phase added in Go fails here
// rather than quietly rendering nothing.
func TestPreRunIndicatorText_CoversEveryClaimPhase(t *testing.T) {
	for _, phase := range append(domain.AllClaimPhases(), domain.StatusQueued) {
		text, ok := preRunIndicatorText(phase)
		if !ok {
			t.Errorf("preRunIndicatorText(%q) = ok:false — every pre-running status needs indicator copy", phase)
			continue
		}
		if text.status == "" || text.loading == "" {
			t.Errorf("preRunIndicatorText(%q) = %+v, want both the status line and the loading text set", phase, text)
		}
	}

	// Everything else stays invisible rather than rendering guessed copy.
	for _, status := range []string{domain.StatusRunning, domain.StatusOpen, domain.StatusCompleted, "", "some_future_state"} {
		if _, ok := preRunIndicatorText(status); ok {
			t.Errorf("preRunIndicatorText(%q) = ok:true, want no setup copy for a non-setup status", status)
		}
	}
}
