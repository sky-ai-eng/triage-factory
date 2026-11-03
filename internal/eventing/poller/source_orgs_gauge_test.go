package poller

import (
	"context"
	"errors"
	"testing"

	"github.com/zalando/go-keyring"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const sourceOrgsMetric = "tf_source_orgs"

// fakeSourceOrgsSource is the reads as the source orgs observer sees them,
// keyed by org.
type fakeSourceOrgsSource struct {
	orgs       []string
	orgsErr    error
	avail      map[string]eventsource.Availability
	resolveErr map[string]error
	settings   map[string]domain.OrgSettings
	apps       map[string]*domain.OrgGitHubApp
	installs   map[string][]domain.OrgGitHubAppInstallation
	// installHosts records the host each installation read asked about.
	installHosts []string
}

func (f *fakeSourceOrgsSource) ListActiveSystem(context.Context) ([]string, error) {
	return f.orgs, f.orgsErr
}

func (f *fakeSourceOrgsSource) ResolveSystem(_ context.Context, orgID string) (eventsource.Availability, error) {
	if err := f.resolveErr[orgID]; err != nil {
		return nil, err
	}
	return f.avail[orgID], nil
}

func (f *fakeSourceOrgsSource) GetSettingsSystem(_ context.Context, orgID string) (domain.OrgSettings, error) {
	return f.settings[orgID], nil
}

func (f *fakeSourceOrgsSource) GetForOrgSystem(_ context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	return f.apps[orgID], nil
}

func (f *fakeSourceOrgsSource) ListInstallationsOnHostSystem(_ context.Context, orgID, host string) ([]domain.OrgGitHubAppInstallation, error) {
	f.installHosts = append(f.installHosts, host)
	return f.installs[orgID], nil
}

// avail builds an org's availability from github, jira and linear states,
// in that order.
func avail(github, jira, linear eventsource.State) eventsource.Availability {
	return eventsource.Availability{
		eventsource.NewSource(eventsource.KindGitHub, github),
		eventsource.NewSource(eventsource.KindJira, jira),
		eventsource.NewSource(eventsource.KindLinear, linear),
	}
}

const (
	stAvailable    = eventsource.StateAvailable
	stUnconfigured = eventsource.StateUnconfigured
	stDisabled     = eventsource.StateDisabled
)

// TestSourceOrgsObserver_CountsOrgsByStateWithEveryPairPresent: each org is
// counted once per source under its resolved state, every (source, state)
// pair the gauge declares is reported, 0 included, and a source outside core
// is not counted. An org whose own active App has no installation on its host
// is no_installations; an org with installations there, a staged App, a
// PAT or the deployment's App stays available.
func TestSourceOrgsObserver_CountsOrgsByStateWithEveryPairPresent(t *testing.T) {
	provider, registry := newScrapedProvider(t)
	byo := domain.OrgSettings{GitHubCredentialClass: domain.GitHubCredentialClassBYOApp, GitHubBaseURL: "https://GHE.example.com/"}
	active := &domain.OrgGitHubApp{Active: true, ClientID: "Iv1.x"}
	withChat := append(avail(stAvailable, stUnconfigured, stUnconfigured), eventsource.NewSource("chat", stAvailable))
	src := &fakeSourceOrgsSource{
		orgs: []string{"installed", "uninstalled", "staged", "pat", "managed", "none"},
		avail: map[string]eventsource.Availability{
			"installed":   avail(stAvailable, stAvailable, stAvailable),
			"uninstalled": avail(stAvailable, stDisabled, stUnconfigured),
			"staged":      avail(stAvailable, stUnconfigured, stAvailable),
			"pat":         avail(stAvailable, stDisabled, stDisabled),
			"managed":     withChat,
			"none":        avail(stUnconfigured, stUnconfigured, stUnconfigured),
		},
		settings: map[string]domain.OrgSettings{
			"installed":   byo,
			"uninstalled": byo,
			"staged":      byo,
			"pat":         {GitHubCredentialClass: domain.GitHubCredentialClassPAT, GitHubBaseURL: "https://github.com"},
			"managed":     {GitHubCredentialClass: domain.GitHubCredentialClassManagedApp, GitHubBaseURL: "https://github.com"},
			"none":        byo,
		},
		apps: map[string]*domain.OrgGitHubApp{
			"installed":   active,
			"uninstalled": active,
			"staged":      {Active: false, ClientID: "Iv1.x"},
			"none":        active,
		},
		installs: map[string][]domain.OrgGitHubAppInstallation{
			"installed": {{InstallationID: "1", AccountLogin: "acme"}},
		},
	}
	c := NewSourceOrgsObserver(provider, src)
	defer c.Close()

	if lines := scrapeMetric(t, registry, sourceOrgsMetric); len(lines) != 0 {
		t.Errorf("reported before any read: %v", lines)
	}

	c.Tick(context.Background())
	assertLines(t, scrapeMetric(t, registry, sourceOrgsMetric), []string{
		`tf_source_orgs{source="github",state="available"} 4`,
		`tf_source_orgs{source="github",state="no_installations"} 1`,
		`tf_source_orgs{source="github",state="unconfigured"} 1`,
		`tf_source_orgs{source="jira",state="available"} 1`,
		`tf_source_orgs{source="jira",state="unconfigured"} 3`,
		`tf_source_orgs{source="jira",state="disabled"} 2`,
		`tf_source_orgs{source="linear",state="available"} 2`,
		`tf_source_orgs{source="linear",state="unconfigured"} 3`,
		`tf_source_orgs{source="linear",state="disabled"} 1`,
	})
	// Only the two orgs whose own active App decides it read installations,
	// and on the org's host as the poll keys it.
	if len(src.installHosts) != 2 {
		t.Errorf("installation reads = %v, want one for each of the two orgs with an active App of their own", src.installHosts)
	}
	for _, host := range src.installHosts {
		if host != domain.GitHubHost(byo.GitHubBaseURL) {
			t.Errorf("installations read on %q, want the org's host %q", host, domain.GitHubHost(byo.GitHubBaseURL))
		}
	}
}

// TestSourceOrgsObserver_ReportsZerosWithNoOrgs: a deployment with no active
// org reports every pair at 0 once the org list has been read, rather than
// no series.
func TestSourceOrgsObserver_ReportsZerosWithNoOrgs(t *testing.T) {
	provider, registry := newScrapedProvider(t)
	c := NewSourceOrgsObserver(provider, &fakeSourceOrgsSource{})
	defer c.Close()
	c.Tick(context.Background())
	assertLines(t, scrapeMetric(t, registry, sourceOrgsMetric), []string{
		`tf_source_orgs{source="github",state="available"} 0`,
		`tf_source_orgs{source="github",state="no_installations"} 0`,
		`tf_source_orgs{source="github",state="unconfigured"} 0`,
		`tf_source_orgs{source="jira",state="available"} 0`,
		`tf_source_orgs{source="jira",state="unconfigured"} 0`,
		`tf_source_orgs{source="jira",state="disabled"} 0`,
		`tf_source_orgs{source="linear",state="available"} 0`,
		`tf_source_orgs{source="linear",state="unconfigured"} 0`,
		`tf_source_orgs{source="linear",state="disabled"} 0`,
	})
}

// TestSourceOrgsObserver_KeepsStatesOnAFailedRead: each read replaces the
// last, so a deleted org stops being counted; an org whose read fails keeps
// its previous states in the count, and every org does when the org list
// cannot be read. Before any org list has been read, nothing is reported.
func TestSourceOrgsObserver_KeepsStatesOnAFailedRead(t *testing.T) {
	provider, registry := newScrapedProvider(t)
	src := &fakeSourceOrgsSource{orgsErr: errors.New("database unreachable")}
	c := NewSourceOrgsObserver(provider, src)
	defer c.Close()
	c.Tick(context.Background())
	if lines := scrapeMetric(t, registry, sourceOrgsMetric); len(lines) != 0 {
		t.Errorf("reported with the org list never read: %v", lines)
	}

	src.orgs, src.orgsErr = []string{"org-1", "org-2", "org-3"}, nil
	src.avail = map[string]eventsource.Availability{
		"org-1": avail(stUnconfigured, stAvailable, stUnconfigured),
		"org-2": avail(stUnconfigured, stAvailable, stUnconfigured),
		"org-3": avail(stUnconfigured, stUnconfigured, stUnconfigured),
	}
	c.Tick(context.Background())

	// org-1 fails, org-2 changes, org-3 is deleted.
	src.orgs = []string{"org-1", "org-2"}
	src.resolveErr = map[string]error{"org-1": errors.New("database unreachable")}
	src.avail["org-1"] = avail(stUnconfigured, stDisabled, stUnconfigured)
	src.avail["org-2"] = avail(stUnconfigured, stDisabled, stUnconfigured)
	c.Tick(context.Background())
	want := []string{
		`tf_source_orgs{source="github",state="available"} 0`,
		`tf_source_orgs{source="github",state="no_installations"} 0`,
		`tf_source_orgs{source="github",state="unconfigured"} 2`,
		`tf_source_orgs{source="jira",state="available"} 1`,
		`tf_source_orgs{source="jira",state="unconfigured"} 0`,
		`tf_source_orgs{source="jira",state="disabled"} 1`,
		`tf_source_orgs{source="linear",state="available"} 0`,
		`tf_source_orgs{source="linear",state="unconfigured"} 2`,
		`tf_source_orgs{source="linear",state="disabled"} 0`,
	}
	assertLines(t, scrapeMetric(t, registry, sourceOrgsMetric), want)

	src.orgs, src.orgsErr = nil, errors.New("database unreachable")
	c.Tick(context.Background())
	assertLines(t, scrapeMetric(t, registry, sourceOrgsMetric), want)
}

// TestSourceOrgsObserver_CancelledTickKeepsThePreviousValues: a tick whose
// context is cancelled (the brain demoting mid-pass) changes nothing.
func TestSourceOrgsObserver_CancelledTickKeepsThePreviousValues(t *testing.T) {
	provider, registry := newScrapedProvider(t)
	src := &fakeSourceOrgsSource{
		orgs:  []string{"org-1"},
		avail: map[string]eventsource.Availability{"org-1": avail(stUnconfigured, stUnconfigured, stUnconfigured)},
	}
	c := NewSourceOrgsObserver(provider, src)
	defer c.Close()
	c.Tick(context.Background())
	before := scrapeMetric(t, registry, sourceOrgsMetric)

	src.orgs = []string{"org-1", "org-2"}
	src.avail["org-2"] = avail(stUnconfigured, stUnconfigured, stUnconfigured)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Tick(ctx)
	assertLines(t, scrapeMetric(t, registry, sourceOrgsMetric), before)
}

// TestSourceOrgsObserver_CloseStopsReporting is the lease-flap case, as for
// the connection observer.
func TestSourceOrgsObserver_CloseStopsReporting(t *testing.T) {
	provider, registry := newScrapedProvider(t)
	c := NewSourceOrgsObserver(provider, &fakeSourceOrgsSource{})
	c.Tick(context.Background())
	c.Close()
	if lines := scrapeMetric(t, registry, sourceOrgsMetric); len(lines) != 0 {
		t.Errorf("a closed observer still reports: %v", lines)
	}
}

// TestSourceOrgsStores_ReadsTheSourcesRouteDerivation runs the observer over
// real stores: the org's states are the ones eventsource.ResolveSystem
// answers, an active App of the org's own with no installation on its host is
// no_installations, and an installation there makes it available.
func TestSourceOrgsStores_ReadsTheSourcesRouteDerivation(t *testing.T) {
	keyring.MockInit()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	seedBYOAppCredentialClass(t, stores, org)
	if _, err := stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
		OrgID: org, AppID: "123", Slug: "acme-tf", ClientID: "Iv1.deadbeef", Active: true,
	}); err != nil {
		t.Fatalf("register app: %v", err)
	}
	if err := stores.Secrets.Put(ctx, org, integrations.KeyLinearAPIKey, "lin_api_x", ""); err != nil {
		t.Fatalf("store linear key: %v", err)
	}
	if _, err := stores.OrgEventSources.SetDisabled(ctx, org, eventsource.KindJira, true, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("turn jira off: %v", err)
	}

	provider, registry := newScrapedProvider(t)
	c := NewSourceOrgsObserver(provider, SourceOrgsStores(stores))
	defer c.Close()
	c.Tick(ctx)
	assertLines(t, scrapeMetric(t, registry, sourceOrgsMetric), []string{
		`tf_source_orgs{source="github",state="available"} 0`,
		`tf_source_orgs{source="github",state="no_installations"} 1`,
		`tf_source_orgs{source="github",state="unconfigured"} 0`,
		`tf_source_orgs{source="jira",state="available"} 0`,
		`tf_source_orgs{source="jira",state="unconfigured"} 0`,
		`tf_source_orgs{source="jira",state="disabled"} 1`,
		`tf_source_orgs{source="linear",state="available"} 1`,
		`tf_source_orgs{source="linear",state="unconfigured"} 0`,
		`tf_source_orgs{source="linear",state="disabled"} 0`,
	})

	if _, err := stores.GitHubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
		InstallationID: "42", OrgID: org, AccountType: "Organization", AccountLogin: "acme",
		GitHubHost: domain.GitHubHost("https://github.com"),
	}); err != nil {
		t.Fatalf("upsert installation: %v", err)
	}
	c.Tick(ctx)
	lines := scrapeMetric(t, registry, sourceOrgsMetric)
	for _, want := range []string{
		`tf_source_orgs{source="github",state="available"} 1`,
		`tf_source_orgs{source="github",state="no_installations"} 0`,
	} {
		found := false
		for _, line := range lines {
			found = found || line == want
		}
		if !found {
			t.Errorf("scrape lacks %q after the App was installed\nscrape:\n%v", want, lines)
		}
	}
}
