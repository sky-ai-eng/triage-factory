package poller

import (
	"context"
	"errors"
	"testing"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

const scopeDriftMetric = "tf_github_scope_drift_repos"

// fakeScopeDriftSource is the store reads as the scope drift observer sees
// them, keyed by org.
type fakeScopeDriftSource struct {
	orgs        []string
	orgsErr     error
	settings    map[string]domain.OrgSettings
	settingsErr map[string]error
	installs    map[string][]domain.OrgGitHubAppInstallation
	refreshed   map[string]bool
	drift       map[string]int
	driftOpts   []dbpkg.ListOpts
}

func (f *fakeScopeDriftSource) ListActiveSystem(context.Context) ([]string, error) {
	return f.orgs, f.orgsErr
}

func (f *fakeScopeDriftSource) GetSettingsSystem(_ context.Context, orgID string) (domain.OrgSettings, error) {
	if err := f.settingsErr[orgID]; err != nil {
		return domain.OrgSettings{}, err
	}
	return f.settings[orgID], nil
}

func (f *fakeScopeDriftSource) ListInstallationsOnHostSystem(_ context.Context, orgID, _ string) ([]domain.OrgGitHubAppInstallation, error) {
	return f.installs[orgID], nil
}

func (f *fakeScopeDriftSource) ReachableStateSystem(_ context.Context, orgID, _ string, _ domain.GitHubCredentialClass) (domain.ReachableCacheState, error) {
	return domain.ReachableCacheState{Refreshed: f.refreshed[orgID]}, nil
}

func (f *fakeScopeDriftSource) ListScopeDriftSystem(_ context.Context, orgID, _ string, _ domain.GitHubCredentialClass, opts dbpkg.ListOpts) ([]domain.ScopeDriftRepository, int, error) {
	f.driftOpts = append(f.driftOpts, opts)
	return nil, f.drift[orgID], nil
}

func appOrg(class domain.GitHubCredentialClass) domain.OrgSettings {
	return domain.OrgSettings{GitHubCredentialClass: class, GitHubBaseURL: "https://github.com"}
}

func grant(selection string) []domain.OrgGitHubAppInstallation {
	return []domain.OrgGitHubAppInstallation{{InstallationID: "1", AccountLogin: "acme", RepositorySelection: selection}}
}

// TestScopeDriftObserver_ReportsOnlyOrgsWhoseFindingCanBeTrusted: an org with
// an App, a selective grant and a refreshed mirror reports its count, 0
// included. Every org whose 0 would mean something other than an all-clear
// has no series. The count is read count-only.
func TestScopeDriftObserver_ReportsOnlyOrgsWhoseFindingCanBeTrusted(t *testing.T) {
	provider, registry := newScrapedProvider(t)
	noHost := appOrg(domain.GitHubCredentialClassBYOApp)
	noHost.GitHubBaseURL = ""
	src := &fakeScopeDriftSource{
		orgs: []string{"drift", "clear", "pat", "all", "unknown", "mixed", "unrefreshed", "nohost"},
		settings: map[string]domain.OrgSettings{
			"drift":       appOrg(domain.GitHubCredentialClassBYOApp),
			"clear":       appOrg(domain.GitHubCredentialClassManagedApp),
			"pat":         appOrg(domain.GitHubCredentialClassPAT),
			"all":         appOrg(domain.GitHubCredentialClassBYOApp),
			"unknown":     appOrg(domain.GitHubCredentialClassBYOApp),
			"mixed":       appOrg(domain.GitHubCredentialClassBYOApp),
			"unrefreshed": appOrg(domain.GitHubCredentialClassBYOApp),
			"nohost":      noHost,
		},
		installs: map[string][]domain.OrgGitHubAppInstallation{
			"drift":       grant(domain.RepositorySelectionSelected),
			"clear":       grant(domain.RepositorySelectionSelected),
			"pat":         grant(domain.RepositorySelectionSelected),
			"all":         grant(domain.RepositorySelectionAll),
			"unknown":     grant(""),
			"mixed":       append(grant(domain.RepositorySelectionAll), grant(domain.RepositorySelectionSelected)...),
			"unrefreshed": grant(domain.RepositorySelectionSelected),
			"nohost":      grant(domain.RepositorySelectionSelected),
		},
		refreshed: map[string]bool{
			"drift": true, "clear": true, "pat": true, "all": true, "unknown": true, "mixed": true, "nohost": true,
		},
		drift: map[string]int{"drift": 3, "pat": 9, "all": 9, "unknown": 9, "mixed": 1, "unrefreshed": 9, "nohost": 9},
	}
	c := NewScopeDriftObserver(provider, src)
	defer c.Close()

	if lines := scrapeMetric(t, registry, scopeDriftMetric); len(lines) != 0 {
		t.Errorf("reported before any read: %v", lines)
	}

	c.Tick(context.Background())
	assertLines(t, scrapeMetric(t, registry, scopeDriftMetric), []string{
		`tf_github_scope_drift_repos{org_id="drift"} 3`,
		`tf_github_scope_drift_repos{org_id="clear"} 0`,
		`tf_github_scope_drift_repos{org_id="mixed"} 1`,
	})
	for _, opts := range src.driftOpts {
		if !opts.CountOnly || opts.Limit != 0 || opts.Offset != 0 || opts.After != nil {
			t.Errorf("scope drift read with %+v, want count-only", opts)
		}
	}
}

// TestScopeDriftObserver_DropsOrgsThatStopQualifyingAndKeepsValuesOnAFailedRead:
// each read replaces the last, so an org that no longer qualifies stops
// reporting; an org whose own reads fail keeps its value, and so does every
// org when the org list cannot be read.
func TestScopeDriftObserver_DropsOrgsThatStopQualifyingAndKeepsValuesOnAFailedRead(t *testing.T) {
	provider, registry := newScrapedProvider(t)
	src := &fakeScopeDriftSource{
		orgs: []string{"org-1", "org-2", "org-3"},
		settings: map[string]domain.OrgSettings{
			"org-1": appOrg(domain.GitHubCredentialClassBYOApp),
			"org-2": appOrg(domain.GitHubCredentialClassBYOApp),
			"org-3": appOrg(domain.GitHubCredentialClassBYOApp),
		},
		installs: map[string][]domain.OrgGitHubAppInstallation{
			"org-1": grant(domain.RepositorySelectionSelected),
			"org-2": grant(domain.RepositorySelectionSelected),
			"org-3": grant(domain.RepositorySelectionSelected),
		},
		refreshed: map[string]bool{"org-1": true, "org-2": true, "org-3": true},
		drift:     map[string]int{"org-1": 2, "org-2": 1, "org-3": 4},
	}
	c := NewScopeDriftObserver(provider, src)
	defer c.Close()
	c.Tick(context.Background())

	src.orgs = []string{"org-1", "org-2"}
	src.settings["org-2"] = appOrg(domain.GitHubCredentialClassPAT)
	src.settingsErr = map[string]error{"org-1": errors.New("database unreachable")}
	c.Tick(context.Background())
	assertLines(t, scrapeMetric(t, registry, scopeDriftMetric), []string{`tf_github_scope_drift_repos{org_id="org-1"} 2`})

	src.orgs, src.orgsErr = nil, errors.New("database unreachable")
	c.Tick(context.Background())
	assertLines(t, scrapeMetric(t, registry, scopeDriftMetric), []string{`tf_github_scope_drift_repos{org_id="org-1"} 2`})
}

// TestScopeDriftObserver_CloseStopsReporting is the lease-flap case, as for
// the connection observer.
func TestScopeDriftObserver_CloseStopsReporting(t *testing.T) {
	provider, registry := newScrapedProvider(t)
	src := &fakeScopeDriftSource{
		orgs:      []string{"org-1"},
		settings:  map[string]domain.OrgSettings{"org-1": appOrg(domain.GitHubCredentialClassBYOApp)},
		installs:  map[string][]domain.OrgGitHubAppInstallation{"org-1": grant(domain.RepositorySelectionSelected)},
		refreshed: map[string]bool{"org-1": true},
		drift:     map[string]int{"org-1": 1},
	}
	c := NewScopeDriftObserver(provider, src)
	c.Tick(context.Background())
	c.Close()
	if lines := scrapeMetric(t, registry, scopeDriftMetric); len(lines) != 0 {
		t.Errorf("a closed observer still reports: %v", lines)
	}
}
