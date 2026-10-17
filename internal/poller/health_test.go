package poller

import (
	"context"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// settingsCountingOrgsStore wraps fakeOrgsStore to count GetSettingsSystem
// calls per org, so a test can assert Health() loads each org's settings
// exactly once regardless of how many sources/branches consume it.
type settingsCountingOrgsStore struct {
	*fakeOrgsStore
	mu    sync.Mutex
	calls map[string]int
}

func newSettingsCountingOrgsStore(ids []string) *settingsCountingOrgsStore {
	return &settingsCountingOrgsStore{
		fakeOrgsStore: &fakeOrgsStore{ids: ids},
		calls:         map[string]int{},
	}
}

func (s *settingsCountingOrgsStore) GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	s.mu.Lock()
	s.calls[orgID]++
	s.mu.Unlock()
	return s.fakeOrgsStore.GetSettingsSystem(ctx, orgID)
}

// TestHealth_LoadsOrgSettingsOncePerOrg is a regression test for a review
// finding: sourceHealth used to call loadOrgSettings once for GitHub and
// AGAIN for Jira's own field read, tripling org_settings reads per
// /readyz hit at multi-mode scale. Health() now loads settings once per
// org up front and shares it across the per-source branches.
func TestHealth_LoadsOrgSettingsOncePerOrg(t *testing.T) {
	orgs := newSettingsCountingOrgsStore([]string{"org-1", "org-2"})
	m := &Manager{orgs: orgs}

	m.Health(context.Background())

	for _, orgID := range []string{"org-1", "org-2"} {
		if got := orgs.calls[orgID]; got != 1 {
			t.Errorf("GetSettingsSystem(%s) called %d times, want exactly 1", orgID, got)
		}
	}
}
