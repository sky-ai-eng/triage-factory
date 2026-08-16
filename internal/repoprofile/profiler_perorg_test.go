package repoprofile

import (
	"context"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestProfiler_Run_IteratesActiveOrgs pins the outer-loop contract
// on the profiler: Run enumerates every active org via
// OrgsStore.ListActiveSystem and resolves each org's configured
// repos inside the loop. Empty repo lists short-circuit before any
// GitHub API call, so this test exercises the iteration without
// needing a real github client.
func TestProfiler_Run_IteratesActiveOrgs(t *testing.T) {
	orgs := &fakeOrgsStore{ids: []string{"org-a", "org-b", "org-c"}}
	repos := &recordingRepositoryStore{}

	p := NewProfiler(nil, nil, nil, repos, orgs, nil, nil, nil)
	if err := p.Run(context.Background(), false); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if orgs.calls != 1 {
		t.Errorf("ListActiveSystem called %d times; want 1 per Run", orgs.calls)
	}
	repos.mu.Lock()
	defer repos.mu.Unlock()
	if len(repos.visited) != len(orgs.ids) {
		t.Fatalf("ListTrackedNamesSystem visited %d orgs (%v); want %d (%v)", len(repos.visited), repos.visited, len(orgs.ids), orgs.ids)
	}
	for i, got := range repos.visited {
		if got != orgs.ids[i] {
			t.Errorf("visit[%d] = %s; want %s (per-org iteration must preserve ListActiveSystem order)", i, got, orgs.ids[i])
		}
	}
}

// TestProfiler_Run_OrgsStoreErrorBubbles pins that a failure listing
// active orgs aborts the whole Run with an error rather than silently
// falling back to the local sentinel. Per-org-repo errors degrade
// gracefully (logged + continue), but the orgs lookup is the outer
// loop boundary — losing visibility into the active org set means
// the run is fundamentally unable to proceed.
func TestProfiler_Run_OrgsStoreErrorBubbles(t *testing.T) {
	orgs := &fakeOrgsStore{err: errOrgsDown}
	repos := &recordingRepositoryStore{}

	p := NewProfiler(nil, nil, nil, repos, orgs, nil, nil, nil)
	if err := p.Run(context.Background(), false); err == nil {
		t.Fatal("Run returned nil; want error when ListActiveSystem fails")
	}
	repos.mu.Lock()
	defer repos.mu.Unlock()
	if len(repos.visited) != 0 {
		t.Errorf("ListTrackedNamesSystem called %d times despite ListActiveSystem error; want 0", len(repos.visited))
	}
}

// --- test doubles ---

type fakeOrgsStore struct {
	db.OrgsStore // embed nil — every method except ListActiveSystem panics if reached
	ids          []string
	err          error
	calls        int
}

func (f *fakeOrgsStore) ListActiveSystem(ctx context.Context) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]string(nil), f.ids...), nil
}

// recordingRepositoryStore embeds db.RepositoryStore as nil and overrides only
// ListTrackedNamesSystem. Returning empty short-circuits Run
// before any GitHub API call, so the test isolates the per-org loop
// behavior from the inner profiling body.
type recordingRepositoryStore struct {
	db.RepositoryStore
	mu      sync.Mutex
	visited []string
}

func (r *recordingRepositoryStore) ListTrackedNamesSystem(ctx context.Context, orgID string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.visited = append(r.visited, orgID)
	return nil, nil
}

type stubErr string

func (e stubErr) Error() string { return string(e) }

var errOrgsDown = stubErr("simulated orgs-store outage")

var (
	_ db.OrgsStore       = (*fakeOrgsStore)(nil)
	_ db.RepositoryStore = (*recordingRepositoryStore)(nil)
	_ domain.Repository  // keep domain import live for parity with siblings
)
