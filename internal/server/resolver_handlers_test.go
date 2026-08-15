package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TFAC-327: the review diff/submit, pending-PR submit, branches, and
// project-bundle probe handlers were migrated off the PAT-only process-global
// GitHub client onto the credential resolver, so App-only orgs (no PAT) reach
// GitHub through the org's App installation token instead of 503/400-ing on a
// nil global client. These tests drive each call site against an App-only org
// (resolver tier 1) and against an org with no credentials at all (resolver
// returns ErrNoGitHubCredentials).

// newAppAPIMux returns a ServeMux pre-wired with the two endpoints every
// resolver tier-1 path needs: the App installation-token mint (the token value
// encodes the installation id, so a test can assert which installation token
// reached GitHub) and GET /repos/{owner}/{repo}, which serves double duty as
// the ClientForRepo coverage probe (needs a 200) and GetRepoMeta (needs a
// clone_url). Tests register their specific endpoint on the returned mux, wrap
// it in httptest.NewServer, then point the org's App base URL at it via seedApp.
func newAppAPIMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_" + r.PathValue("id"),
			"expires_at": time.Now().Add(time.Hour).UTC(),
		})
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"clone_url":      "https://example.test/" + r.PathValue("owner") + "/" + r.PathValue("repo") + ".git",
			"default_branch": "main",
		})
	})
	return mux
}

// acmeInstall is the single App installation the resolver matches against the
// "acme" repo owner used throughout these tests.
func acmeInstall() []domain.OrgGitHubAppInstallation {
	return []domain.OrgGitHubAppInstallation{
		{InstallationID: "111", AccountType: "Organization", AccountLogin: "acme"},
	}
}

// captureLog redirects component-logger output to a buffer for the duration of
// the test so a handler's "github not configured" log line can be asserted.
// logging.SetOutput restores the prior destination on cleanup, so it composes
// safely if anything else has already redirected output. The handler tests
// don't run in parallel and newTestServer starts no background goroutines, so
// the global swap is safe.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	t.Cleanup(logging.SetOutput(&buf))
	return &buf
}

func seedDraftPRArtifact(t *testing.T, s *Server, owner, repo string) string {
	t.Helper()
	// artifacts.conversation_id REFERENCES conversations(id), so mint a full
	// entity→event→prompt→task→blueprint_run→run chain and hang the draft PR
	// artifact off it. The owner/repo the resolver keys on are encoded in the
	// artifact's target (owner/repo#number), independent of the entity's source.
	runID := seedSteerRun(t, s.db, "ppr-"+uuid.New().String()[:8], "completed")
	a := domain.NewPullRequestArtifact(owner+"/"+repo, 42, "PR_node", "feature/x", "main",
		"https://example.test/"+owner+"/"+repo+"/pull/42", "Add thing", "Body.", true)
	a.ConversationID = runID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(s.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a)
	if err != nil {
		t.Fatalf("seed draft PR artifact: %v", err)
	}
	return stored.ID
}

// --- PR artifact read (resolver tier coverage for the GitHub-native PR path) ---

func TestArtifactGet_AppOnlyOrg_Success(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var gotAuth string
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "title": "Live title", "body": "Live body"})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID := seedDraftPRArtifact(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodGet, "/api/artifacts/"+artID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Title/body come from the live PR (GetPR), not the artifact snapshot.
	if out["title"] != "Live title" || out["body"] != "Live body" {
		t.Errorf("title/body = %v/%v, want live values", out["title"], out["body"])
	}
	if out["number"] != float64(42) {
		t.Errorf("number = %v, want 42", out["number"])
	}
	// The live read reached GitHub through the App installation token (App bot
	// identity), not a PAT.
	if gotAuth != "Bearer ghs_111" {
		t.Errorf("PR read with auth %q, want App installation token Bearer ghs_111", gotAuth)
	}
}

func TestArtifactGet_NoCredentials_503(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	logs := captureLog(t)

	artID := seedDraftPRArtifact(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodGet, "/api/artifacts/"+artID, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("get = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorBody(t, rec.Body.Bytes(), "GitHub credentials not configured")
	assertLogged(t, logs, "github not configured")
}

// --- repo branches ---

func TestRepoBranches_AppOnlyOrg_Success(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/branches", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "main"}, {"name": "feature/x"}})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	rec := doJSON(t, srv, http.MethodGet, "/api/repos/acme/api/branches", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("branches = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var names []string
	if err := json.Unmarshal(rec.Body.Bytes(), &names); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Join(names, ",") != "main,feature/x" {
		t.Errorf("branches = %v, want [main feature/x]", names)
	}
}

func TestRepoBranches_NoCredentials_400(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	logs := captureLog(t)

	rec := doJSON(t, srv, http.MethodGet, "/api/repos/acme/api/branches", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("branches = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorBody(t, rec.Body.Bytes(), "GitHub not configured")
	assertLogged(t, logs, "github not configured")
}

// --- project-bundle probe ---

func TestProjectBundleProbe_AppOnlyOrg_ResolvesPerRepo(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	stub := httptest.NewServer(newAppAPIMux()) // GET /repos/{owner}/{repo} returns clone_url
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	probe := projectBundleGitHubProbe{resolver: srv.ghResolver, orgID: runmode.LocalDefaultOrgID}
	url, err := probe.CloneURLForRepo(context.Background(), "acme", "api")
	if err != nil {
		t.Fatalf("CloneURLForRepo: %v", err)
	}
	if url != "https://example.test/acme/api.git" {
		t.Errorf("clone url = %q, want resolved-per-repo value", url)
	}
}

func TestProjectBundleProbe_NoCredentials_SurfacesError(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	probe := projectBundleGitHubProbe{resolver: srv.ghResolver, orgID: runmode.LocalDefaultOrgID}
	_, err := probe.CloneURLForRepo(context.Background(), "acme", "api")
	if err == nil {
		t.Fatal("CloneURLForRepo with no credentials = nil error, want a failure")
	}
	// preflightPinnedRepos folds this error into the existing MissingReposError
	// import-failure shape (covered by projectbundle tests); here we just pin
	// that the no-credentials resolve surfaces rather than being swallowed.
	if !errors.Is(err, ghclient.ErrNoGitHubCredentials) {
		t.Errorf("err = %v, want it to wrap ErrNoGitHubCredentials", err)
	}
}

func assertErrorBody(t *testing.T, body []byte, want string) {
	t.Helper()
	var out map[string]string
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode error body: %v; raw=%s", err, string(body))
	}
	if out["error"] != want {
		t.Errorf("error = %q, want %q", out["error"], want)
	}
}

func assertLogged(t *testing.T, logs *bytes.Buffer, want string) {
	t.Helper()
	if !strings.Contains(logs.String(), want) {
		t.Errorf("expected a log line containing %q; got:\n%s", want, logs.String())
	}
}
