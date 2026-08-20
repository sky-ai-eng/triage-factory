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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// newAppRepoStub stands in for GitHub's REST API for the App-mode repo picker: it
// mints a per-installation token (encoding the installation id into the token
// value) and serves GET /installation/repositories scoped to whichever token
// the client presents. That lets one stub model multiple installations, each
// granting a distinct repo set, so the handler's union+dedup is exercised.
func newAppRepoStub(t *testing.T, reposByInstall map[string][]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// GET /app/installations — what the grant refresh reconciles existence
	// against before it reads any grant. Registered explicitly rather than left
	// to the subtree pattern below, which would redirect the GET onto the
	// token-mint handler and answer 405. The stub reports exactly the
	// installations its repo map has entries for, so a refresh neither
	// soft-removes a seeded installation nor discovers one the fixture never
	// staged.
	// Rendered once, up front, rather than per request: t.Fatalf on a bad fixture
	// only fails the test from the test's own goroutine, and this handler runs on
	// the server's.
	installations := stubInstallations(t, reposByInstall)
	mux.HandleFunc("GET /api/v3/app/installations", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(installations)
	})
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

// stubInstallations renders the installation objects GET /app/installations
// answers with, one per key of a repo map. The account login is derived from the
// first slug's owner so the fixture does not have to state it twice — a grant
// map and an account list that disagreed would send the refresh looking for a
// token it cannot mint.
//
// It takes t and FAILS on a key it cannot render, rather than skipping it. A
// skip would answer with fewer installations than the fixture staged — possibly
// none — and the refresh reconciles existence against this answer, so it would
// soft-remove the very installations the test seeded and then pass with an empty
// mirror. A typo'd fixture must break the test that holds it, not quietly change
// what that test is about.
func stubInstallations(t *testing.T, reposByInstall map[string][]string) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(reposByInstall))
	for id, slugs := range reposByInstall {
		login := "acme"
		if len(slugs) > 0 {
			if owner, _, ok := strings.Cut(slugs[0], "/"); ok {
				login = owner
			}
		}
		// GitHub reports the installation id as a number; TF stores it as text.
		// The fixture keys the map by the stored form, so it converts back here
		// rather than making every caller write the id twice.
		numericID, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			t.Fatalf("fixture installation id %q is not numeric; GitHub reports installation ids as numbers", id)
		}
		out = append(out, map[string]any{
			"id":         numericID,
			"account":    map[string]any{"login": login, "type": "Organization"},
			"created_at": "2026-01-02T03:04:05Z",
		})
	}
	return out
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
	if _, err := s.orgs.UpdateSettings(ctx, org, domain.OrgSettings{GitHubBaseURL: stub.URL}); err != nil {
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

// pickerPage is the picker's response envelope: the shared list keys plus the
// readiness discriminator. Decoded as its own type rather than through
// decodeList because `status` is the field most of these assertions are about.
type pickerPage struct {
	Items         []githubRepoJSON `json:"items"`
	NextPageToken string           `json:"next_page_token"`
	TotalCount    int              `json:"total_count"`
	Status        string           `json:"status"`
}

// fetchPickerPage reads one page of the picker list with the given body.
func fetchPickerPage(t *testing.T, s *Server, body map[string]any) pickerPage {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/github/repos/list", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("picker list = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var page pickerPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode picker page: %v; body=%s", err, rec.Body.String())
	}
	return page
}

// fetchPickerRepos walks every page of the picker list and returns the
// concatenated slugs, so the assertions below read the whole set whether it
// arrived in one page or several. It also pins the two properties every page
// owes on both credential classes: a real total_count (never the null a proxy
// list answered) and the ready discriminator.
func fetchPickerRepos(t *testing.T, s *Server) []string {
	t.Helper()
	var all []string
	body := map[string]any{}
	for range 10 { // a fixture never needs 10 pages; the bound stops a token loop
		page := fetchPickerPage(t, s, body)
		if page.Status != githubRepoStatusReady {
			t.Fatalf("picker status = %q, want %q — the mirror was refreshed before this read", page.Status, githubRepoStatusReady)
		}
		for _, item := range page.Items {
			all = append(all, item.FullName)
		}
		if page.NextPageToken == "" {
			if page.TotalCount != len(all) {
				t.Errorf("total_count = %d after walking %d rows; want them equal", page.TotalCount, len(all))
			}
			return all
		}
		body = map[string]any{"page_token": page.NextPageToken}
	}
	t.Fatalf("picker list did not terminate after 10 pages")
	return nil
}

// App org with a single installation: the picker lists that installation's
// repositories, sourced from the mirror the grant refresh wrote.
func TestHandleGitHubRepos_AppSingleInstallation(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	stub := newAppRepoStub(t, map[string][]string{
		"111": {"acme/api", "acme/web"},
	})
	seedApp(t, srv, stub, []domain.OrgGitHubAppInstallation{
		{InstallationID: "111", AccountType: "Organization", AccountLogin: "acme"},
	})
	reach := wireReachRefresh(t, srv)
	reach.refresh(t, runmode.LocalDefaultOrgID, true)

	got := fetchPickerRepos(t, srv)
	want := []string{"acme/api", "acme/web"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("repos = %v, want %v", got, want)
	}
}

// App org where the installation's repo-list call fails: the refresh writes
// nothing, and the picker must say so. Answering an empty READY list would read
// as "your App reaches no repositories", which is a claim only a successful
// enumeration can make — the whole reason the readiness discriminator exists.
func TestHandleGitHubRepos_AppListFailureLeavesTheMirrorUnclaimed(t *testing.T) {
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
	wireReachRefresh(t, srv)

	page := fetchPickerPage(t, srv, map[string]any{})
	if page.Status != githubRepoStatusDiscovering {
		t.Errorf("picker status = %q after a failed enumeration; want %q — an empty ready list would claim the App reaches nothing",
			page.Status, githubRepoStatusDiscovering)
	}
	if len(page.Items) != 0 || page.TotalCount != 0 {
		t.Errorf("picker returned %d items / total %d with nothing mirrored; want none", len(page.Items), page.TotalCount)
	}
}

// App org whose App is registered + active but installed on zero accounts, with
// no PAT to fall back to: the picker returns a distinct 409 the frontend keys
// install guidance off — not the generic "GitHub not configured" that reads as
// "add a PAT". This is the first-run dead-end TFAC-324 makes diagnosable, and it
// is a preflight: it fires before the mirror is consulted, because "we have not
// looked yet" promises that looking will eventually produce something.
func TestHandleGitHubRepos_AppActiveZeroInstallations(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	// The stub is only here to wire the org's base URL + PEM; the zero-install
	// case never makes a GitHub call.
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

// A failed installations read (transient DB/store error) has three wrong answers
// available to it and this pins which one the handler gives.
//
// It must NOT be reported as "not installed" — that claim is only valid on a
// positive read of zero installations, and it sends the user to fix an install
// that is fine. It must NOT be reported as DISCOVERING either, which is the
// subtler mistake: that state promises the list will fill in, and the read that
// would have established the promise is exactly the one that failed, so an org
// with nothing mirrored would sit on a spinner that may never resolve. With no
// mirror to fall back on, the honest answer is the error.
func TestHandleGitHubRepos_AppListInstallationsErrorIsNotDiscovering(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	seedBYOAppCredentialClass(t, srv, runmode.LocalDefaultOrgID)
	srv.githubApps = &fakeGitHubAppsStore{
		app:     &domain.OrgGitHubApp{OrgID: runmode.LocalDefaultOrgID, AppID: "123", Slug: "acme-bot", Active: true},
		listErr: errors.New("transient store failure"),
	}

	rec := doJSON(t, srv, http.MethodPost, "/api/github/repos/list", map[string]any{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("picker list = %d on a failed installations read with an empty mirror, want 500; body=%s",
			rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "not installed") {
		t.Errorf("body=%s, want no 'not installed' claim — only a successful read of zero installations supports it", body)
	}
	if body := rec.Body.String(); strings.Contains(body, githubRepoStatusDiscovering) {
		t.Errorf("body=%s, want no discovering state — nothing established that discovery will resolve", body)
	}
}

// The same failed read, on an org whose mirror already answers: the read decides
// only whether to offer install guidance, and the picker's data does not come
// from it. Blanking a working picker over a hint it could not compute would be
// the failure this degrade exists to avoid.
func TestHandleGitHubRepos_AppListInstallationsErrorStillServesAMirror(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	stub := newAppRepoStub(t, map[string][]string{
		"111": {"acme/api"},
	})
	seedApp(t, srv, stub, []domain.OrgGitHubAppInstallation{
		{InstallationID: "111", AccountType: "Organization", AccountLogin: "acme"},
	})
	reach := wireReachRefresh(t, srv)
	reach.refresh(t, runmode.LocalDefaultOrgID, true)

	// Only now does the installations read start failing — the mirror is already
	// written, which is the state this test is about.
	liveApps := srv.githubApps
	srv.githubApps = &fakeGitHubAppsStore{
		app:     &domain.OrgGitHubApp{OrgID: runmode.LocalDefaultOrgID, AppID: "123", Slug: "acme-bot", Active: true},
		listErr: errors.New("transient store failure"),
	}
	t.Cleanup(func() { srv.githubApps = liveApps })

	page := fetchPickerPage(t, srv, map[string]any{})
	if page.Status != githubRepoStatusReady {
		t.Fatalf("picker status = %q with a written mirror, want %q", page.Status, githubRepoStatusReady)
	}
	if page.TotalCount != 1 || len(page.Items) != 1 {
		t.Errorf("picker = %d items / total %d; want the mirrored one", len(page.Items), page.TotalCount)
	}
}

// App org with multiple installations: the mirror carries every installation's
// grant, and the picker's order is the folded slug across all of them — one
// order for the whole set, not per-account runs, because a list with a search
// box over it is read alphabetically and offset paging needs a total order.
func TestHandleGitHubRepos_AppMultiInstallation(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	stub := newAppRepoStub(t, map[string][]string{
		"111": {"acme/api", "shared/lib"},
		"222": {"beta/web"},
	})
	seedApp(t, srv, stub, []domain.OrgGitHubAppInstallation{
		{InstallationID: "111", AccountType: "Organization", AccountLogin: "acme"},
		{InstallationID: "222", AccountType: "Organization", AccountLogin: "beta"},
	})
	reach := wireReachRefresh(t, srv)
	reach.refresh(t, runmode.LocalDefaultOrgID, true)

	got := fetchPickerRepos(t, srv)
	want := []string{"acme/api", "beta/web", "shared/lib"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("repos = %v, want %v (every installation's repos, in one folded-slug order)", got, want)
	}
}
