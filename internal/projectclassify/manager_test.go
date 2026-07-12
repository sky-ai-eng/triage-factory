package projectclassify

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestManager_TriggerLazyCreatesPerOrgRunner pins the lazy-creation contract:
// triggering an unseen orgID instantiates a Runner for that org and only that
// org. A regression that hardcoded the local sentinel — or that fanned a
// trigger to every existing runner — would either drop multi-org work or burn
// cycles on uninvolved orgs. (Replaces the previous "Runner iterates
// active orgs" test: the Runner no longer enumerates orgs; the Manager routes
// per org.)
func TestManager_TriggerLazyCreatesPerOrgRunner(t *testing.T) {
	entities := &recordingEntityStore{seen: map[string]int{}}
	m := NewManager(entities, nilProjectStore{}, nil, nil, nil, nil)
	defer m.Stop()

	m.Trigger("org-a")

	// The trigger is asynchronous: it wakes the runner's goroutine, which
	// calls ListUnclassifiedSystem. Poll for the side effect rather than sleep.
	if !waitFor(time.Second, func() bool { return entities.callsFor("org-a") >= 1 }) {
		t.Fatalf("ListUnclassifiedSystem was not called for org-a within 1s")
	}
	if got := entities.callsFor("org-b"); got != 0 {
		t.Errorf("ListUnclassifiedSystem called %d times for org-b without a Trigger; want 0", got)
	}

	m.mu.Lock()
	n := len(m.runners)
	m.mu.Unlock()
	if n != 1 {
		t.Errorf("manager holds %d runners after one org triggered; want 1", n)
	}
}

// TestManager_RepeatTriggerReusesRunner pins that repeated triggers for the
// same org route to the same per-org Runner rather than spawning a new one
// each time.
func TestManager_RepeatTriggerReusesRunner(t *testing.T) {
	entities := &recordingEntityStore{seen: map[string]int{}}
	m := NewManager(entities, nilProjectStore{}, nil, nil, nil, nil)
	defer m.Stop()

	m.Trigger("org-a")
	m.Trigger("org-a")
	m.Trigger("org-a")

	// Triggers coalesce (buffered chan), so we can't assert an exact cycle
	// count — only that the org ran and exactly one runner exists for it.
	if !waitFor(time.Second, func() bool { return entities.callsFor("org-a") >= 1 }) {
		t.Fatalf("ListUnclassifiedSystem was not called for org-a within 1s")
	}
	m.mu.Lock()
	n := len(m.runners)
	m.mu.Unlock()
	if n != 1 {
		t.Errorf("repeat triggers for org-a created %d runners; want 1", n)
	}
}

// TestManager_DistinctOrgsRunConcurrently is the headline contract for this
// refactor: a slow classification cycle on one tenant must not head-of-line-
// block another. Two orgs trigger; we block both inside ListUnclassifiedSystem
// and verify both arrivals are observed before either is released. The
// previous single Runner looped orgs serially, so the second arrival would
// queue behind the first's release.
func TestManager_DistinctOrgsRunConcurrently(t *testing.T) {
	arrivalA := make(chan struct{}, 1)
	arrivalB := make(chan struct{}, 1)
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})

	entities := &blockingEntityStore{
		onCall: func(orgID string) {
			switch orgID {
			case "org-a":
				arrivalA <- struct{}{}
				<-releaseA
			case "org-b":
				arrivalB <- struct{}{}
				<-releaseB
			}
		},
	}

	m := NewManager(entities, nilProjectStore{}, nil, nil, nil, nil)
	defer m.Stop()

	m.Trigger("org-a")
	m.Trigger("org-b")

	select {
	case <-arrivalA:
	case <-time.After(time.Second):
		t.Fatal("org-a cycle did not start within 1s")
	}
	select {
	case <-arrivalB:
	case <-time.After(time.Second):
		t.Fatal("org-b cycle did not start within 1s — head-of-line blocked on org-a")
	}

	close(releaseA)
	close(releaseB)
}

// TestManager_EmptyOrgIDDropped pins the defensive guard: a caller bug that
// passes an empty orgID must not silently route to a runner (which would
// cross-tenant-bleed classification in multi mode).
func TestManager_EmptyOrgIDDropped(t *testing.T) {
	entities := &recordingEntityStore{seen: map[string]int{}}
	m := NewManager(entities, nilProjectStore{}, nil, nil, nil, nil)
	defer m.Stop()

	m.Trigger("")

	// Give any (incorrectly-spawned) runner a chance to fire so the negative
	// assertion is meaningful.
	time.Sleep(50 * time.Millisecond)

	if got := entities.totalCalls(); got != 0 {
		t.Errorf("empty Trigger produced %d ListUnclassifiedSystem calls; want 0", got)
	}
	m.mu.Lock()
	n := len(m.runners)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("empty Trigger lazy-created %d runners; want 0", n)
	}
}

// TestManager_StopIdempotent pins both halves of Stop's contract: (1) it tears
// down without panic, (2) post-Stop Triggers are no-ops rather than
// re-instantiating runners the manager can no longer shut down.
func TestManager_StopIdempotent(t *testing.T) {
	entities := &recordingEntityStore{seen: map[string]int{}}
	m := NewManager(entities, nilProjectStore{}, nil, nil, nil, nil)

	m.Trigger("org-a")
	if !waitFor(time.Second, func() bool { return entities.callsFor("org-a") >= 1 }) {
		t.Fatalf("ListUnclassifiedSystem was not called for org-a within 1s")
	}

	m.Stop()
	m.Stop() // second call must not panic

	preCount := entities.callsFor("org-a")
	m.Trigger("org-a")
	m.Trigger("org-b")
	time.Sleep(50 * time.Millisecond)

	if got := entities.callsFor("org-a"); got != preCount {
		t.Errorf("post-Stop Trigger fired runner for org-a (calls went %d → %d)", preCount, got)
	}
	if got := entities.callsFor("org-b"); got != 0 {
		t.Errorf("post-Stop Trigger spawned new runner for org-b (%d calls)", got)
	}
}

// --- test doubles ---

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// recordingEntityStore counts ListUnclassifiedSystem calls per orgID. It
// returns an empty list so the runner cycle exits before any project read or
// LLM work — the embedded nil EntityStore would panic on any unexpected method
// call, the loud-failure-on-regression posture we want.
type recordingEntityStore struct {
	db.EntityStore
	mu   sync.Mutex
	seen map[string]int
}

func (s *recordingEntityStore) ListUnclassifiedSystem(ctx context.Context, orgID string) ([]domain.Entity, error) {
	s.mu.Lock()
	s.seen[orgID]++
	s.mu.Unlock()
	return nil, nil
}

func (s *recordingEntityStore) callsFor(orgID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[orgID]
}

func (s *recordingEntityStore) totalCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.seen {
		n += c
	}
	return n
}

// blockingEntityStore lets the test gate when each org's cycle returns. onCall
// fires synchronously inside ListUnclassifiedSystem; the test arranges for it
// to block on a per-org release channel. Returns an empty list once released so
// the cycle exits without other store methods being touched.
type blockingEntityStore struct {
	db.EntityStore
	onCall func(orgID string)
}

func (s *blockingEntityStore) ListUnclassifiedSystem(ctx context.Context, orgID string) ([]domain.Entity, error) {
	if s.onCall != nil {
		s.onCall(orgID)
	}
	return nil, nil
}

// nilProjectStore embeds db.ProjectStore as nil — the runner never reaches a
// project read in these tests (ListUnclassifiedSystem returns empty), so any
// call panics loudly, flagging a regression that adds an unexpected store hit.
type nilProjectStore struct{ db.ProjectStore }

var (
	_ db.EntityStore  = (*recordingEntityStore)(nil)
	_ db.EntityStore  = (*blockingEntityStore)(nil)
	_ db.ProjectStore = nilProjectStore{}
)
