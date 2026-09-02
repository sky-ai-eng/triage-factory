package grantmirror

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// The cadence pass for the managed installation set, end to end against real
// stores and a fake GitHub. The per-org grant pass is driven through fakes in
// reconcile_test.go because what it decides is a matter of control flow; these
// cases are about what the row converging BUYS — a token that mints again, a
// grant that refreshes again — so they run the real store and, where the
// property is minting, the real resolver.

// countingApps counts the deployment-wide refresh and answers it with a fixed
// error. Every other method panics: RunDeployment must reach the store through
// exactly this door.
type countingApps struct {
	db.GitHubAppsStore
	refreshes atomic.Int64
	err       error
}

func (c *countingApps) RefreshAllManagedInstallations(context.Context, githubapp.DeploymentApp) error {
	c.refreshes.Add(1)
	return c.err
}

func TestRunDeployment_NoDeploymentAppIsANoOp(t *testing.T) {
	// Every local process and every multi deployment whose orgs all bring their
	// own key: nothing could be listed, so nothing is asked of the store either.
	apps := &countingApps{}
	r := &Reconciler{apps: apps}

	if err := r.RunDeployment(context.Background()); err != nil {
		t.Fatalf("RunDeployment with no deployment App: %v; want nil", err)
	}
	if got := apps.refreshes.Load(); got != 0 {
		t.Errorf("store refreshed %d times with no deployment App; want 0", got)
	}
}

func TestRunDeployment_RunsTheStoreRefreshUnderTheDeploymentApp(t *testing.T) {
	apps := &countingApps{err: errors.New("github unreachable")}
	r := &Reconciler{apps: apps, deployment: testDeploymentApp(t)}

	err := r.RunDeployment(context.Background())
	if err == nil {
		t.Fatal("RunDeployment swallowed the store's error; want it surfaced")
	}
	if got := apps.refreshes.Load(); got != 1 {
		t.Errorf("store refreshed %d times; want 1", got)
	}
}

func TestRunDeployment_ARenameResolvesMintingAgain(t *testing.T) {
	// The consequence with no push signal at all. Minting resolves an
	// installation by account LOGIN, and the login is written by exactly two
	// things: the bind, once, and the listing. After a rename upstream the bound
	// row still says the old name, so every mint for the account fails — until
	// the cadence brings the listing in. The property asserted is the token,
	// not the row: a row converging while minting stays broken would be a pass
	// that fixed the wrong thing.
	ctx := context.Background()
	gh := newDeploymentGH(t)
	app := testDeploymentApp(t)
	stores, org := newManagedWorkspace(t, gh.srv.URL)
	bindInstallation(t, stores, org, gh.srv.URL, "456", "1000", "acme")
	gh.setListing(t, listedInstallation{ID: 456, AccountID: 1000, Login: "acme-renamed"})

	resolver := github.NewResolver(stores.Secrets, stores.GitHubApps, stores.Orgs, stores.Agents, nil, github.WithDeploymentApp(app))
	if _, err := resolver.TokenFor(ctx, org, "acme-renamed"); !errors.Is(err, github.ErrNoGitHubCredentials) {
		t.Fatalf("TokenFor(renamed account) before the pass: err = %v; want ErrNoGitHubCredentials — the fixture must start broken", err)
	}

	r := NewReconciler(stores.GitHubApps, stores.ReachableRepos, resolver, fakeClasses{class: domain.GitHubCredentialClassManagedApp}, app)
	if err := r.RunDeployment(ctx); err != nil {
		t.Fatalf("RunDeployment: %v", err)
	}

	tok, err := resolver.TokenFor(ctx, org, "acme-renamed")
	if err != nil {
		t.Fatalf("TokenFor(renamed account) after the pass: %v; want a minted token", err)
	}
	if tok.Value != "ghs_deployment_minted" {
		t.Errorf("TokenFor = %q; want the token minted from the deployment App", tok.Value)
	}
	if got := gh.mintCalls.Load(); got != 1 {
		t.Errorf("installation token minted %d times; want 1", got)
	}
	if got := gh.listCalls.Load(); got != 1 {
		t.Errorf("listing walked %d times; want 1", got)
	}
}

func TestRunDeployment_ALostUnsuspendUnblocksTheGrantPass(t *testing.T) {
	// The deadlock. RunOrg skips a suspended installation's grant — correctly,
	// since a 403 is not evidence of a narrowed grant — and suspension clears
	// only through a delivery or the listing. With the unsuspend delivery lost,
	// the row stays suspended and the grant stays stale, forever, unless
	// something pulls. This is the something: after one cadence pass the next
	// RunOrg reconciles the grant it had been skipping.
	ctx := context.Background()
	gh := newDeploymentGH(t)
	app := testDeploymentApp(t)
	stores, org := newManagedWorkspace(t, gh.srv.URL)
	bindInstallation(t, stores, org, gh.srv.URL, "456", "1000", "acme")
	if _, err := stores.GitHubApps.SetInstallationSuspension(ctx, org, "456", time.Now().Add(-time.Hour), "owner"); err != nil {
		t.Fatalf("suspend the installation: %v", err)
	}
	// GitHub, meanwhile, has lifted it — and no delivery said so.
	gh.setListing(t, listedInstallation{ID: 456, AccountID: 1000, Login: "acme"})

	mirror := newFakeMirror()
	r := &Reconciler{
		apps:       stores.GitHubApps,
		mirror:     mirror,
		clients:    fakeGrants{byLogin: map[string]grantAnswer{"acme": {repos: []github.UserRepo{repo("acme/api", 1)}, complete: true}}},
		classes:    fakeClasses{class: domain.GitHubCredentialClassManagedApp},
		deployment: app,
	}

	if err := r.RunOrg(ctx, org); err != nil {
		t.Fatalf("RunOrg before the pass: %v", err)
	}
	if mirror.replaces != 0 {
		t.Fatalf("grant reconciled %d times while the row read suspended; want 0 — the fixture must start deadlocked", mirror.replaces)
	}

	if err := r.RunDeployment(ctx); err != nil {
		t.Fatalf("RunDeployment: %v", err)
	}
	if err := r.RunOrg(ctx, org); err != nil {
		t.Fatalf("RunOrg after the pass: %v", err)
	}
	if mirror.replaces != 1 {
		t.Errorf("grant reconciled %d times after the suspension cleared; want 1", mirror.replaces)
	}
	if got := mirror.rows["456"]; len(got) != 1 || got[0] != "acme/api" {
		t.Errorf("mirror for 456 = %v; want [acme/api]", got)
	}
	if got := mirror.classes["456"]; got != domain.GitHubCredentialClassManagedApp {
		t.Errorf("mirror class for 456 = %q; want managed_app", got)
	}
}

// listedInstallation is one installation in the fake GitHub's listing.
type listedInstallation struct {
	ID        int64
	AccountID int64
	Login     string
}

// deploymentGH is the fake GitHub the deployment App talks to: the listing the
// cadence pass reads, and the three calls the resolver's managed tier makes to
// mint a token (the App preflight, the bot-user read, the mint itself).
type deploymentGH struct {
	srv       *httptest.Server
	mu        sync.Mutex
	listing   string
	listCalls atomic.Int64
	mintCalls atomic.Int64
}

func newDeploymentGH(t *testing.T) *deploymentGH {
	t.Helper()
	g := &deploymentGH{listing: "[]"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/app/installations", func(w http.ResponseWriter, r *http.Request) {
		g.listCalls.Add(1)
		if r.Header.Get("Authorization") == "" {
			t.Errorf("listing request carried no App JWT")
		}
		g.mu.Lock()
		body := g.listing
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("GET /api/v3/app", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          987,
			"slug":        "tf-deployment",
			"client_id":   "Iv1.deployment",
			"owner":       map[string]any{"login": "tf-inc", "type": "Organization"},
			"permissions": map[string]string{"members": "read", "contents": "write"},
		})
	})
	mux.HandleFunc("GET /api/v3/users/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 424242})
	})
	mux.HandleFunc("POST /api/v3/app/installations/", func(w http.ResponseWriter, _ *http.Request) {
		g.mintCalls.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_deployment_minted",
			"expires_at": time.Now().Add(time.Hour).UTC(),
		})
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// setListing renders items as GET /app/installations does, none suspended.
func (g *deploymentGH) setListing(t *testing.T, items ...listedInstallation) {
	t.Helper()
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"id":                   it.ID,
			"account":              map[string]any{"id": it.AccountID, "login": it.Login, "type": "Organization"},
			"repository_selection": domain.RepositorySelectionSelected,
			"suspended_at":         nil,
			"suspended_by":         nil,
		})
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("render listing: %v", err)
	}
	g.mu.Lock()
	g.listing = string(body)
	g.mu.Unlock()
}

func testDeploymentApp(t *testing.T) githubapp.DeploymentApp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return githubapp.DeploymentApp{AppID: 987, PrivateKey: key, WebhookSecret: "wh", ClientSecret: "cs"}
}

// newManagedWorkspace opens a migrated in-memory store bundle holding one org
// on the managed class, pointed at the fake GitHub.
func newManagedWorkspace(t *testing.T, baseURL string) (db.Stores, string) {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	stores := sqlitestore.New(conn)

	org := uuid.NewString()
	if _, err := conn.Exec(`INSERT INTO orgs (id, slug, name) VALUES (?, ?, ?)`, org, org, "org-"+org[:8]); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := stores.Orgs.SetGitHubCredentialClass(context.Background(), org, domain.GitHubCredentialClassManagedApp); err != nil {
		t.Fatalf("set credential class: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'github', ?)
		ON CONFLICT(org_id, kind) DO UPDATE SET base_url = excluded.base_url
	`, org, baseURL); err != nil {
		t.Fatalf("seed github base url: %v", err)
	}
	return stores, org
}

// bindInstallation writes the row the bind ceremony would have written.
func bindInstallation(t *testing.T, stores db.Stores, org, baseURL, installationID, accountID, login string) {
	t.Helper()
	if _, err := stores.GitHubApps.UpsertInstallation(context.Background(), domain.OrgGitHubAppInstallation{
		InstallationID: installationID,
		OrgID:          org,
		AccountType:    "Organization",
		AccountID:      accountID,
		AccountLogin:   login,
		GitHubHost:     db.EffectiveGitHubHost(baseURL),
	}); err != nil {
		t.Fatalf("bind installation %s: %v", installationID, err)
	}
}
