package tracker

import (
	"context"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
)

// TestRefreshJira_PropagatesTrackerOrgIDToEntityStore pins the
// per-Tracker tenant contract: every entity-store read/write happens
// against the Tracker's construction-time orgID, with no hardcoded
// sentinel fallback. The poller manager constructs one Tracker per
// active org per cycle and the orgID stays stable for that Tracker's
// lifetime.
//
// The test uses the empty-projects fast path (discoverJira returns nil
// for len(projects)==0) so no Jira client call is required. The first
// reachable entity-store call after discovery is ListActiveSystem,
// which lets the test capture the orgID without setting up snapshot
// diff scaffolding.
func TestRefreshJira_PropagatesTrackerOrgIDToEntityStore(t *testing.T) {
	bus := eventbus.New()
	defer bus.Close()

	entities := &recordingEntityStore{}
	const wantOrg = "00000000-0000-0000-0000-000000000abc"
	tr := &Tracker{pub: bus, entities: entities, orgID: wantOrg}

	if _, err := tr.RefreshJira(context.Background(), nil, "", nil); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}

	entities.mu.Lock()
	defer entities.mu.Unlock()
	if len(entities.listActiveOrgIDs) == 0 {
		t.Fatal("ListActiveSystem was not called; the tracker should query entities for its bound org")
	}
	for i, got := range entities.listActiveOrgIDs {
		if got != wantOrg {
			t.Errorf("ListActiveSystem[%d] received orgID %q; want %q (tracker should propagate its construction-time orgID)", i, got, wantOrg)
		}
	}
}

// --- test doubles ---

// recordingEntityStore embeds db.EntityStore as nil and overrides
// ListActiveSystem (the only entity-store method reached by the
// empty-projects test path). Any unexpected EntityStore call panics
// via the nil embedded interface — a regression that bypasses the
// fast-path early-exit fails loudly.
type recordingEntityStore struct {
	db.EntityStore
	mu               sync.Mutex
	listActiveOrgIDs []string
}

func (r *recordingEntityStore) ListActiveSystem(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listActiveOrgIDs = append(r.listActiveOrgIDs, orgID)
	return nil, nil
}
