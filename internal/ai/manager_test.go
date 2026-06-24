package ai

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestManager_TriggerLazyCreatesPerOrgRunner pins the lazy-creation
// contract: triggering an unseen orgID instantiates a Runner for that
// org and only that org. A regression that hardcoded the local
// sentinel — or that fanned a trigger to every existing runner —
// would either silently drop multi-org work or burn cycles on
// uninvolved orgs.
func TestManager_TriggerLazyCreatesPerOrgRunner(t *testing.T) {
	scores := &recordingScoreStore{seen: map[string]int{}}

	m := NewManager(nil, scores, nil, nil, nil, RunnerCallbacks{})
	defer m.Stop()

	m.Trigger("org-a")

	// The trigger is asynchronous: it wakes the runner's goroutine,
	// which calls UnscoredTasks. Poll for the side effect rather than
	// sleep-and-hope.
	if !waitFor(t, time.Second, func() bool { return scores.callsFor("org-a") >= 1 }) {
		t.Fatalf("UnscoredTasks was not called for org-a within 1s")
	}
	if got := scores.callsFor("org-b"); got != 0 {
		t.Errorf("UnscoredTasks called %d times for org-b without a Trigger; want 0", got)
	}
}

// TestManager_DistinctOrgsRunConcurrently is the headline contract for
// this refactor: a slow scoring cycle on one tenant must not head-of-
// line-block another tenant. Two orgs trigger; we block both inside
// UnscoredTasks and verify both arrivals are observed before either
// is released. With a sequential per-cycle loop, only one arrival
// would land — the second would queue behind the first's release.
func TestManager_DistinctOrgsRunConcurrently(t *testing.T) {
	arrivalA := make(chan struct{}, 1)
	arrivalB := make(chan struct{}, 1)
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})

	scores := &blockingScoreStore{
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

	m := NewManager(nil, scores, nil, nil, nil, RunnerCallbacks{})
	defer m.Stop()

	m.Trigger("org-a")
	m.Trigger("org-b")

	// Both cycles must be in-flight simultaneously. If the manager
	// serialized them, only the first arrival channel would fire and
	// the second would block on the runner's stop or on a per-org
	// queue that doesn't exist.
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

// TestManager_EmptyOrgIDDropped pins the defensive guard: a caller bug
// that passes an empty orgID must not silently route to the local
// sentinel (which would cross-tenant-bleed scoring in multi mode).
func TestManager_EmptyOrgIDDropped(t *testing.T) {
	scores := &recordingScoreStore{seen: map[string]int{}}

	m := NewManager(nil, scores, nil, nil, nil, RunnerCallbacks{})
	defer m.Stop()

	m.Trigger("")

	// Give any (incorrectly-spawned) runner a chance to fire so the
	// negative assertion is meaningful rather than just "we polled
	// fast enough."
	time.Sleep(50 * time.Millisecond)

	scores.mu.Lock()
	defer scores.mu.Unlock()
	for org, n := range scores.seen {
		if n > 0 {
			t.Errorf("empty Trigger produced %d UnscoredTasks calls for org %q; want none", n, org)
		}
	}
}

// TestManager_StopIdempotent pins both halves of Stop's contract:
// (1) it tears down without panic, (2) post-Stop Triggers are no-ops
// rather than re-instantiating runners that the manager can no longer
// shut down.
func TestManager_StopIdempotent(t *testing.T) {
	scores := &recordingScoreStore{seen: map[string]int{}}
	m := NewManager(nil, scores, nil, nil, nil, RunnerCallbacks{})

	m.Trigger("org-a")
	if !waitFor(t, time.Second, func() bool { return scores.callsFor("org-a") >= 1 }) {
		t.Fatalf("UnscoredTasks was not called for org-a within 1s")
	}

	m.Stop()
	m.Stop() // second call must not panic

	preCount := scores.callsFor("org-a")
	m.Trigger("org-a")
	m.Trigger("org-b")
	time.Sleep(50 * time.Millisecond)

	if got := scores.callsFor("org-a"); got != preCount {
		t.Errorf("post-Stop Trigger fired runner for org-a (calls went %d → %d)", preCount, got)
	}
	if got := scores.callsFor("org-b"); got != 0 {
		t.Errorf("post-Stop Trigger spawned new runner for org-b (%d calls)", got)
	}
}

// --- test doubles ---

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// recordingScoreStore counts UnscoredTasks calls per orgID. Returns an
// empty task list so the runner cycle exits without exercising any
// other ScoreStore method — the embedded nil interface would panic on
// an unexpected method call, which is the loud-failure-on-regression
// posture we want.
type recordingScoreStore struct {
	db.ScoreStore
	mu   sync.Mutex
	seen map[string]int
}

func (s *recordingScoreStore) UnscoredTasks(ctx context.Context, orgID string) ([]domain.Task, error) {
	s.mu.Lock()
	s.seen[orgID]++
	s.mu.Unlock()
	return nil, nil
}

func (s *recordingScoreStore) callsFor(orgID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[orgID]
}

// blockingScoreStore lets the test gate when each org's cycle returns.
// onCall fires synchronously inside UnscoredTasks; the test arranges
// for it to block on a per-org release channel. Returns an empty task
// list once released so the cycle exits without other ScoreStore
// methods being touched.
type blockingScoreStore struct {
	db.ScoreStore
	onCall func(orgID string)
}

func (s *blockingScoreStore) UnscoredTasks(ctx context.Context, orgID string) ([]domain.Task, error) {
	if s.onCall != nil {
		s.onCall(orgID)
	}
	return nil, nil
}

var (
	_ db.ScoreStore = (*recordingScoreStore)(nil)
	_ db.ScoreStore = (*blockingScoreStore)(nil)
)
