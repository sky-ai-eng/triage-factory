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

// TokenFor satisfies the ghclient.Resolver interface. The poller
// never calls it (it works through *Client), so a zero Token is enough to
// keep the fake compiling against the interface.
func (f *fakeResolver) TokenFor(ctx context.Context, orgID, target string) (githubapp.Token, error) {
	if f.err != nil {
		return githubapp.Token{}, f.err
	}
	return githubapp.Token{}, nil
}

// fakeInstallsStore embeds db.GitHubAppsStore (nil) and overrides only
// ListInstallationsForOrgSystem — the only method runGitHubCycleForOrg
// reaches in multi mode (the local-NAT backfill + orgHasRegisteredApp reads
// are gated to local mode).
type fakeInstallsStore struct {
	db.GitHubAppsStore
	installs []domain.OrgGitHubAppInstallation
}

func (f *fakeInstallsStore) ListInstallationsForOrgSystem(ctx context.Context, orgID string) ([]domain.OrgGitHubAppInstallation, error) {
	return append([]domain.OrgGitHubAppInstallation(nil), f.installs...), nil
}

// TestRunGitHubCycleForOrg_PATFallbackWhenInstallationTokenUnusable pins the
// reviewer's case: an org HAS an installation, but minting its token fails
// and the resolver hands back a PAT client. The installation-only
// /installation/repositories 403s, so the App loop covers nothing — and
// because a PAT is available the org must still be polled via the PAT path
// (REST open-PR enumeration), not left dark.
func TestRunGitHubCycleForOrg_PATFallbackWhenInstallationTokenUnusable(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti) // skip local-NAT backfill + username read

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/installation/repositories"):
			// A PAT cannot use the installation endpoint — 403, exactly as
			// happens when ClientFor falls back to the PAT after a mint failure.
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

	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	if err := stores.Repos.SetConfigured(ctx, org, []string{"octo/repo"}); err != nil {
		t.Fatalf("SetConfigured: %v", err)
	}

	bus := eventbus.New()
	t.Cleanup(bus.Close)

	m := &Manager{
		database: database,
		pub:      bus,
		tasks:    stores.Tasks,
		entities: stores.Entities,
		repos:    stores.Repos,
		apps:     &fakeInstallsStore{installs: []domain.OrgGitHubAppInstallation{{InstallationID: "1", AccountLogin: "octo"}}},
		resolver: &fakeResolver{client: ghclient.NewClient(srv.URL, "pat")},
	}

	m.runGitHubCycleForOrg(ctx, org)

	ents, err := stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("got %d entities; want 1 — the PAT fallback must poll the configured repo when the installation token is unusable", len(ents))
	}
	if ents[0].SourceID != "octo/repo#42" {
		t.Errorf("entity SourceID = %q; want octo/repo#42", ents[0].SourceID)
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
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
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
