package curator

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

type fakeClaimStore struct {
	rows []domain.CuratorTurn
	err  error
}

func (f *fakeClaimStore) ListClaimableTurnsForHomeSystem(context.Context, string) ([]domain.CuratorTurn, error) {
	return f.rows, f.err
}

// newTestLoop builds a HomeClaimLoop with an injected enqueue func (no real
// Curator / sessions), so the scan/track/prune logic is unit-testable.
func newTestLoop(store *fakeClaimStore, enqueue func(orgID, projectID, conversationID string, messageID int64, creatorUserID string) bool) *HomeClaimLoop {
	return &HomeClaimLoop{
		enqueue:  enqueue,
		store:    store,
		selfID:   "e1",
		interval: DefaultClaimScanInterval,
		wake:     make(chan struct{}, 1),
		tracked:  map[int64]struct{}{},
	}
}

func claimable(id int64) domain.CuratorTurn {
	return domain.CuratorTurn{
		MessageID:      id,
		ConversationID: "conv",
		OrgID:          "org",
		ProjectID:      "proj",
		CreatorUserID:  "u",
	}
}

func TestClaimLoop_FeedsEachQueuedOnce(t *testing.T) {
	store := &fakeClaimStore{rows: []domain.CuratorTurn{claimable(1), claimable(2)}}
	var fed []int64
	loop := newTestLoop(store, func(_, _, _ string, messageID int64, _ string) bool {
		fed = append(fed, messageID)
		return true
	})

	loop.scan(context.Background())
	if len(fed) != 2 {
		t.Fatalf("first scan should feed both queued turns, fed=%v", fed)
	}
	// Same rows still claimable (not yet claimed): a re-scan must NOT
	// re-feed the already-tracked ids.
	fed = nil
	loop.scan(context.Background())
	if len(fed) != 0 {
		t.Fatalf("tracked turns must not be re-fed, fed=%v", fed)
	}
}

func TestClaimLoop_RetriesWhenEnqueueRefused(t *testing.T) {
	store := &fakeClaimStore{rows: []domain.CuratorTurn{claimable(1)}}
	refuse := true
	var attempts int
	loop := newTestLoop(store, func(_, _, _ string, _ int64, _ string) bool {
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
	store := &fakeClaimStore{rows: []domain.CuratorTurn{claimable(1)}}
	var attempts int
	loop := newTestLoop(store, func(_, _, _ string, _ int64, _ string) bool {
		attempts++
		return true
	})

	loop.scan(context.Background()) // feeds 1, tracks it
	if _, ok := loop.tracked[1]; !ok {
		t.Fatalf("turn 1 should be tracked after first scan")
	}
	// 1 leaves the claimable set (dispatch claimed it).
	store.rows = nil
	loop.scan(context.Background())
	if _, ok := loop.tracked[1]; ok {
		t.Fatalf("turn 1 should be pruned once it leaves the claimable set")
	}
	if attempts != 1 {
		t.Fatalf("turn 1 must be fed exactly once, attempts=%d", attempts)
	}
}
