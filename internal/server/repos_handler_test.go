package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// appRepoStub stands in for GitHub's REST API for the App-mode repo picker: it
// mints a per-installation token (encoding the installation id into the token
// value) and serves GET /installation/repositories scoped to whichever token
// the client presents. That lets one stub model multiple installations, each
// granting a distinct repo set, so the handler's union+dedup is exercised.
func newAppRepoStub(t *testing.T, reposByInstall map[string][]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		// Path: /api/v3/app/installations/{id}/access_tokens — pull {id}.
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		var id string
		for i, p := range parts {
			if p == "installations" && i+1 < len(parts) {
				id = parts[i+1]
				break
			}
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_" + id,
			"expires_at": time.Now().Add(time.Hour).UTC(),
		})
	})
	mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ghs_")
		names := reposByInstall[id]
		repos := make([]map[string]any, 0, len(names))
		for _, n := range names {
			repos = append(repos, map[string]any{"full_name": n})
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count":  len(repos),
			"repositories": repos,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testRSAPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

// seedApp registers an org App (active by default) backed by pemRef and mirrors
// the given installations. The PEM is stored so the resolver can mint tokens.
func seedApp(t *testing.T, s *Server, stub *httptest.Server, installs []domain.OrgGitHubAppInstallation) {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	if err := s.orgs.UpdateSettings(ctx, org, domain.OrgSettings{GitHubBaseURL: stub.URL}); err != nil {
		t.Fatalf("set org github base: %v", err)
	}
	if err := s.secrets.Put(ctx, org, "pem", testRSAPEM(t), ""); err != nil {
		t.Fatalf("store pem: %v", err)
	}
	if err := s.githubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
		OrgID: org, AppID: "123", Slug: "test-bot", PEMRef: "pem",
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}
	for _, inst := range installs {
		inst.OrgID = org
		if err := s.githubApps.UpsertInstallation(ctx, inst); err != nil {
			t.Fatalf("upsert installation %s: %v", inst.AccountLogin, err)
		}
	}
}

func fetchPickerRepos(t *testing.T, s *Server) []ghclient.UserRepo {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, "/api/github/repos", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/github/repos = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var repos []ghclient.UserRepo
	if err := json.Unmarshal(rec.Body.Bytes(), &repos); err != nil {
		t.Fatalf("decode repos: %v", err)
	}
	return repos
}

func fullNames(repos []ghclient.UserRepo) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.FullName
	}
	return out
}

// App org with a single installation: the picker lists that installation's
// repositories rather than calling /user/repos.
func TestHandleGitHubRepos_AppSingleInstallation(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	stub := newAppRepoStub(t, map[string][]string{
		"111": {"acme/api", "acme/web"},
	})
	seedApp(t, srv, stub, []domain.OrgGitHubAppInstallation{
		{InstallationID: "111", AccountType: "Organization", AccountLogin: "acme"},
	})

	got := fullNames(fetchPickerRepos(t, srv))
	want := []string{"acme/api", "acme/web"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("repos = %v, want %v", got, want)
	}
}

// App org with multiple installations: the picker returns the union of every
// installation's repos, deduped by full name and sorted stably.
func TestHandleGitHubRepos_AppMultiInstallationDedup(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	stub := newAppRepoStub(t, map[string][]string{
		"111": {"acme/api", "shared/lib"},
		"222": {"beta/web", "shared/lib"}, // shared/lib overlaps → deduped
	})
	seedApp(t, srv, stub, []domain.OrgGitHubAppInstallation{
		{InstallationID: "111", AccountType: "Organization", AccountLogin: "acme"},
		{InstallationID: "222", AccountType: "Organization", AccountLogin: "beta"},
	})

	got := fullNames(fetchPickerRepos(t, srv))
	want := []string{"acme/api", "beta/web", "shared/lib"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("repos = %v, want %v (union deduped + sorted)", got, want)
	}
}
