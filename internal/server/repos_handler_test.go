package server

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
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
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
		// The real endpoint is POST /app/installations/{id}/access_tokens —
		// reject other methods so a regression that issues the wrong verb fails
		// the test instead of silently minting.
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
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
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
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
		OrgID: org, AppID: "123", Slug: "test-bot", PEMRef: "pem", Active: true,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}
	seedBYOAppCredentialClass(t, s, org)
	for _, inst := range installs {
		inst.OrgID = org
		if err := s.githubApps.UpsertInstallation(ctx, inst); err != nil {
			t.Fatalf("upsert installation %s: %v", inst.AccountLogin, err)
		}
	}
}

// fetchPickerRepos walks every page of the picker's proxy list and returns the
// concatenation, so the assertions below read the whole enumeration whether it
// arrived in one page or several.
func fetchPickerRepos(t *testing.T, s *Server) []ghclient.UserRepo {
	t.Helper()
	var all []ghclient.UserRepo
	body := map[string]any{}
	for range 10 { // a fixture never needs 10 pages; the bound stops a token loop
		rec := doJSON(t, s, http.MethodPost, "/api/github/repos/list", body)
		page := decodeList[ghclient.UserRepo](t, rec)
		// A proxy list cannot count its upstream, so total_count is null.
		if page.TotalCount != nil {
			t.Errorf("total_count = %d, want null on a proxy list", *page.TotalCount)
		}
		all = append(all, page.Items...)
		if page.NextPageToken == "" {
			return all
		}
		body = map[string]any{"page_token": page.NextPageToken}
	}
	t.Fatalf("picker list did not terminate after 10 pages")
	return nil
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

// App org where every installation's repo-list call fails: the handler must
// surface a fetch error (502), not return an empty list that reads as "no
// repositories" and warms the reachable-repo cache with an empty set.
func TestHandleGitHubRepos_AppListFailureSurfacesError(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_x",
			"expires_at": time.Now().Add(time.Hour).UTC(),
		})
	})
	mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, []domain.OrgGitHubAppInstallation{
		{InstallationID: "111", AccountType: "Organization", AccountLogin: "acme"},
	})

	rec := doJSON(t, srv, http.MethodPost, "/api/github/repos/list", map[string]any{})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("picker list = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
}

// App org whose App is registered + active but installed on zero accounts,
// with no PAT to fall back to: the picker returns a distinct 400 the frontend
// keys install guidance off — not the generic "GitHub not configured" that
// reads as "add a PAT". This is the first-run dead-end TFAC-324 makes
// diagnosable.
func TestHandleGitHubRepos_AppActiveZeroInstallations(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	// The stub is only here to wire the org's base URL + PEM; the zero-install
	// case falls into the PAT path and never makes a GitHub call.
	stub := newAppRepoStub(t, nil)
	seedApp(t, srv, stub, nil) // active App, zero installations

	rec := doJSON(t, srv, http.MethodPost, "/api/github/repos/list", map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("picker list = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonNotConfigured, "")
	if want := "the GitHub App is not installed on any account"; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body=%s, want it to say %q", rec.Body.String(), want)
	}
}

// A failed installations read (transient DB/store error) leaves insts nil,
// which must NOT be reported as "not installed" — that claim is only valid on a
// positive read of zero installations. The handler degrades to the PAT path and
// returns the generic "not configured" instead. Guards the regression where the
// swallowed read error masqueraded as an empty install set.
func TestHandleGitHubRepos_AppListInstallationsErrorNotReportedAsUninstalled(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	srv.githubApps = &fakeGitHubAppsStore{
		app:     &domain.OrgGitHubApp{OrgID: runmode.LocalDefaultOrgID, AppID: "123", Slug: "acme-bot", Active: true},
		listErr: errors.New("transient store failure"),
	}

	rec := doJSON(t, srv, http.MethodPost, "/api/github/repos/list", map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("picker list = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonNotConfigured, "")
	if body := rec.Body.String(); strings.Contains(body, "not installed") {
		t.Errorf("body=%s, want the generic not-connected message (a failed read must not report 'not installed')", body)
	}
}

// App org with multiple installations: the picker walks the installations in
// order, so the concatenation of its pages is every installation's repos.
//
// The old union deduped by full name and sorted globally; a proxy list resumes
// from an upstream position and cannot do either without re-fetching every
// account per page. A repository belongs to exactly one account, so the
// accounts partition the result and the dedup was defensive rather than
// load-bearing.
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
	want := []string{"acme/api", "shared/lib", "beta/web", "shared/lib"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("repos = %v, want %v (each installation's repos, in installation order)", got, want)
	}
}
