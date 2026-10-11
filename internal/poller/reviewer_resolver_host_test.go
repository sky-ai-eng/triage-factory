package poller

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
)

// erroringOrgs is an OrgsStore whose settings read fails, the shape a store
// blip leaves the reviewer resolver in.
type erroringOrgs struct {
	db.OrgsStore
}

func (erroringOrgs) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{}, errors.New("settings unavailable")
}

// hostRecordingUsers records the host every reverse login lookup is keyed on.
type hostRecordingUsers struct {
	db.UsersStore
	hosts []string
}

func (u *hostRecordingUsers) UserIDsForGitHubLoginSystem(_ context.Context, host, _ string) ([]string, error) {
	u.hosts = append(u.hosts, host)
	return []string{"user-1"}, nil
}

// TestReviewerResolver_SettingsReadFailureResolvesNobody pins the error arm:
// when the org's settings cannot be read there is no GitHub host to key the
// reverse identity lookup on, so no identity is consulted and a review request
// routes to nobody rather than to whoever holds the login on some other GitHub.
func TestReviewerResolver_SettingsReadFailureResolvesNobody(t *testing.T) {
	ghbase.SetDefaultBaseURLForTest(t, "https://ghe.default.test")
	users := &hostRecordingUsers{}
	m := &Manager{orgs: erroringOrgs{}, users: users}

	resolver := m.reviewerResolver(context.Background(), "org-1", "", nil)
	if resolver.KnownUser("octocat") {
		t.Error("KnownUser = true with no GitHub host to look the login up on")
	}
	if len(users.hosts) != 0 {
		t.Errorf("reverse lookup hosts = %q; want none", users.hosts)
	}
}
