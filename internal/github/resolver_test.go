package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// --- fakes (embed the interface so unexercised methods compile-satisfy and
// panic only if the resolver unexpectedly calls them) ---

type fakeSecrets struct {
	db.SecretStore
	vals map[string]string
	err  error
}

func (f *fakeSecrets) GetSystem(_ context.Context, _ string, key string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.vals[key], nil
}

type fakeApps struct {
	db.GitHubAppsStore
	app     *domain.OrgGitHubApp
	insts   []domain.OrgGitHubAppInstallation
	listErr error // when set, the installation mirror is unreadable
}

func (f *fakeApps) GetForOrgSystem(_ context.Context, _ string) (*domain.OrgGitHubApp, error) {
	return f.app, nil
}

func (f *fakeApps) ListInstallationsForOrgSystem(_ context.Context, _ string) ([]domain.OrgGitHubAppInstallation, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.insts, nil
}

type fakeOrgs struct {
	db.OrgsStore
	base string
	// class is the org's GitHub credential class. Empty is not a value the real
	// store can return (the column is NOT NULL DEFAULT 'pat', and a missing row
	// falls back to domain.DefaultOrgSettings) — newTestResolver fills it in
	// from the fixture's App before the resolver ever reads it.
	class domain.GitHubCredentialClass
	// settingsErr makes the settings row unreadable, the state the resolver
	// must refuse to guess past rather than fall back to an inference.
	settingsErr error
	// settingsReads counts reads of the org_settings row. The host and the
	// credential class both live on it, and one resolution must not fetch it
	// twice — see TestResolver_ResolvesOrgSettingsOncePerCall.
	settingsReads int
}

func (f *fakeOrgs) GetSettingsSystem(_ context.Context, _ string) (domain.OrgSettings, error) {
	f.settingsReads++
	if f.settingsErr != nil {
		return domain.OrgSettings{}, f.settingsErr
	}
	return domain.OrgSettings{GitHubBaseURL: f.base, GitHubCredentialClass: f.class}, nil
}

// newTestResolver wires a resolver from the package's fakes, defaulting the
// org's credential class to the one the fixture's App presence implies: an
// org_github_apps row means byo_app, no row means pat.
//
// That pairing is not a convenience — it is the production invariant. The
// registration and the class are written in one transaction, so an org with an
// App always has class byo_app (staged or live), and a fixture that names an
// App without it would be modelling a state the product cannot reach. A test
// that wants the two to disagree sets fakeOrgs.class explicitly and this leaves
// it alone.
func newTestResolver(secrets db.SecretStore, apps *fakeApps, orgs *fakeOrgs, agents db.AgentStore, cache TokenCache) Resolver {
	if orgs.class == "" {
		orgs.class = domain.GitHubCredentialClassPAT
		if apps != nil && apps.app != nil {
			orgs.class = domain.GitHubCredentialClassBYOApp
		}
	}
	return NewResolver(secrets, apps, orgs, agents, cache)
}

type fakeAgents struct {
	db.AgentStore
	agent *domain.Agent
}

func (f *fakeAgents) GetForOrgSystem(_ context.Context, _ string) (*domain.Agent, error) {
	return f.agent, nil
}

func testPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

// ghTestServer stands in for GitHub's REST API: it mints installation
// tokens and records what the returned client subsequently sends, so a test
// can assert which credential (App token vs PAT) the resolver handed back.
type ghTestServer struct {
	srv          *httptest.Server
	mintCalls    int32
	lastProbe    string   // Authorization header seen on the /probe call
	installRepos []string // full names the repo-access probe treats as granted
	repoProbes   int32    // count of GET /repos/{owner}/{repo} coverage probes
	repoProbe5xx bool     // when true, the coverage probe returns 500 (indeterminate)

	// mintedFor records the installation ids mints were requested against, in
	// order. It grows inside the handler goroutine, so unlike the scalars above
	// it takes a lock on both sides — an append that reallocates while a test
	// reads the slice header is a race whatever the ordering happens to be.
	mu        sync.Mutex
	mintedFor []string
}

// minted returns a copy of the installation ids minted so far, so an assertion
// never holds a slice the handler can still append to.
func (g *ghTestServer) minted() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.mintedFor)
}

func newGHTestServer(t *testing.T) *ghTestServer {
	t.Helper()
	g := &ghTestServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&g.mintCalls, 1)
		// POST /app/installations/{id}/access_tokens — the id is which account
		// the minted token will act as, so recording it lets a test assert the
		// resolver picked the installation it meant to.
		id, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/v3/app/installations/"), "/")
		g.mu.Lock()
		g.mintedFor = append(g.mintedFor, id)
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_minted",
			"expires_at": time.Now().Add(time.Hour).UTC(),
		})
	})
	mux.HandleFunc("/api/v3/probe", func(w http.ResponseWriter, r *http.Request) {
		g.lastProbe = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	})
	// Repo-access probe (ClientForRepo's coverage check via CheckRepoAccess):
	// GET /repos/{owner}/{repo} → 200 if the repo is in the installation's
	// grant (installRepos), else 404 like an installation token sees for a
	// repo outside its "Selected repositories" scope.
	mux.HandleFunc("/api/v3/repos/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&g.repoProbes, 1)
		if g.repoProbe5xx {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/api/v3/repos/")
		for _, granted := range g.installRepos {
			if granted == name {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"full_name":"` + name + `"}`))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func activeApp() *domain.OrgGitHubApp {
	return &domain.OrgGitHubApp{OrgID: "org-1", AppID: "123", PEMRef: "pem", Active: true}
}

func installOn(login string) domain.OrgGitHubAppInstallation {
	return domain.OrgGitHubAppInstallation{InstallationID: "456", OrgID: "org-1", AccountType: "Organization", AccountLogin: login}
}

func TestResolver_Tier3_PATWhenNoApp(t *testing.T) {
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: nil}, // no registered App
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	client, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghp_test" {
		t.Errorf("client carried %q, want the PAT", gh.lastProbe)
	}
	if gh.mintCalls != 0 {
		t.Errorf("mint was called %d times for a PAT-only org", gh.mintCalls)
	}
}

func TestResolver_Tier1_AppInstallationToken(t *testing.T) {
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	client, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghs_minted" {
		t.Errorf("client carried %q, want the minted App token", gh.lastProbe)
	}
	if gh.mintCalls != 1 {
		t.Errorf("mint called %d times, want 1", gh.mintCalls)
	}

	// Second resolution for the same installation must hit the cache, not re-mint.
	if _, err := r.ClientFor(context.Background(), "org-1", "acme"); err != nil {
		t.Fatalf("second ClientFor: %v", err)
	}
	if gh.mintCalls != 1 {
		t.Errorf("mint called %d times after a cached resolution, want 1", gh.mintCalls)
	}
}

// ClientForRepo: the org's App is installed on the account AND the repo is in
// the installation's grant → the minted App token is used.
func TestResolver_ClientForRepo_AppCoversRepo(t *testing.T) {
	gh := newGHTestServer(t)
	gh.installRepos = []string{"acme/widget", "acme/gadget"}
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	client, err := r.ClientForRepo(context.Background(), "org-1", "acme", "widget")
	if err != nil {
		t.Fatalf("ClientForRepo: %v", err)
	}
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghs_minted" {
		t.Errorf("client carried %q, want the minted App token", gh.lastProbe)
	}
}

// ClientForRepo: the App is installed on the account but the repo is NOT in the
// grant (a "Selected repositories" install). App XOR PAT — an active-App org's
// credential is the App, so a repo the App can't reach is an error (surface the
// misconfig: grant the App the repo), NOT a silent borrow of the stale PAT that
// happens to be present here.
func TestResolver_ClientForRepo_AppDoesNotCoverRepo_Errors(t *testing.T) {
	gh := newGHTestServer(t)
	gh.installRepos = []string{"acme/widget"} // "acme/other" is not granted
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	if _, err := r.ClientForRepo(context.Background(), "org-1", "acme", "other"); !errors.Is(err, ErrNoGitHubCredentials) {
		t.Fatalf("ClientForRepo err = %v, want ErrNoGitHubCredentials (active App can't reach the repo; no PAT fallback)", err)
	}
}

// ClientForRepo: the App is installed but the coverage probe is indeterminate
// (GitHub 5xx). The contract is fail-open — return the minted App client rather
// than discarding it for the PAT — and the indeterminate result must NOT be
// cached, so a transient outage can't pin a wrong coverage answer.
func TestResolver_ClientForRepo_CoverageProbeIndeterminate_FailsOpen(t *testing.T) {
	gh := newGHTestServer(t)
	gh.repoProbe5xx = true // GET /repos/{owner}/{repo} → 500
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	client, err := r.ClientForRepo(context.Background(), "org-1", "acme", "widget")
	if err != nil {
		t.Fatalf("ClientForRepo: %v", err)
	}
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghs_minted" {
		t.Errorf("client carried %q, want the minted App token (fail-open on indeterminate)", gh.lastProbe)
	}

	// A second resolution must re-probe (the indeterminate result wasn't
	// cached), not serve a stale decision.
	if _, err := r.ClientForRepo(context.Background(), "org-1", "acme", "widget"); err != nil {
		t.Fatalf("second ClientForRepo: %v", err)
	}
	if got := atomic.LoadInt32(&gh.repoProbes); got != 2 {
		t.Errorf("coverage probes = %d, want 2 (indeterminate must not be cached)", got)
	}
}

// ClientForRepo memoizes a conclusive coverage answer: two resolutions for the
// same repo probe GitHub only once.
func TestResolver_ClientForRepo_CachesCoverage(t *testing.T) {
	gh := newGHTestServer(t)
	gh.installRepos = []string{"acme/widget"}
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	for i := 0; i < 3; i++ {
		if _, err := r.ClientForRepo(context.Background(), "org-1", "acme", "widget"); err != nil {
			t.Fatalf("ClientForRepo #%d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&gh.repoProbes); got != 1 {
		t.Errorf("coverage probes = %d, want 1 (positive decision should be memoized)", got)
	}
}

// ClientForRepo must NOT cache a non-coverage answer: a repo newly added to a
// selective grant has to be picked up on the next call, not pinned to
// ErrNoGitHubCredentials for a whole TTL. So each not-covered resolution
// re-probes (and, under App XOR PAT, errors rather than borrowing a PAT).
func TestResolver_ClientForRepo_DoesNotCacheNonCoverage(t *testing.T) {
	gh := newGHTestServer(t)
	gh.installRepos = []string{"acme/widget"} // "acme/other" not granted
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	for i := 0; i < 2; i++ {
		if _, err := r.ClientForRepo(context.Background(), "org-1", "acme", "other"); !errors.Is(err, ErrNoGitHubCredentials) {
			t.Fatalf("ClientForRepo #%d err = %v, want ErrNoGitHubCredentials (App can't reach the repo)", i, err)
		}
	}
	if got := atomic.LoadInt32(&gh.repoProbes); got != 2 {
		t.Errorf("coverage probes = %d, want 2 (non-coverage must not be cached)", got)
	}
}

// A cached positive coverage decision expires after repoCoverageTTL: once the
// clock advances past it, the next resolution re-probes rather than serving a
// stale "covered" answer (which would 403 if the grant had since dropped it).
func TestResolver_ClientForRepo_CoverageExpires(t *testing.T) {
	gh := newGHTestServer(t)
	gh.installRepos = []string{"acme/widget"}
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)
	// Drive the cache's clock so the TTL boundary is exercised deterministically.
	clock := time.Now()
	r.(*resolver).coverage.now = func() time.Time { return clock }

	if _, err := r.ClientForRepo(context.Background(), "org-1", "acme", "widget"); err != nil {
		t.Fatalf("ClientForRepo (populate): %v", err)
	}
	// Still inside the TTL → cache hit, no re-probe.
	clock = clock.Add(repoCoverageTTL - time.Second)
	if _, err := r.ClientForRepo(context.Background(), "org-1", "acme", "widget"); err != nil {
		t.Fatalf("ClientForRepo (fresh): %v", err)
	}
	if got := atomic.LoadInt32(&gh.repoProbes); got != 1 {
		t.Fatalf("coverage probes = %d before expiry, want 1", got)
	}
	// Past the TTL → entry expired, re-probe.
	clock = clock.Add(2 * time.Second)
	if _, err := r.ClientForRepo(context.Background(), "org-1", "acme", "widget"); err != nil {
		t.Fatalf("ClientForRepo (expired): %v", err)
	}
	if got := atomic.LoadInt32(&gh.repoProbes); got != 2 {
		t.Errorf("coverage probes = %d after expiry, want 2 (expired entry must re-probe)", got)
	}
}

// ClientForRepo with no App falls straight through to the PAT, exactly like
// ClientFor — the repo-grain check only adds work when an App is installed.
func TestResolver_ClientForRepo_NoApp_PAT(t *testing.T) {
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: nil},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	client, err := r.ClientForRepo(context.Background(), "org-1", "acme", "widget")
	if err != nil {
		t.Fatalf("ClientForRepo: %v", err)
	}
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghp_test" {
		t.Errorf("client carried %q, want the PAT", gh.lastProbe)
	}
	if gh.mintCalls != 0 {
		t.Errorf("mint was called %d times for a PAT-only org", gh.mintCalls)
	}
}

// ClientForRepoWithIdentity reports the credential it resolved as an Identity,
// so the agenthost pending-review collision check can branch on App-vs-PAT
// without re-deriving it from the opaque bearer token. App XOR PAT: an active
// App covering the repo → IdentityApp; an active App that can't reach the repo
// → error (no PAT fallback); no active App → IdentityPAT; nothing resolves →
// IdentityUnknown alongside the error.
func TestResolver_ClientForRepoWithIdentity_ReportsTier(t *testing.T) {
	t.Run("app covers repo → IdentityApp", func(t *testing.T) {
		gh := newGHTestServer(t)
		gh.installRepos = []string{"acme/widget"}
		r := newTestResolver(
			&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
			&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
			&fakeOrgs{base: gh.srv.URL},
			&fakeAgents{},
			nil,
		)
		client, identity, err := r.(RepoIdentityResolver).ClientForRepoWithIdentity(context.Background(), "org-1", "acme", "widget")
		if err != nil {
			t.Fatalf("ClientForRepoWithIdentity: %v", err)
		}
		if identity != IdentityApp {
			t.Errorf("identity = %v, want IdentityApp (tier 1)", identity)
		}
		if _, err := client.Get(context.Background(), "/probe"); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if gh.lastProbe != "Bearer ghs_minted" {
			t.Errorf("client carried %q, want the minted App token alongside IdentityApp", gh.lastProbe)
		}
	})

	t.Run("active App does not cover repo → error, no PAT", func(t *testing.T) {
		gh := newGHTestServer(t)
		gh.installRepos = []string{"acme/widget"} // "acme/other" not granted
		r := newTestResolver(
			&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
			&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
			&fakeOrgs{base: gh.srv.URL},
			&fakeAgents{},
			nil,
		)
		_, identity, err := r.(RepoIdentityResolver).ClientForRepoWithIdentity(context.Background(), "org-1", "acme", "other")
		if !errors.Is(err, ErrNoGitHubCredentials) {
			t.Fatalf("err = %v, want ErrNoGitHubCredentials (active App can't reach the repo; no PAT fallback)", err)
		}
		if identity != IdentityUnknown {
			t.Errorf("identity = %v, want IdentityUnknown on the error path", identity)
		}
	})

	t.Run("no app → IdentityPAT", func(t *testing.T) {
		gh := newGHTestServer(t)
		r := newTestResolver(
			&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}},
			&fakeApps{app: nil},
			&fakeOrgs{base: gh.srv.URL},
			&fakeAgents{},
			nil,
		)
		_, identity, err := r.(RepoIdentityResolver).ClientForRepoWithIdentity(context.Background(), "org-1", "acme", "widget")
		if err != nil {
			t.Fatalf("ClientForRepoWithIdentity: %v", err)
		}
		if identity != IdentityPAT {
			t.Errorf("identity = %v, want IdentityPAT", identity)
		}
	})

	t.Run("no credentials → IdentityUnknown + error", func(t *testing.T) {
		gh := newGHTestServer(t)
		r := newTestResolver(
			&fakeSecrets{vals: map[string]string{}}, // no PAT, no App
			&fakeApps{app: nil},
			&fakeOrgs{base: gh.srv.URL},
			&fakeAgents{},
			nil,
		)
		_, identity, err := r.(RepoIdentityResolver).ClientForRepoWithIdentity(context.Background(), "org-1", "acme", "widget")
		if !errors.Is(err, ErrNoGitHubCredentials) {
			t.Fatalf("err = %v, want ErrNoGitHubCredentials", err)
		}
		if identity != IdentityUnknown {
			t.Errorf("identity = %v, want IdentityUnknown when nothing resolves", identity)
		}
	})
}

// TokenFor must hand back the same App installation token ClientFor would
// authenticate with (tier 1), and share the mint cache so the host-side clone
// and the API client don't each mint a separate token.
func TestResolver_TokenFor_Tier1_SharesMintCacheWithClientFor(t *testing.T) {
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	tok, err := r.TokenFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("TokenFor: %v", err)
	}
	if tok.Value != "ghs_minted" {
		t.Errorf("TokenFor returned %q, want the minted App token", tok.Value)
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("tier-1 token should carry the mint expiry, got zero")
	}
	if gh.mintCalls != 1 {
		t.Errorf("mint called %d times, want 1", gh.mintCalls)
	}

	// A subsequent ClientFor for the same installation must reuse the cached
	// token rather than minting a second one — the whole point of routing both
	// through installationToken + TokenCache.
	if _, err := r.ClientFor(context.Background(), "org-1", "acme"); err != nil {
		t.Fatalf("ClientFor after TokenFor: %v", err)
	}
	if gh.mintCalls != 1 {
		t.Errorf("mint called %d times after a cached resolution, want 1 (shared cache)", gh.mintCalls)
	}
}

// With no App, TokenFor falls through to the PAT tier and returns it as the
// credential, with a zero expiry (PATs have no mint lifetime).
func TestResolver_TokenFor_Tier3_PAT(t *testing.T) {
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: nil},
		&fakeOrgs{base: "https://github.com"},
		&fakeAgents{},
		nil,
	)
	tok, err := r.TokenFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("TokenFor: %v", err)
	}
	if tok.Value != "ghp_test" {
		t.Errorf("TokenFor returned %q, want the org PAT", tok.Value)
	}
	if !tok.ExpiresAt.IsZero() {
		t.Errorf("tier-3 PAT token should have a zero expiry, got %v", tok.ExpiresAt)
	}
}

// No App and no PAT → ErrNoGitHubCredentials, same sentinel ClientFor returns
// so callers can distinguish "not configured" from a backend error.
func TestResolver_TokenFor_NoCredentials(t *testing.T) {
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{}},
		&fakeApps{app: nil},
		&fakeOrgs{base: "https://github.com"},
		&fakeAgents{},
		nil,
	)
	if _, err := r.TokenFor(context.Background(), "org-1", "acme"); !errors.Is(err, ErrNoGitHubCredentials) {
		t.Errorf("got %v, want ErrNoGitHubCredentials", err)
	}
}

// A secret-store read failure on the PAT tier must propagate, not be
// misreported as ErrNoGitHubCredentials — mirrors ClientFor's contract.
func TestResolver_TokenFor_PATReadError_Propagates(t *testing.T) {
	r := newTestResolver(
		&fakeSecrets{err: errVaultDown},
		&fakeApps{app: nil},
		&fakeOrgs{base: "https://github.com"},
		&fakeAgents{},
		nil,
	)
	_, err := r.TokenFor(context.Background(), "org-1", "acme")
	if err == nil || errors.Is(err, ErrNoGitHubCredentials) {
		t.Fatalf("want a propagated backend error, got %v", err)
	}
	if !errors.Is(err, errVaultDown) {
		t.Errorf("error should wrap the secret-store failure, got %v", err)
	}
}

func TestResolver_Tier1_EmptyTargetSingleInstall(t *testing.T) {
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t)}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)
	if _, err := r.ClientFor(context.Background(), "org-1", ""); err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if gh.mintCalls != 1 {
		t.Errorf("empty target + single install should mint (tier1); mintCalls=%d", gh.mintCalls)
	}
}

// An ambiguous empty target with multiple installs can't pick an installation.
// App XOR PAT — an active-App org errors rather than borrowing the (stale) PAT
// — and the error says ambiguous rather than no-credentials, since a workspace
// told its GitHub is disconnected re-authorizes a connection that was fine.
func TestResolver_EmptyTargetMultiInstall_Errors(t *testing.T) {
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme"), installOn("globex")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)
	_, err := r.ClientFor(context.Background(), "org-1", "")
	if !errors.Is(err, ErrAmbiguousInstallation) {
		t.Fatalf("ClientFor err = %v, want ErrAmbiguousInstallation (empty target, two installations)", err)
	}
	if errors.Is(err, ErrNoGitHubCredentials) {
		t.Errorf("ClientFor err = %v; ambiguity must not read as missing credentials", err)
	}
	if gh.mintCalls != 0 {
		t.Errorf("ambiguous empty target should not mint; mintCalls=%d", gh.mintCalls)
	}
}

// The active App has no installation on the target account. App XOR PAT — error
// (the App can't serve it, and an active-App org doesn't borrow a PAT), rather
// than the prior silent PAT fallback.
func TestResolver_TargetNoMatch_Errors(t *testing.T) {
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)
	if _, err := r.ClientFor(context.Background(), "org-1", "globex"); !errors.Is(err, ErrNoGitHubCredentials) {
		t.Fatalf("ClientFor err = %v, want ErrNoGitHubCredentials (no installation for target, no PAT fallback)", err)
	}
	if gh.mintCalls != 0 {
		t.Errorf("no installation matches target; should not mint; mintCalls=%d", gh.mintCalls)
	}
}

var errVaultDown = errors.New("vault unavailable")

// A secret-store read failure on the PAT (the last tier) must propagate as a
// real error, not be misreported as ErrNoGitHubCredentials — the dashboard
// maps the former to 500 and the latter to a "GitHub not configured" 503.
func TestResolver_PATReadError_Propagates(t *testing.T) {
	r := newTestResolver(
		&fakeSecrets{err: errVaultDown},
		&fakeApps{app: nil},
		&fakeOrgs{base: "https://github.com"}, // base resolves from settings, no secret read
		&fakeAgents{},
		nil,
	)
	_, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err == nil || errors.Is(err, ErrNoGitHubCredentials) {
		t.Fatalf("want a propagated backend error, got %v", err)
	}
	if !errors.Is(err, errVaultDown) {
		t.Errorf("error should wrap the secret-store failure, got %v", err)
	}
}

// When org_settings has no base and the github_url secret read fails, the
// resolver must NOT default to the deployment default (which would send a possibly-GHES
// PAT to the wrong host) — it must propagate the error.
func TestResolver_BaseReadError_Propagates(t *testing.T) {
	r := newTestResolver(
		&fakeSecrets{err: errVaultDown},
		&fakeApps{app: nil},
		&fakeOrgs{base: ""}, // settings empty → resolver reads the github_url secret, which errors
		&fakeAgents{},
		nil,
	)
	_, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err == nil || errors.Is(err, ErrNoGitHubCredentials) {
		t.Fatalf("want a propagated base-resolution error, got %v", err)
	}
	if !errors.Is(err, errVaultDown) {
		t.Errorf("error should wrap the secret-store failure, got %v", err)
	}
}

func TestResolver_NoCredentials(t *testing.T) {
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{}}, // no PAT
		&fakeApps{app: nil},                     // no App
		&fakeOrgs{base: "https://github.com"},
		&fakeAgents{},
		nil,
	)
	_, err := r.ClientFor(context.Background(), "org-1", "acme")
	if !errors.Is(err, ErrNoGitHubCredentials) {
		t.Errorf("got %v, want ErrNoGitHubCredentials", err)
	}
}

// When org_settings.github_base_url is empty, the resolver must still honor
// a GHES host stored only in the github_url secret (the pre-settings-mirror
// / local-mode case) rather than defaulting to public github.com.
func TestResolver_BaseURLFallbackToSecret(t *testing.T) {
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{
			integrations.KeyGitHubURL: gh.srv.URL, // host lives only in the secret
			integrations.KeyGitHubPAT: "ghp_test",
		}},
		&fakeApps{app: nil},
		&fakeOrgs{base: ""}, // settings has no base
		&fakeAgents{},
		nil,
	)
	client, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	// If the base wrongly defaulted to the deployment default, this request would never
	// reach the test server and lastProbe would stay empty.
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghp_test" {
		t.Errorf("base did not fall back to the github_url secret; lastProbe=%q", gh.lastProbe)
	}
}

// An installation match but a missing/invalid PEM. App XOR PAT — a mint failure
// on an active-App org is a hard error (surface the bad PEM), not a silent PAT
// fallback. The error is the raw mint failure (not ErrNoGitHubCredentials): the
// org IS configured, the credential just couldn't be minted.
func TestResolver_AppMintFails_Errors(t *testing.T) {
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}}, // PEM absent
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)
	_, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err == nil {
		t.Fatal("ClientFor err = nil, want a mint error (active App with a bad PEM must not fall back to PAT)")
	}
	if errors.Is(err, ErrNoGitHubCredentials) {
		t.Errorf("err = %v, want the raw mint error, not ErrNoGitHubCredentials (the org IS configured)", err)
	}
}

// TestResolver_OrgIdentityFor pins the org commit-identity resolution (TFAC-452,
// TFAC-474): the App tier yields name "<slug>[bot]" live from the registration
// (App-preferred, even when a stored PAT login also exists) with the numeric-id
// noreply email when org_github_apps.bot_user_id is set and the plain form when
// it's 0 (unknown); an inactive/absent App falls through to the stored
// agents.github_org_login + github_org_email — but ONLY while
// the org still has a PAT — and an all-miss yields ok=false (the caller stamps
// no identity).
func TestResolver_OrgIdentityFor(t *testing.T) {
	// withPAT is the fakeSecrets an org with a live PAT presents; the PAT tier is
	// gated on this presence so a cached login can't outlive the credential.
	withPAT := func() *fakeSecrets {
		return &fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}}
	}

	assertIdentity := func(t *testing.T, gotName, gotEmail string, gotOK bool, wantName, wantEmail string) {
		t.Helper()
		if !gotOK || gotName != wantName || gotEmail != wantEmail {
			t.Fatalf("OrgIdentityFor = (%q, %q, %v), want (%q, %q, true)", gotName, gotEmail, gotOK, wantName, wantEmail)
		}
	}

	t.Run("App tier resolves <slug>[bot] live, plain email when bot id unknown", func(t *testing.T) {
		// No PAT secret: the App tier must resolve without one (it short-circuits
		// before the PAT-presence gate). BotUserID unset (0) → plain noreply form.
		r := newTestResolver(
			&fakeSecrets{},
			&fakeApps{app: &domain.OrgGitHubApp{OrgID: "org-1", Active: true, Slug: "acme-bot"}},
			&fakeOrgs{},
			&fakeAgents{},
			nil,
		)
		name, email, ok := r.OrgIdentityFor(context.Background(), "org-1")
		assertIdentity(t, name, email, ok, "acme-bot[bot]", "acme-bot[bot]@users.noreply.github.com")
	})

	t.Run("App tier with bot_user_id -> numeric-id noreply email", func(t *testing.T) {
		// TFAC-474: a stored bot user id yields the numeric-id form, the only form
		// that links a bot's commits to its account on github.com.
		r := newTestResolver(
			&fakeSecrets{},
			&fakeApps{app: &domain.OrgGitHubApp{OrgID: "org-1", Active: true, Slug: "acme-bot", BotUserID: 41898282}},
			&fakeOrgs{},
			&fakeAgents{},
			nil,
		)
		name, email, ok := r.OrgIdentityFor(context.Background(), "org-1")
		assertIdentity(t, name, email, ok, "acme-bot[bot]", "41898282+acme-bot[bot]@users.noreply.github.com")
	})

	t.Run("App preferred over stored PAT login", func(t *testing.T) {
		r := newTestResolver(
			withPAT(),
			&fakeApps{app: &domain.OrgGitHubApp{Active: true, Slug: "acme-bot", BotUserID: 999}},
			&fakeOrgs{},
			&fakeAgents{agent: &domain.Agent{GitHubOrgLogin: "pat-login", GitHubOrgEmail: "pat@example.com"}},
			nil,
		)
		name, email, ok := r.OrgIdentityFor(context.Background(), "org-1")
		assertIdentity(t, name, email, ok, "acme-bot[bot]", "999+acme-bot[bot]@users.noreply.github.com")
	})

	t.Run("inactive App falls through to stored PAT identity", func(t *testing.T) {
		r := newTestResolver(
			withPAT(),
			&fakeApps{app: &domain.OrgGitHubApp{Active: false, Slug: "acme-bot", BotUserID: 12345}},
			&fakeOrgs{},
			&fakeAgents{agent: &domain.Agent{GitHubOrgLogin: "octocat", GitHubOrgEmail: "octocat@example.com"}},
			nil,
		)
		name, email, ok := r.OrgIdentityFor(context.Background(), "org-1")
		// The PAT tier ignores the inactive App's bot identity.
		assertIdentity(t, name, email, ok, "octocat", "octocat@example.com")
	})

	t.Run("PAT tier (no App) resolves stored identity", func(t *testing.T) {
		r := newTestResolver(
			withPAT(),
			&fakeApps{app: nil},
			&fakeOrgs{},
			&fakeAgents{agent: &domain.Agent{GitHubOrgLogin: "octocat", GitHubOrgEmail: "octocat@example.com"}},
			nil,
		)
		name, email, ok := r.OrgIdentityFor(context.Background(), "org-1")
		assertIdentity(t, name, email, ok, "octocat", "octocat@example.com")
	})

	t.Run("stored login but PAT cleared yields ok=false", func(t *testing.T) {
		// The regression this gate closes: a login persisted while the org had a
		// PAT must NOT resurface once the PAT is gone (no App, empty secrets), or
		// OrgIdentityFor would stamp a stale identity for an org with no GitHub
		// credential at all.
		r := newTestResolver(
			&fakeSecrets{}, // PAT cleared — github_org_login is now stale
			&fakeApps{app: nil},
			&fakeOrgs{},
			&fakeAgents{agent: &domain.Agent{GitHubOrgLogin: "octocat"}},
			nil,
		)
		name, email, ok := r.OrgIdentityFor(context.Background(), "org-1")
		if ok || name != "" || email != "" {
			t.Fatalf("OrgIdentityFor = (%q, %q, %v), want (\"\", \"\", false) for a cleared PAT with a stale login", name, email, ok)
		}
	})

	t.Run("neither yields ok=false", func(t *testing.T) {
		r := newTestResolver(
			&fakeSecrets{},
			&fakeApps{app: nil},
			&fakeOrgs{},
			&fakeAgents{agent: &domain.Agent{}}, // no stored login
			nil,
		)
		name, email, ok := r.OrgIdentityFor(context.Background(), "org-1")
		if ok || name != "" || email != "" {
			t.Fatalf("OrgIdentityFor = (%q, %q, %v), want (\"\", \"\", false)", name, email, ok)
		}
	})
}

// TestResolver_TokenForRepoScoped_AppXorPAT pins the scoped path's App-XOR-PAT
// contract: an active App is the credential (scoped mint or error, never the
// PAT, even when one is present); only a no-App org uses the PAT (unscoped).
func TestResolver_TokenForRepoScoped_AppXorPAT(t *testing.T) {
	t.Run("active App mints scoped, ignores a present PAT", func(t *testing.T) {
		gh := newGHTestServer(t)
		r := newTestResolver(
			&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
			&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
			&fakeOrgs{base: gh.srv.URL},
			&fakeAgents{},
			nil,
		)
		tok, err := r.(ScopedResolver).TokenForRepoScoped(context.Background(), "org-1", "acme", "widget", map[string]string{"contents": "write"})
		if err != nil {
			t.Fatalf("TokenForRepoScoped: %v", err)
		}
		if tok.Value != "ghs_minted" {
			t.Errorf("token = %q, want the minted App token, not the PAT (App XOR PAT)", tok.Value)
		}
	})

	t.Run("active App, no installation for owner → error, not PAT", func(t *testing.T) {
		gh := newGHTestServer(t)
		r := newTestResolver(
			&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
			&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
			&fakeOrgs{base: gh.srv.URL},
			&fakeAgents{},
			nil,
		)
		if _, err := r.(ScopedResolver).TokenForRepoScoped(context.Background(), "org-1", "globex", "widget", nil); !errors.Is(err, ErrNoGitHubCredentials) {
			t.Errorf("err = %v, want ErrNoGitHubCredentials (no installation; no PAT fallback)", err)
		}
	})

	t.Run("no active App → PAT, unscoped", func(t *testing.T) {
		r := newTestResolver(
			&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}},
			&fakeApps{app: nil},
			&fakeOrgs{base: "https://github.com"},
			&fakeAgents{},
			nil,
		)
		tok, err := r.(ScopedResolver).TokenForRepoScoped(context.Background(), "org-1", "acme", "widget", map[string]string{"contents": "write"})
		if err != nil {
			t.Fatalf("TokenForRepoScoped: %v", err)
		}
		if tok.Value != "ghp_test" {
			t.Errorf("token = %q, want the PAT (no App → PAT, unscoped)", tok.Value)
		}
		if !tok.ExpiresAt.IsZero() {
			t.Errorf("PAT token should carry a zero expiry, got %v", tok.ExpiresAt)
		}
	})
}

// TestResolver_HasAnyCredential pins the run-start probe: true for an active App
// WITH an installation, or a present PAT; false for an App with no installation
// and no PAT, or nothing.
func TestResolver_HasAnyCredential(t *testing.T) {
	cases := []struct {
		name  string
		app   *domain.OrgGitHubApp
		insts []domain.OrgGitHubAppInstallation
		pat   string
		want  bool
	}{
		{"active App with installation", activeApp(), []domain.OrgGitHubAppInstallation{installOn("acme")}, "", true},
		{"PAT only", nil, nil, "ghp_test", true},
		{"active App, no installation, no PAT", activeApp(), nil, "", false},
		{"neither", nil, nil, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vals := map[string]string{}
			if c.pat != "" {
				vals[integrations.KeyGitHubPAT] = c.pat
			}
			r := newTestResolver(&fakeSecrets{vals: vals}, &fakeApps{app: c.app, insts: c.insts}, &fakeOrgs{base: "https://github.com"}, &fakeAgents{}, nil)
			got, err := r.(ScopedResolver).HasAnyCredential(context.Background(), "org-1")
			if err != nil {
				t.Fatalf("HasAnyCredential: %v", err)
			}
			if got != c.want {
				t.Errorf("HasAnyCredential = %v, want %v", got, c.want)
			}
		})
	}
}

// TestResolver_ScopedMint_DisabledSecretStore_FailsLoudlyWithoutLoadingKey is
// the executor-side guarantee behind control-plane App-token minting: an
// executor wires the disabled SecretStore (it never holds the
// secret-decryption key — App-token minting is a control-plane
// responsibility, and executors receive only pre-minted, hour-lived tokens in
// their sealed run bundle). So any stray attempt to mint an installation token
// on an executor — the one code path that would parse the App private key —
// must fail loudly at the store, before an App JWT is signed. Proven two ways:
// the error wraps the executor sentinel, and the mint endpoint is never hit
// (no signed JWT means the private key was never parsed or loaded).
func TestResolver_ScopedMint_DisabledSecretStore_FailsLoudlyWithoutLoadingKey(t *testing.T) {
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{err: db.ErrSecretStoreUnavailable}, // the executor's disabled secret store
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	_, err := r.(ScopedResolver).TokenForRepoScoped(context.Background(), "org-1", "acme", "widget", map[string]string{"contents": "write"})
	if !errors.Is(err, db.ErrSecretStoreUnavailable) {
		t.Fatalf("err = %v, want it to wrap db.ErrSecretStoreUnavailable (a stray executor mint must fail loudly at the sentinel)", err)
	}
	if n := atomic.LoadInt32(&gh.mintCalls); n != 0 {
		t.Fatalf("mint endpoint hit %d times; the disabled secret store must fail before any App JWT is signed (no private-key material loaded)", n)
	}
}
