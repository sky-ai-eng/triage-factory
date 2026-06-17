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

func seedReview(t *testing.T, s *Server, owner, repo string, number int) string {
	t.Helper()
	id := uuid.New().String()
	if err := sqlitestore.New(s.db).Reviews.Create(context.Background(), runmode.LocalDefaultOrgID, domain.PendingReview{
		ID: id, PRNumber: number, Owner: owner, Repo: repo, CommitSHA: "sha",
	}); err != nil {
		t.Fatalf("seed review: %v", err)
	}
	return id
}

// seedSubmittableReview seeds a review whose review_event is set, which
// handleReviewSubmit requires before it will post to GitHub. Event is COMMENT
// so the submit path skips the APPROVE-only auto-merge guardrail (which would
// otherwise GET the PR).
func seedSubmittableReview(t *testing.T, s *Server, owner, repo string, number int) string {
	t.Helper()
	id := seedReview(t, s, owner, repo, number)
	if err := sqlitestore.New(s.db).Reviews.SetSubmission(context.Background(), runmode.LocalDefaultOrgID, id, "Looks good.", "COMMENT"); err != nil {
		t.Fatalf("set submission: %v", err)
	}
	return id
}

func seedPendingPR(t *testing.T, s *Server, owner, repo string) string {
	t.Helper()
	// pending_prs.run_id is NOT NULL REFERENCES runs(id), so mint a full
	// entity→event→prompt→task→blueprint_run→run chain and hang the pending PR
	// off it. The owner/repo the resolver keys on live on the pending_prs row
	// and are independent of the entity's source.
	runID := seedSteerRun(t, s.db, "ppr-"+uuid.New().String()[:8], "pending_approval")
	id := uuid.New().String()
	if err := sqlitestore.New(s.db).PendingPRs.Create(context.Background(), runmode.LocalDefaultOrgID, domain.PendingPR{
		ID: id, RunID: runID, Owner: owner, Repo: repo, HeadBranch: "feature/x", BaseBranch: "main", Title: "Add thing", Body: "Body.",
	}); err != nil {
		t.Fatalf("seed pending pr: %v", err)
	}
	return id
}

// --- review diff ---

func TestReviewDiff_AppOnlyOrg_Success(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	const diffText = "diff --git a/f b/f\n@@ -1 +1 @@\n-old\n+new\n"
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(diffText))
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	reviewID := seedReview(t, srv, "acme", "api", 7)
	rec := doJSON(t, srv, http.MethodGet, "/api/reviews/"+reviewID+"/diff", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("diff = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != diffText {
		t.Errorf("diff body = %q, want %q", rec.Body.String(), diffText)
	}
}

func TestReviewDiff_NoCredentials_503(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	logs := captureLog(t)

	reviewID := seedReview(t, srv, "acme", "api", 7)
	rec := doJSON(t, srv, http.MethodGet, "/api/reviews/"+reviewID+"/diff", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("diff = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorBody(t, rec.Body.Bytes(), "GitHub credentials not configured")
	assertLogged(t, logs, "github not configured")
}

// --- review submit ---

func TestReviewSubmit_AppOnlyOrg_Success(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var gotAuth string
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 9988})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	reviewID := seedSubmittableReview(t, srv, "acme", "api", 7)
	rec := doJSON(t, srv, http.MethodPost, "/api/reviews/"+reviewID+"/submit", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("submit = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["github_review_id"] != float64(9988) {
		t.Errorf("github_review_id = %v, want 9988", out["github_review_id"])
	}
	// The write reached GitHub through the App installation token (App bot
	// identity), not a PAT — the intentional App-mode behavior TFAC-327 notes.
	if gotAuth != "Bearer ghs_111" {
		t.Errorf("review posted with auth %q, want App installation token Bearer ghs_111", gotAuth)
	}
}

func TestReviewSubmit_NoCredentials_503(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	logs := captureLog(t)

	reviewID := seedSubmittableReview(t, srv, "acme", "api", 7)
	rec := doJSON(t, srv, http.MethodPost, "/api/reviews/"+reviewID+"/submit", map[string]any{})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("submit = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorBody(t, rec.Body.Bytes(), "GitHub credentials not configured")
	assertLogged(t, logs, "github not configured")
}

// --- pending-PR submit ---

func TestPendingPRSubmit_AppOnlyOrg_Success(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var gotAuth string
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "html_url": "https://example.test/acme/api/pull/42"})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	prID := seedPendingPR(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodPost, "/api/pending-prs/"+prID+"/submit", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("submit = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["number"] != float64(42) {
		t.Errorf("number = %v, want 42", out["number"])
	}
	if out["html_url"] != "https://example.test/acme/api/pull/42" {
		t.Errorf("html_url = %v", out["html_url"])
	}
	// Published through the App installation token, not a PAT (App bot identity).
	if gotAuth != "Bearer ghs_111" {
		t.Errorf("PR opened with auth %q, want App installation token Bearer ghs_111", gotAuth)
	}
}

func TestPendingPRSubmit_NoCredentials_503(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	logs := captureLog(t)

	prID := seedPendingPR(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodPost, "/api/pending-prs/"+prID+"/submit", map[string]any{})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("submit = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorBody(t, rec.Body.Bytes(), "GitHub credentials not configured")
	assertLogged(t, logs, "github not configured")

	// The resolve happens BEFORE the concurrent-submit guard, so a no-creds
	// failure must leave the row un-guarded (not stuck "in flight"). A retry
	// once creds exist must not 409.
	pr, err := sqlitestore.New(srv.db).PendingPRs.Get(context.Background(), runmode.LocalDefaultOrgID, prID)
	if err != nil {
		t.Fatalf("re-read pending pr: %v", err)
	}
	if pr.SubmittedAt != nil {
		t.Errorf("submit guard set after a no-credentials failure; row would be stuck in flight")
	}
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
