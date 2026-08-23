package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TFAC-327: the review diff/submit, pending-PR submit, and branches
// handlers were migrated off the PAT-only process-global
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
	conversationID := seedSteerConversation(t, s.db, "ppr-"+uuid.New().String()[:8], "completed")
	a := domain.NewPullRequestArtifact(owner+"/"+repo, 42, "PR_node", "feature/x", "main",
		"https://example.test/"+owner+"/"+repo+"/pull/42", "Add thing", "Body.", true)
	a.ConversationID = conversationID
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
	var out struct {
		Kind    string         `json:"kind"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Kind != domain.ArtifactKindPullRequest {
		t.Errorf("kind = %q, want pull_request", out.Kind)
	}
	// Title/body come from the live PR (GetPR), not the artifact snapshot.
	if out.Details["title"] != "Live title" || out.Details["body"] != "Live body" {
		t.Errorf("title/body = %v/%v, want live values", out.Details["title"], out.Details["body"])
	}
	if out.Details["number"] != float64(42) {
		t.Errorf("number = %v, want 42", out.Details["number"])
	}
	// The live read reached GitHub through the App installation token (App bot
	// identity), not a PAT.
	if gotAuth != "Bearer ghs_111" {
		t.Errorf("PR read with auth %q, want App installation token Bearer ghs_111", gotAuth)
	}
}

// TestArtifactGet_NoCredentials_NotConfigured: an artifact read on a workspace
// with no GitHub credential is 409 NOT_CONFIGURED — server-side configuration
// the caller must fix, not a transient upstream outage (the 503 it used to
// answer read as "try again later").
func TestArtifactGet_NoCredentials_NotConfigured(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	logs := captureLog(t)

	artID := seedDraftPRArtifact(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodGet, "/api/artifacts/"+artID, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("get = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonNotConfigured, "")
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
	repoID := seedConfiguredRepo(t, srv, "acme", "api")

	rec := doJSON(t, srv, http.MethodPost, "/api/repos/"+repoID+"/branches/list", map[string]any{})
	page := decodeList[branchJSON](t, rec)
	// A proxy list cannot count its upstream, so total_count is null.
	if page.TotalCount != nil {
		t.Errorf("total_count = %d, want null on a proxy list", *page.TotalCount)
	}
	names := make([]string, len(page.Items))
	for i, b := range page.Items {
		names[i] = b.Name
	}
	if strings.Join(names, ",") != "main,feature/x" {
		t.Errorf("branches = %v, want [main feature/x]", names)
	}
	// Two rows against the default 50-row window is a short page, which is
	// how GitHub says there is no more — so no next token.
	if page.NextPageToken != "" {
		t.Errorf("next_page_token = %q, want empty on a short upstream page", page.NextPageToken)
	}
}

func TestRepoBranches_NoCredentials_NotConfigured(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	logs := captureLog(t)
	repoID := seedConfiguredRepo(t, srv, "acme", "api")

	rec := doJSON(t, srv, http.MethodPost, "/api/repos/"+repoID+"/branches/list", map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("branches = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonNotConfigured, "")
	assertLogged(t, logs, "github not configured")
}

func assertLogged(t *testing.T, logs *bytes.Buffer, want string) {
	t.Helper()
	if !strings.Contains(logs.String(), want) {
		t.Errorf("expected a log line containing %q; got:\n%s", want, logs.String())
	}
}
