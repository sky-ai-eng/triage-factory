package marketplacestats

import (
	"testing"
	"time"
)

// TestManager_LazyPerOrgRunners pins that Trigger lazy-creates one Runner
// per org and that distinct orgs each recompute independently — the
// per-org split that keeps a slow tenant from head-of-line-blocking
// another's stats freshness.
func TestManager_LazyPerOrgRunners(t *testing.T) {
	store := &fakeStore{}
	m := NewManager(NewAggregator(store))
	defer m.Stop()

	m.Trigger("org-a")
	m.Trigger("org-b")

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if store.callCount() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := store.callCount(); got != 2 {
		t.Fatalf("store called %d times; want 2 (one recompute per org)", got)
	}

	m.mu.Lock()
	n := len(m.runners)
	m.mu.Unlock()
	if n != 2 {
		t.Errorf("manager holds %d runners; want 2 (one per distinct org)", n)
	}
}

// TestManager_EmptyOrgDropped pins that an empty orgID never reaches a
// runner — it points at an emitter bug worth surfacing, not the local
// sentinel.
func TestManager_EmptyOrgDropped(t *testing.T) {
	store := &fakeStore{}
	m := NewManager(NewAggregator(store))
	defer m.Stop()

	m.Trigger("")

	time.Sleep(20 * time.Millisecond)
	if got := store.callCount(); got != 0 {
		t.Fatalf("empty org produced %d recomputes; want 0", got)
	}
	m.mu.Lock()
	n := len(m.runners)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("manager lazy-created %d runners for empty org; want 0", n)
	}
}

// TestManager_StopIsNoOpAfterwards pins that Trigger after Stop never
// reaches the aggregator.
func TestManager_StopIsNoOpAfterwards(t *testing.T) {
	store := &fakeStore{}
	m := NewManager(NewAggregator(store))

	m.Stop()
	m.Trigger("org-a")

	time.Sleep(20 * time.Millisecond)
	if got := store.callCount(); got != 0 {
		t.Fatalf("post-Stop trigger produced %d recomputes; want 0", got)
	}
}

// TestManager_TriggerCoalescesWithinOneRunner pins single-flight: several
// rapid triggers for the same org before its first cycle starts collapse
// into the buffered channel, so the aggregator's own TTL gate (not runner
// stacking) is what determines recompute count. The final state should be
// "recomputed at least once, runner count still 1".
func TestManager_TriggerCoalescesWithinOneRunner(t *testing.T) {
	store := &fakeStore{}
	m := NewManager(NewAggregator(store))
	defer m.Stop()

	for i := 0; i < 5; i++ {
		m.Trigger("org-a")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if store.callCount() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := store.callCount(); got < 1 {
		t.Fatalf("store called %d times; want at least 1", got)
	}

	m.mu.Lock()
	n := len(m.runners)
	m.mu.Unlock()
	if n != 1 {
		t.Errorf("manager holds %d runners for one org triggered repeatedly; want 1", n)
	}
}
