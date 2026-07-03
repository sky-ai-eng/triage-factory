package marketplacestats

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// fakeStore is a narrow db.MarketplaceStore test double: it embeds the nil
// interface so it satisfies db.MarketplaceStore without implementing every
// method, and overrides only RecomputeStatsSystem, the sole method
// Aggregator calls. Any other method would nil-panic if exercised, which
// these tests never do.
type fakeStore struct {
	db.MarketplaceStore

	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeStore) RecomputeStatsSystem(ctx context.Context, orgID string) error {
	f.mu.Lock()
	f.calls = append(f.calls, orgID)
	f.mu.Unlock()
	return f.err
}

func (f *fakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestAggregator_RunOrg_RecomputesOnFirstCall pins that a never-before-seen
// org always recomputes — there's no stale in-memory entry to skip on.
func TestAggregator_RunOrg_RecomputesOnFirstCall(t *testing.T) {
	store := &fakeStore{}
	a := NewAggregator(store)

	recomputed, err := a.RunOrg(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	if !recomputed {
		t.Error("recomputed = false on first call for a never-seen org; want true")
	}
	if got := store.callCount(); got != 1 {
		t.Errorf("store called %d times; want 1", got)
	}
}

// TestAggregator_RunOrg_TTLSkipsWithinWindow pins the whole point of the
// TTL gate: a second call for the same org within ttl is a no-op, so a
// poll-cycle-triggered subscriber doesn't hammer the DB every cycle.
func TestAggregator_RunOrg_TTLSkipsWithinWindow(t *testing.T) {
	store := &fakeStore{}
	a := NewAggregator(store)

	if _, err := a.RunOrg(context.Background(), "org-a"); err != nil {
		t.Fatalf("first RunOrg: %v", err)
	}
	recomputed, err := a.RunOrg(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("second RunOrg: %v", err)
	}
	if recomputed {
		t.Error("recomputed = true on a call within ttl; want false (TTL skip)")
	}
	if got := store.callCount(); got != 1 {
		t.Errorf("store called %d times; want 1 (second call skipped)", got)
	}
}

// TestAggregator_RunOrg_DistinctOrgsIndependent pins that the TTL gate is
// per-org, not global — a fresh org triggers its own recompute regardless
// of another org's recent cycle.
func TestAggregator_RunOrg_DistinctOrgsIndependent(t *testing.T) {
	store := &fakeStore{}
	a := NewAggregator(store)

	if _, err := a.RunOrg(context.Background(), "org-a"); err != nil {
		t.Fatalf("org-a RunOrg: %v", err)
	}
	recomputed, err := a.RunOrg(context.Background(), "org-b")
	if err != nil {
		t.Fatalf("org-b RunOrg: %v", err)
	}
	if !recomputed {
		t.Error("recomputed = false for a distinct org; want true (per-org TTL)")
	}
	if got := store.callCount(); got != 2 {
		t.Errorf("store called %d times; want 2 (one per org)", got)
	}
}

// TestAggregator_RunOrg_ErrorDoesNotArmTTL pins that a failed recompute
// leaves the TTL gate untouched, so the next trigger retries rather than
// silently sitting stale for a full ttl window after a transient failure.
func TestAggregator_RunOrg_ErrorDoesNotArmTTL(t *testing.T) {
	boom := context.DeadlineExceeded
	store := &fakeStore{err: boom}
	a := NewAggregator(store)

	if _, err := a.RunOrg(context.Background(), "org-a"); err == nil {
		t.Fatal("RunOrg with a failing store returned nil error")
	}
	store.err = nil
	recomputed, err := a.RunOrg(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("retry RunOrg: %v", err)
	}
	if !recomputed {
		t.Error("recomputed = false on retry after a failed cycle; want true (TTL not armed by the failure)")
	}
	if got := store.callCount(); got != 2 {
		t.Errorf("store called %d times; want 2 (failed attempt + retry)", got)
	}
}

// TestAggregator_TTLConstant is a sanity pin on the generous-freshness
// intent from the ticket ("daily-ish freshness is plenty") — catches an
// accidental order-of-magnitude change (e.g. minutes instead of hours).
func TestAggregator_TTLConstant(t *testing.T) {
	if ttl < 12*time.Hour || ttl > 48*time.Hour {
		t.Errorf("ttl = %v; want roughly daily (12h-48h)", ttl)
	}
}
