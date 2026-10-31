package poller

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// classOrgs answers the one read orgHasRegisteredApp makes of org settings.
type classOrgs struct {
	db.OrgsStore
	class domain.GitHubCredentialClass
	err   error
}

func (o classOrgs) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	if o.err != nil {
		return domain.OrgSettings{}, o.err
	}
	return domain.OrgSettings{GitHubCredentialClass: o.class}, nil
}

// classApps answers the registration read the BYO arm makes — and counts it, so
// the managed arm can be shown not to make one.
type classApps struct {
	db.GitHubAppsStore
	app   *domain.OrgGitHubApp
	err   error
	reads int
}

func (a *classApps) GetForOrgSystem(context.Context, string) (*domain.OrgGitHubApp, error) {
	a.reads++
	return a.app, a.err
}

// TestOrgHasRegisteredApp_EveryCredentialClass walks all three classes through
// the poll cycle's App-vs-PAT dispatch gate.
//
// What it pins is that the CLASS decides, not the presence of a registration
// row. Two of the three classes have no row at all, and they want opposite
// answers: a PAT org must not fan the cycle out over installations, and a
// managed org must — its installations are exactly what it bound the shared App
// on. Reading rowlessness as "poll with a PAT" is the inference this gate exists
// to have already stopped making.
func TestOrgHasRegisteredApp_EveryCredentialClass(t *testing.T) {
	ctx := context.Background()
	active := &domain.OrgGitHubApp{Active: true}
	staged := &domain.OrgGitHubApp{Active: false}

	for _, tc := range []struct {
		name     string
		class    domain.GitHubCredentialClass
		app      *domain.OrgGitHubApp
		want     bool
		wantRead bool // whether the registration row is consulted at all
	}{
		{"pat org", domain.GitHubCredentialClassPAT, nil, false, false},
		{"byo app, active", domain.GitHubCredentialClassBYOApp, active, true, true},
		// The class says App, the Active bit says the PAT is still live. Two
		// orthogonal facts, and the cycle follows the second.
		{"byo app, staged behind a live pat", domain.GitHubCredentialClassBYOApp, staged, false, true},
		{"byo app class with no row", domain.GitHubCredentialClassBYOApp, nil, false, true},
		// No row exists or can, so there is no Active bit and no staged window —
		// the class is the whole answer, and it is answered without a second read.
		{"managed app", domain.GitHubCredentialClassManagedApp, nil, true, false},
		{"unknown class", domain.GitHubCredentialClass("not-a-credential-class"), nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apps := &classApps{app: tc.app}
			m := &Manager{orgs: classOrgs{class: tc.class}, apps: apps}
			if got := m.orgHasRegisteredApp(ctx, "org-1"); got != tc.want {
				t.Errorf("orgHasRegisteredApp = %v; want %v", got, tc.want)
			}
			if gotRead := apps.reads > 0; gotRead != tc.wantRead {
				t.Errorf("registration row read = %v; want %v", gotRead, tc.wantRead)
			}
		})
	}
}

// TestOrgHasRegisteredApp_UnreadableSettingsPollsAsPAT pins that the fail-open
// answer is unchanged. False here does not mean "poll with a PAT" — it means
// "don't fan out per installation", and the resolver one layer down refuses an
// unreadable class outright rather than borrowing a credential.
func TestOrgHasRegisteredApp_UnreadableSettingsPollsAsPAT(t *testing.T) {
	m := &Manager{
		orgs: classOrgs{err: errors.New("settings store unavailable")},
		apps: &classApps{},
	}
	if m.orgHasRegisteredApp(context.Background(), "org-1") {
		t.Error("an unreadable settings row engaged the App path; the class is what selects it and it was never read")
	}
}
