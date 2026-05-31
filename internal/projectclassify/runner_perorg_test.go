package projectclassify

import (
	"context"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRunner_IteratesActiveOrgs pins the outer-loop contract: the
// runner enumerates every active org via OrgsStore.ListActiveSystem
// and dispatches one classification pass per org. A regression that
// hardcoded the local-mode sentinel would silently skip entities/
// projects in any non-sentinel org.
//
// SQLite's per-store assertLocalOrg gates prevent inserting entities or
// projects under a non-sentinel org, so this test drives the loop with
// fake OrgsStore + fake EntityStore that record per-org invocations.
// ListUnclassifiedSystem returns empty for each org so we exit the
// classification body before any LLM work — the assertion is purely
// "every active org was visited."
func TestRunner_IteratesActiveOrgs(t *testing.T) {
	orgs := &fakeOrgsStore{ids: []string{"org-a", "org-b", "org-c"}}
	entities := &recordingEntityStore{}

	r := NewRunner(entities, nilProjectStore{}, orgs, nil)
	r.run(context.Background())

	if orgs.calls != 1 {
		t.Errorf("ListActiveSystem called %d times; want 1 per cycle", orgs.calls)
	}
	entities.mu.Lock()
	defer entities.mu.Unlock()
	if len(entities.visited) != len(orgs.ids) {
		t.Fatalf("ListUnclassifiedSystem visited %d orgs (%v); want %d (%v)", len(entities.visited), entities.visited, len(orgs.ids), orgs.ids)
	}
	for i, got := range entities.visited {
		if got != orgs.ids[i] {
			t.Errorf("visit[%d] = %s; want %s (per-org iteration must preserve ListActiveSystem order)", i, got, orgs.ids[i])
		}
	}
}

// TestRunner_OrgsStoreErrorAbortsCycle pins the contract: if
// ListActiveSystem fails, the runner logs and exits the cycle rather
// than falling back to a hardcoded sentinel. A silent fallback would
// mask the multi-mode "orgs table is unreachable" failure as
// "local-mode behavior" and produce wrong cross-tenant attribution.
func TestRunner_OrgsStoreErrorAbortsCycle(t *testing.T) {
	orgs := &fakeOrgsStore{err: errOrgsDown}
	entities := &recordingEntityStore{}

	r := NewRunner(entities, nilProjectStore{}, orgs, nil)
	r.run(context.Background())

	entities.mu.Lock()
	defer entities.mu.Unlock()
	if len(entities.visited) != 0 {
		t.Errorf("ListUnclassifiedSystem was called %d times despite ListActiveSystem error; want 0", len(entities.visited))
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

// recordingEntityStore embeds db.EntityStore as nil and overrides only
// ListUnclassifiedSystem (the single method the runner reaches before
// the empty-result early return). Any other method panic-traps via the
// nil embedded interface — which is what we want: a regression that
// adds an unrelated EntityStore call to the per-org loop should fail
// loudly rather than silently.
type recordingEntityStore struct {
	db.EntityStore
	mu      sync.Mutex
	visited []string
}

func (r *recordingEntityStore) ListUnclassifiedSystem(ctx context.Context, orgID string) ([]domain.Entity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.visited = append(r.visited, orgID)
	return nil, nil
}

// nilProjectStore is unused by the test because ListUnclassifiedSystem
// returns empty for every org (runner exits early). Embedding nil
// would crash if the runner unexpectedly reached projects — same
// loud-failure-on-regression posture as recordingEntityStore.
type nilProjectStore struct{ db.ProjectStore }

var (
	_ db.OrgsStore    = (*fakeOrgsStore)(nil)
	_ db.EntityStore  = (*recordingEntityStore)(nil)
	_ db.ProjectStore = nilProjectStore{}
)

type stubErr string

func (e stubErr) Error() string { return string(e) }

var errOrgsDown = stubErr("simulated orgs-store outage")
