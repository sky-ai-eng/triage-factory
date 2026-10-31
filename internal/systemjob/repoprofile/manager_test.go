package repoprofile

import (
	"sync"
	"testing"
	"time"
)

// newTestManager builds a Manager whose per-org Runners drive the supplied
// cycle function instead of the real GitHub-backed Profiler.RunOrg. It
// bypasses NewManager (which would construct a Profiler) so the per-org
// trigger/lazy-create/single-flight machinery can be tested in isolation.
func newTestManager(cycle cycleFunc) *Manager {
	return &Manager{
		profiler: nil,
		runners:  make(map[string]*Runner),
		// onCycleComplete left nil; set by tests that need it.
		newRunnerFor: func(orgID string, onComplete func(string)) *Runner {
			return newRunner(cycle, orgID, onComplete)
		},
	}
}

// TestManager_LazyPerOrgRunners pins that Trigger lazy-creates one Runner per
// org and that distinct orgs each get a cycle — the per-org split that keeps
// a slow tenant from head-of-line-blocking others.
func TestManager_LazyPerOrgRunners(t *testing.T) {
	rec := &cycleRecorder{}
	m := newTestManager(rec.fn)
	defer m.Stop()

	m.Trigger("org-a", false)
	m.Trigger("org-b", true)

	got := waitForCalls(rec, 2, time.Second)
	if len(got) != 2 {
		t.Fatalf("got %d cycles; want 2 (one per org)", len(got))
	}
	byOrg := map[string]bool{}
	for _, c := range got {
		byOrg[c.orgID] = c.force
	}
	if force, ok := byOrg["org-a"]; !ok || force {
		t.Errorf("org-a cycle: got force=%v ok=%v; want force=false present", force, ok)
	}
	if force, ok := byOrg["org-b"]; !ok || !force {
		t.Errorf("org-b cycle: got force=%v ok=%v; want force=true present", force, ok)
	}

	m.mu.Lock()
	n := len(m.runners)
	m.mu.Unlock()
	if n != 2 {
		t.Errorf("manager holds %d runners; want 2 (one per distinct org)", n)
	}
}

// TestManager_EmptyOrgDropped pins that an empty orgID is dropped rather than
// routed to a runner — it points at an emitter bug we'd rather surface.
func TestManager_EmptyOrgDropped(t *testing.T) {
	rec := &cycleRecorder{}
	m := newTestManager(rec.fn)
	defer m.Stop()

	m.Trigger("", false)

	// Give any erroneous cycle a chance to run.
	time.Sleep(20 * time.Millisecond)
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("empty org produced %d cycles; want 0", len(got))
	}
	m.mu.Lock()
	n := len(m.runners)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("manager lazy-created %d runners for empty org; want 0", n)
	}
}

// TestManager_StopIsNoOpAfterwards pins that Trigger is a no-op after Stop —
// no new runner is created and no cycle fires.
func TestManager_StopIsNoOpAfterwards(t *testing.T) {
	rec := &cycleRecorder{}
	m := newTestManager(rec.fn)
	m.Stop()

	m.Trigger("org-a", true)
	time.Sleep(20 * time.Millisecond)

	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("got %d cycles after Stop; want 0", len(got))
	}
	m.mu.Lock()
	n := len(m.runners)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("manager created %d runners after Stop; want 0", n)
	}
}

// TestManager_OnCycleCompleteWired pins that the Manager threads its
// onCycleComplete callback into each Runner it creates.
func TestManager_OnCycleCompleteWired(t *testing.T) {
	rec := &cycleRecorder{changed: true}
	var mu sync.Mutex
	var seen []string
	m := newTestManager(rec.fn)
	m.SetOnCycleComplete(func(orgID string) {
		mu.Lock()
		seen = append(seen, orgID)
		mu.Unlock()
	})
	defer m.Stop()

	m.Trigger("org-a", false)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "org-a" {
		t.Fatalf("onCycleComplete saw %v; want [org-a]", seen)
	}
}
