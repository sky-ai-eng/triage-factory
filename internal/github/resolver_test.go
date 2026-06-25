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
	"strings"
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
	app   *domain.OrgGitHubApp
	insts []domain.OrgGitHubAppInstallation
}

func (f *fakeApps) GetForOrgSystem(_ context.Context, _ string) (*domain.OrgGitHubApp, error) {
	return f.app, nil
}

func (f *fakeApps) ListInstallationsForOrgSystem(_ context.Context, _ string) ([]domain.OrgGitHubAppInstallation, error) {
	return f.insts, nil
}

type fakeOrgs struct {
	db.OrgsStore
	base string
}

func (f *fakeOrgs) GetSettingsSystem(_ context.Context, _ string) (domain.OrgSettings, error) {
	return domain.OrgSettings{GitHubBaseURL: f.base}, nil
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
}

func newGHTestServer(t *testing.T) *ghTestServer {
	t.Helper()
	g := &ghTestServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/app/installations/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&g.mintCalls, 1)
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
	r := NewResolver(
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
	if _, err := client.Get("/probe"); err != nil {
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
	r := NewResolver(
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
	if _, err := client.Get("/probe"); err != nil {
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
	r := NewResolver(
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
	if _, err := client.Get("/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghs_minted" {
		t.Errorf("client carried %q, want the minted App token", gh.lastProbe)
	}
}

// ClientForRepo: the App is installed on the account but the repo is NOT in the
// grant (a "Selected repositories" install). Resolving on the owner alone would
// hand back an App token that 403s on this repo; ClientForRepo detects the gap
// up front and falls through to the PAT.
func TestResolver_ClientForRepo_AppDoesNotCoverRepo_FallsBackToPAT(t *testing.T) {
	gh := newGHTestServer(t)
	gh.installRepos = []string{"acme/widget"} // "acme/other" is not granted
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	client, err := r.ClientForRepo(context.Background(), "org-1", "acme", "other")
	if err != nil {
		t.Fatalf("ClientForRepo: %v", err)
	}
	if _, err := client.Get("/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghp_test" {
		t.Errorf("client carried %q, want the PAT fallback for an uncovered repo", gh.lastProbe)
	}
}

// ClientForRepo: the App is installed but the coverage probe is indeterminate
// (GitHub 5xx). The contract is fail-open — return the minted App client rather
// than discarding it for the PAT — and the indeterminate result must NOT be
// cached, so a transient outage can't pin a wrong coverage answer.
func TestResolver_ClientForRepo_CoverageProbeIndeterminate_FailsOpen(t *testing.T) {
	gh := newGHTestServer(t)
	gh.repoProbe5xx = true // GET /repos/{owner}/{repo} → 500
	r := NewResolver(
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
	if _, err := client.Get("/probe"); err != nil {
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
	r := NewResolver(
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
// selective grant has to be picked up on the next call, not pinned to the PAT
// (or to ErrNoGitHubCredentials for an App-only org) for a whole TTL. So each
// not-covered resolution re-probes.
func TestResolver_ClientForRepo_DoesNotCacheNonCoverage(t *testing.T) {
	gh := newGHTestServer(t)
	gh.installRepos = []string{"acme/widget"} // "acme/other" not granted
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	for i := 0; i < 2; i++ {
		if _, err := r.ClientForRepo(context.Background(), "org-1", "acme", "other"); err != nil {
			t.Fatalf("ClientForRepo #%d: %v", i, err)
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
	r := NewResolver(
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
	r := NewResolver(
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
	if _, err := client.Get("/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghp_test" {
		t.Errorf("client carried %q, want the PAT", gh.lastProbe)
	}
	if gh.mintCalls != 0 {
		t.Errorf("mint was called %d times for a PAT-only org", gh.mintCalls)
	}
}

// ClientForRepoWithIdentity reports the tier it resolved as an Identity, so the
// agenthost pending-review collision check can branch on App-vs-PAT without
// re-deriving it from the opaque bearer token. The identity must track the same
// tier decision ClientForRepo makes: App covers the repo → IdentityApp; App
// doesn't cover (or no App) → the PAT fallback → IdentityPAT; nothing resolves
// → IdentityUnknown alongside the error.
func TestResolver_ClientForRepoWithIdentity_ReportsTier(t *testing.T) {
	t.Run("app covers repo → IdentityApp", func(t *testing.T) {
		gh := newGHTestServer(t)
		gh.installRepos = []string{"acme/widget"}
		r := NewResolver(
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
		if _, err := client.Get("/probe"); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if gh.lastProbe != "Bearer ghs_minted" {
			t.Errorf("client carried %q, want the minted App token alongside IdentityApp", gh.lastProbe)
		}
	})

	t.Run("app does not cover repo → IdentityPAT", func(t *testing.T) {
		gh := newGHTestServer(t)
		gh.installRepos = []string{"acme/widget"} // "acme/other" not granted
		r := NewResolver(
			&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
			&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
			&fakeOrgs{base: gh.srv.URL},
			&fakeAgents{},
			nil,
		)
		_, identity, err := r.(RepoIdentityResolver).ClientForRepoWithIdentity(context.Background(), "org-1", "acme", "other")
		if err != nil {
			t.Fatalf("ClientForRepoWithIdentity: %v", err)
		}
		if identity != IdentityPAT {
			t.Errorf("identity = %v, want IdentityPAT (App didn't cover → PAT fallback)", identity)
		}
	})

	t.Run("no app → IdentityPAT", func(t *testing.T) {
		gh := newGHTestServer(t)
		r := NewResolver(
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
		r := NewResolver(
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
	r := NewResolver(
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
	r := NewResolver(
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
	r := NewResolver(
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
	r := NewResolver(
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
	r := NewResolver(
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

func TestResolver_EmptyTargetMultiInstall_FallsToPAT(t *testing.T) {
	gh := newGHTestServer(t)
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme"), installOn("globex")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)
	client, err := r.ClientFor(context.Background(), "org-1", "")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get("/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghp_test" {
		t.Errorf("ambiguous empty target should fall to PAT; client carried %q", gh.lastProbe)
	}
	if gh.mintCalls != 0 {
		t.Errorf("ambiguous empty target should not mint; mintCalls=%d", gh.mintCalls)
	}
}

func TestResolver_TargetNoMatch_FallsToPAT(t *testing.T) {
	gh := newGHTestServer(t)
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)
	if _, err := r.ClientFor(context.Background(), "org-1", "globex"); err != nil {
		t.Fatalf("ClientFor: %v", err)
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
	r := NewResolver(
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
// resolver must NOT default to github.com (which would send a possibly-GHES
// PAT to the wrong host) — it must propagate the error.
func TestResolver_BaseReadError_Propagates(t *testing.T) {
	r := NewResolver(
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
	r := NewResolver(
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
	r := NewResolver(
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
	// If the base wrongly defaulted to github.com, this request would never
	// reach the test server and lastProbe would stay empty.
	if _, err := client.Get("/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghp_test" {
		t.Errorf("base did not fall back to the github_url secret; lastProbe=%q", gh.lastProbe)
	}
}

// An installation match but a missing/invalid PEM must not hard-fail — the
// resolver logs and falls through to the PAT.
func TestResolver_AppMintFails_FallsToPAT(t *testing.T) {
	gh := newGHTestServer(t)
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}}, // PEM absent
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)
	client, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get("/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghp_test" {
		t.Errorf("a failed mint should fall back to PAT; client carried %q", gh.lastProbe)
	}
}
