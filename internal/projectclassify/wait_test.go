package projectclassify

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// waitEntities is an in-memory EntityStore for the WaitFor tests. WaitFor now
// drives a *Manager, which lazy-starts a real per-org runner on Trigger — so a
// real DB store would let that runner mutate (classify) the entity mid-test.
// This fake instead drives the two reads deterministically:
//   - ClassificationStatusSystem (WaitFor's probe): returns (classified,
//     exists) under the mutex so a test can flip them mid-wait.
//   - ListUnclassifiedSystem (the triggered runner's cycle entry): returns an
//     empty list so the runner exits before touching projects / the LLM, and
//     counts calls so a test can assert the Manager actually kicked a runner.
type waitEntities struct {
	db.EntityStore
	mu         sync.Mutex
	classified bool
	exists     bool
	listCalls  int
}

func (w *waitEntities) ClassificationStatusSystem(ctx context.Context, orgID, entityID string) (bool, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.classified, w.exists, nil
}

func (w *waitEntities) ListUnclassifiedSystem(ctx context.Context, orgID string) ([]domain.Entity, error) {
	w.mu.Lock()
	w.listCalls++
	w.mu.Unlock()
	return nil, nil
}

func (w *waitEntities) setClassified(v bool) {
	w.mu.Lock()
	w.classified = v
	w.mu.Unlock()
}

func (w *waitEntities) listCallCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.listCalls
}

// newWaitManager builds a Manager over the fake store. projects is nil — the
// triggered runner exits on the empty unclassified list before any project
// read. Callers defer m.Stop() to tear down the lazily-started runner.
func newWaitManager(w *waitEntities) *Manager {
	return NewManager(w, nil, nil, nil, nil, nil)
}

const waitOrg = runmode.LocalDefaultOrgID

// TestWaitFor_ReturnsImmediatelyWhenAlreadyClassified guards the
// fast-path: an entity that's already been classified should not
// trigger the runner or wait at all.
func TestWaitFor_ReturnsImmediatelyWhenAlreadyClassified(t *testing.T) {
	w := &waitEntities{classified: true, exists: true}
	m := newWaitManager(w)
	defer m.Stop()

	start := time.Now()
	WaitFor(context.Background(), w, m.Trigger, waitOrg, "e1", 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Errorf("WaitFor took %s — should have returned immediately for classified entity", elapsed)
	}
	if got := w.listCallCount(); got != 0 {
		t.Errorf("classified entity triggered %d runner cycles; want 0 (no trigger on the fast path)", got)
	}
}

// TestWaitFor_TriggersRunnerOnEntry verifies that WaitFor kicks the org's
// classifier (via Manager.Trigger) so it wakes up even if no post-poll trigger
// has fired for this entity yet. We observe the triggered runner's cycle
// (ListUnclassifiedSystem) rather than the now-internal trigger channel.
func TestWaitFor_TriggersRunnerOnEntry(t *testing.T) {
	w := &waitEntities{classified: false, exists: true}
	m := newWaitManager(w)
	defer m.Stop()

	// Cancellable ctx so the WaitFor goroutine doesn't outlive the test.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go WaitFor(ctx, w, m.Trigger, waitOrg, "e2", 200*time.Millisecond)

	if !waitFor(time.Second, func() bool { return w.listCallCount() >= 1 }) {
		t.Errorf("Manager.Trigger did not kick a runner cycle for the org")
	}
}

// TestWaitFor_HonorsTimeout verifies WaitFor returns within the
// configured budget when classification never completes.
func TestWaitFor_HonorsTimeout(t *testing.T) {
	w := &waitEntities{classified: false, exists: true}
	m := newWaitManager(w)
	defer m.Stop()

	start := time.Now()
	WaitFor(context.Background(), w, m.Trigger, waitOrg, "e3", 250*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed < 200*time.Millisecond {
		t.Errorf("WaitFor returned in %s — too fast for a 250ms timeout", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("WaitFor took %s — far longer than the 250ms timeout", elapsed)
	}
}

// TestWaitFor_WakesOnceClassificationLands verifies the polling loop
// returns promptly once classified_at is set mid-wait.
func TestWaitFor_WakesOnceClassificationLands(t *testing.T) {
	w := &waitEntities{classified: false, exists: true}
	m := newWaitManager(w)
	defer m.Stop()

	// Mid-wait, mark the entity classified.
	go func() {
		time.Sleep(200 * time.Millisecond)
		w.setClassified(true)
	}()

	start := time.Now()
	WaitFor(context.Background(), w, m.Trigger, waitOrg, "e4", 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("WaitFor returned in %s — should have woken within the polling cadence after classification", elapsed)
	}
}

// TestWaitFor_ReturnsEarlyOnMissingEntity guards against the bug where
// the probe treated a missing row as "still unclassified" and the
// caller stalled the full timeout for a deleted/non-existent row.
func TestWaitFor_ReturnsEarlyOnMissingEntity(t *testing.T) {
	w := &waitEntities{exists: false}
	m := newWaitManager(w)
	defer m.Stop()

	// UUID-shaped id mirrors the cross-backend convention; the row simply
	// doesn't exist.
	start := time.Now()
	WaitFor(context.Background(), w, m.Trigger, waitOrg, uuid.NewString(), 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Errorf("WaitFor took %s for a missing entity — should have returned immediately", elapsed)
	}
	if got := w.listCallCount(); got != 0 {
		t.Errorf("missing entity triggered %d runner cycles; want 0 (short-circuit before Trigger)", got)
	}
}

// TestWaitFor_ReturnsOnContextCancel guards the spawner-shutdown path:
// when the caller's ctx is cancelled (run aborted, server shutting
// down), WaitFor must break out instead of blocking the full timeout.
func TestWaitFor_ReturnsOnContextCancel(t *testing.T) {
	w := &waitEntities{classified: false, exists: true}
	m := newWaitManager(w)
	defer m.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	WaitFor(ctx, w, m.Trigger, waitOrg, "e5", 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Errorf("WaitFor took %s after ctx cancel — should have returned within the next poll tick", elapsed)
	}
}

// TestWaitFor_PreCancelledCtxReturnsWithoutTrigger guards the entry-time
// cancellation guard: a WaitFor entered with an already-cancelled context
// must return immediately AND must not kick the classifier — waking the
// runner on a shutdown path is pointless work.
func TestWaitFor_PreCancelledCtxReturnsWithoutTrigger(t *testing.T) {
	w := &waitEntities{classified: false, exists: true}
	m := newWaitManager(w)
	defer m.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before entry

	start := time.Now()
	WaitFor(ctx, w, m.Trigger, waitOrg, "e6", 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Errorf("WaitFor took %s for a pre-cancelled ctx — should have returned immediately", elapsed)
	}
	if got := w.listCallCount(); got != 0 {
		t.Errorf("WaitFor kicked the classifier on a pre-cancelled ctx (%d cycles); the entry guard should skip Trigger", got)
	}
}

// TestWaitFor_EmptyOrgReturnsEarly guards the empty-orgID path: classification
// is org-scoped and Manager.Trigger drops an empty org, so WaitFor must return
// immediately rather than poll the full timeout for a kick that can never fire.
func TestWaitFor_EmptyOrgReturnsEarly(t *testing.T) {
	w := &waitEntities{classified: false, exists: true}
	m := newWaitManager(w)
	defer m.Stop()

	start := time.Now()
	WaitFor(context.Background(), w, m.Trigger, "", "e-empty", 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Errorf("WaitFor took %s for an empty orgID — should return immediately (a kick can never fire)", elapsed)
	}
	if got := w.listCallCount(); got != 0 {
		t.Errorf("empty orgID triggered %d runner cycles; want 0", got)
	}
}

var _ db.EntityStore = (*waitEntities)(nil)
