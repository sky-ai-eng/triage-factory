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
	srv       *httptest.Server
	mintCalls int32
	lastProbe string // Authorization header seen on the /probe call
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
