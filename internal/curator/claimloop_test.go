package curator

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

type fakeClaimStore struct {
	rows []domain.HomedCuratorRequest
	err  error
}

func (f *fakeClaimStore) ListQueuedRequestsForHomeSystem(context.Context, string) ([]domain.HomedCuratorRequest, error) {
	return f.rows, f.err
}

// newTestLoop builds a HomeClaimLoop with an injected enqueue func (no real
// Curator / sessions), so the scan/track/prune logic is unit-testable.
func newTestLoop(store *fakeClaimStore, enqueue func(orgID, projectID, requestID, creatorUserID string) bool) *HomeClaimLoop {
	return &HomeClaimLoop{
		enqueue:  enqueue,
		store:    store,
		selfID:   "e1",
		interval: DefaultClaimScanInterval,
		wake:     make(chan struct{}, 1),
		tracked:  map[string]struct{}{},
	}
}

func homed(id string) domain.HomedCuratorRequest {
	return domain.HomedCuratorRequest{ID: id, OrgID: "org", ProjectID: "proj-" + id, CreatorUserID: "u"}
}

func TestClaimLoop_FeedsEachQueuedOnce(t *testing.T) {
	store := &fakeClaimStore{rows: []domain.HomedCuratorRequest{homed("a"), homed("b")}}
	var fed []string
	loop := newTestLoop(store, func(_, _, requestID, _ string) bool {
		fed = append(fed, requestID)
		return true
	})

	loop.scan(context.Background())
	if len(fed) != 2 {
		t.Fatalf("first scan should feed both queued turns, fed=%v", fed)
	}
	// Same rows still queued (not yet flipped to running): a re-scan must NOT
	// re-feed the already-tracked ids.
	fed = nil
	loop.scan(context.Background())
	if len(fed) != 0 {
		t.Fatalf("tracked turns must not be re-fed, fed=%v", fed)
	}
}

func TestClaimLoop_RetriesWhenEnqueueRefused(t *testing.T) {
	store := &fakeClaimStore{rows: []domain.HomedCuratorRequest{homed("a")}}
	refuse := true
	var attempts int
	loop := newTestLoop(store, func(_, _, _, _ string) bool {
		attempts++
		return !refuse // first call refused (queue full), later accepted
	})

	loop.scan(context.Background()) // refused → not tracked
	refuse = false
	loop.scan(context.Background()) // accepted → tracked
	loop.scan(context.Background()) // tracked → skipped

	if attempts != 2 {
		t.Fatalf("a refused enqueue must be retried exactly until accepted, attempts=%d", attempts)
	}
}

func TestClaimLoop_PrunesPickedUpTurns(t *testing.T) {
	store := &fakeClaimStore{rows: []domain.HomedCuratorRequest{homed("a")}}
	var attempts int
	loop := newTestLoop(store, func(_, _, _, _ string) bool {
		attempts++
		return true
	})

	loop.scan(context.Background()) // feeds a, tracks it
	if _, ok := loop.tracked["a"]; !ok {
		t.Fatalf("a should be tracked after first scan")
	}
	// a leaves the queued set (dispatch flipped it to running).
	store.rows = nil
	loop.scan(context.Background())
	if _, ok := loop.tracked["a"]; ok {
		t.Fatalf("a should be pruned once it leaves the queued set")
	}
	if attempts != 1 {
		t.Fatalf("a must be fed exactly once, attempts=%d", attempts)
	}
}
