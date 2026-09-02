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

// TestReviewerResolver_SettingsReadFailureFallsBackToTheDeploymentDefault pins
// the error arm: when the org's settings cannot be read, the reverse identity
// lookup keys on the deployment's default GitHub — never on "", which no
// identity row was ever written under and which would route every review
// request to nobody.
func TestReviewerResolver_SettingsReadFailureFallsBackToTheDeploymentDefault(t *testing.T) {
	ghbase.SetDefaultBaseURLForTest(t, "https://ghe.default.test")
	users := &hostRecordingUsers{}
	m := &Manager{orgs: erroringOrgs{}, users: users}

	resolver := m.reviewerResolver(context.Background(), "org-1", "", nil)
	if !resolver.KnownUser("octocat") {
		t.Fatal("KnownUser = false; the fake users store answers for every login")
	}
	if len(users.hosts) != 1 || users.hosts[0] != "https://ghe.default.test" {
		t.Errorf("reverse lookup hosts = %q; want exactly [%q] — the deployment default, not an empty key",
			users.hosts, "https://ghe.default.test")
	}
}
