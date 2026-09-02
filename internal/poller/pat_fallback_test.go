package poller

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeResolver hands back the same client for every (org, target) — the
// resolver's own tier logic is exercised elsewhere; here we model "minting
// the installation token failed, so ClientFor returned the tier-3 PAT
// client" by giving the App loop a client whose installation-only endpoint
// 403s.
type fakeResolver struct {
	client *ghclient.Client
	err    error
}

func (f *fakeResolver) ClientFor(ctx context.Context, orgID, target string) (*ghclient.Client, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.client, nil
}

// ClientForRepo satisfies the ghclient.Resolver interface. The poller's
// cycle resolves account-grain via ClientFor; this repo-aware door is unused
// here, so it just delegates to keep the fake compiling against the interface.
func (f *fakeResolver) ClientForRepo(ctx context.Context, orgID, owner, repo string) (*ghclient.Client, error) {
	return f.ClientFor(ctx, orgID, owner)
}

// TokenFor satisfies the ghclient.Resolver interface. The poller
// never calls it (it works through *Client), so a zero Token is enough to
// keep the fake compiling against the interface.
func (f *fakeResolver) TokenFor(ctx context.Context, orgID, target string) (githubapp.Token, error) {
	if f.err != nil {
		return githubapp.Token{}, f.err
	}
	return githubapp.Token{}, nil
}

// BaseURLFor satisfies the ghclient.Resolver interface. The poller never
// calls it; the deployment default is enough to keep the fake compiling.
func (f *fakeResolver) BaseURLFor(ctx context.Context, orgID string) (string, error) {
	return ghbase.DefaultBaseURL(), nil
}

// OrgIdentityFor satisfies the ghclient.Resolver interface. The poller never
// resolves a commit identity; a no-identity result keeps the fake compiling.
func (f *fakeResolver) OrgIdentityFor(ctx context.Context, orgID string) (string, string, bool) {
	return "", "", false
}

// fakeInstallsStore embeds db.GitHubAppsStore (nil) and overrides the reads
// runGitHubCycleForOrg reaches in multi mode: GetForOrgSystem (the App
// registration, consulted by orgHasRegisteredApp to gate the App-vs-PAT path)
// and ListInstallationsForOrgSystem. A nil app field means "no App registered".
type fakeInstallsStore struct {
	db.GitHubAppsStore
	app      *domain.OrgGitHubApp
	installs []domain.OrgGitHubAppInstallation
}

func (f *fakeInstallsStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	return f.app, nil
}

func (f *fakeInstallsStore) ListInstallationsForOrgSystem(ctx context.Context, orgID string) ([]domain.OrgGitHubAppInstallation, error) {
	return append([]domain.OrgGitHubAppInstallation(nil), f.installs...), nil
}

// pollerTestServer is the shared GitHub stub: /installation/repositories 403s
// (model a non-installation-token client hitting the App-only endpoint), and
// the REST/GraphQL PR-discovery endpoints serve one open PR. A request to
// /pulls means the PAT path ran; a request to /installation/repositories means
// the App path ran. The default case fails the test on any unexpected path.
func pollerTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/installation/repositories"):
			http.Error(w, `{"message":"requires installation token"}`, http.StatusForbidden)
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			_, _ = w.Write([]byte(`{"data":{"nodes":[null]}}`))
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			w.Header().Set("ETag", `"etag-1"`)
			_, _ = w.Write([]byte(openPRBodyForPoller))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunGitHubCycleForOrg_StagedAppPollsViaPAT pins the either/or staging
// rule (TFAC-328): a registered-but-inactive App (mid PAT→App switch) keeps the
// PAT live, so the cycle must poll via the PAT path — the App path (and its
// /installation/repositories call) is never entered for a staged App.
func TestRunGitHubCycleForOrg_StagedAppPollsViaPAT(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti) // skip local-NAT backfill + username read

	srv := pollerTestServer(t)
	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})
	// Staged still means the org is in the BYO-App system — the class is
	// written at registration, and only the Active bit waits for cutover.
	seedBYOAppCredentialClass(t, stores, org)

	bus := eventbus.New()
	t.Cleanup(bus.Close)

	m := &Manager{
		database: database,
		pub:      busPublisher{bus: bus},
		tasks:    stores.Tasks,
		entities: stores.Entities,
		repos:    stores.Repos,
		orgs:     stores.Orgs,
		// A staged App: registered (a row exists) but active=false, plus an
		// installation that would otherwise pull us into the App path.
		apps: &fakeInstallsStore{
			app:      &domain.OrgGitHubApp{OrgID: org, AppID: "1", Active: false},
			installs: []domain.OrgGitHubAppInstallation{{InstallationID: "1", AccountLogin: "octo"}},
		},
		resolver: &fakeResolver{client: ghclient.NewClient(srv.URL, "pat")},
	}

	m.runGitHubCycleForOrg(ctx, org)

	ents, err := stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("got %d entities; want 1 — a staged App must poll the configured repo via the live PAT", len(ents))
	}
	if ents[0].SourceID != "octo/repo#42" {
		t.Errorf("entity SourceID = %q; want octo/repo#42", ents[0].SourceID)
	}
}

// TestRunGitHubCycleForOrg_ActiveAppNoFunctionalInstallationDegrades pins the
// removal of the cross-mode PAT fallback (TFAC-328): an ACTIVE App whose
// installation can't produce a usable token must surface degraded health and
// skip the cycle — never poll as a PAT (under XOR there is none). The stub's
// /installation/repositories 403s (no usable token); a /pulls request would
// mean the forbidden PAT fallback ran and fails the test.
func TestRunGitHubCycleForOrg_ActiveAppNoFunctionalInstallationDegrades(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	srv := pollerTestServer(t)
	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})
	seedBYOAppCredentialClass(t, stores, org)

	bus := eventbus.New()
	t.Cleanup(bus.Close)

	var reportedErr error
	m := &Manager{
		database: database,
		pub:      busPublisher{bus: bus},
		tasks:    stores.Tasks,
		entities: stores.Entities,
		repos:    stores.Repos,
		orgs:     stores.Orgs,
		apps: &fakeInstallsStore{
			app:      &domain.OrgGitHubApp{OrgID: org, AppID: "1", Active: true},
			installs: []domain.OrgGitHubAppInstallation{{InstallationID: "1", AccountLogin: "octo"}},
		},
		resolver: &fakeResolver{client: ghclient.NewClient(srv.URL, "pat")},
		OnError:  func(source, orgID string, err error) { reportedErr = err },
	}

	m.runGitHubCycleForOrg(ctx, org)

	ents, err := stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("got %d entities; want 0 — an active App with no usable installation token must not fall back to a PAT", len(ents))
	}
	if reportedErr == nil {
		t.Fatal("OnError was not called; an active App that can't mint a usable token must report degraded health")
	}
}

const openPRBodyForPoller = `[
  {
    "number": 42,
    "node_id": "PR_kwDOabc",
    "title": "Add widget",
    "state": "open",
    "html_url": "https://github.com/octo/repo/pull/42",
    "updated_at": "2026-05-29T12:00:00Z",
    "user": {"login": "alice"},
    "head": {"sha": "deadbeef", "ref": "feature", "repo": {"full_name": "octo/repo"}},
    "base": {"ref": "main", "repo": {"full_name": "octo/repo"}}
  }
]`

func newMigratedSQLiteForPoller(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

// seedBYOAppCredentialClass records that the org is in the BYO-App credential
// system — what registering or importing an App writes in the same transaction
// as the registration row itself.
//
// A fixture that hands the Manager a fake apps store holding a registration
// must seed this too, or it is modelling a state the product cannot reach: an
// org with an App and a class saying it has none. The poll cycle dispatches on
// the class, so without it such a fixture polls as a PAT.
func seedBYOAppCredentialClass(t *testing.T, stores db.Stores, orgID string) {
	t.Helper()
	if _, err := stores.Orgs.SetGitHubCredentialClass(context.Background(), orgID, domain.GitHubCredentialClassBYOApp); err != nil {
		t.Fatalf("SetGitHubCredentialClass: %v", err)
	}
}

// trackRepos records the org's tracked repo set on the local default team —
// the seed every poll fixture needs now that "which repos does TF poll" is a
// question about tracking rather than a listing of the repository registry.
func trackRepos(t *testing.T, stores db.Stores, orgID string, names []string) {
	t.Helper()
	repos := make([]domain.TeamGitHubRepo, 0, len(names))
	for _, name := range names {
		owner, repo, ok := strings.Cut(name, "/")
		if !ok {
			t.Fatalf("trackRepos: %q is not an owner/repo slug", name)
		}
		repos = append(repos, domain.TeamGitHubRepo{Owner: owner, Repo: repo})
	}
	if err := stores.TeamGitHubRepos.ReplaceForTeam(context.Background(), orgID, runmode.LocalDefaultTeamID, repos); err != nil {
		t.Fatalf("track repos %v: %v", names, err)
	}
}
