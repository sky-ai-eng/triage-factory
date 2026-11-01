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

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The deliverable and branch handlers reach GitHub through the credential
// resolver, so an App-only org (no PAT) uses its App installation token. These
// tests drive each call site against an App-only org and an org with no
// credentials at all.

// newAppAPIMux returns a ServeMux serving the App installation-token mint (the
// token encodes the installation id) and GET /repos/{owner}/{repo}, the
// coverage probe and repo-meta read. Tests add their own endpoint and seedApp it.
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

// captureLog redirects component-logger output to a buffer for the test, and
// restores the prior destination on cleanup. The swap is global, which is safe
// because these tests do not run in parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	t.Cleanup(logging.SetOutput(&buf))
	return &buf
}

func seedDraftPRDeliverable(t *testing.T, s *Server, owner, repo string) string {
	t.Helper()
	// deliverables.conversation_id REFERENCES conversations(id), so mint a full
	// entity→event→prompt→task→blueprint_run→run chain and hang the draft PR
	// deliverable off it. The owner/repo the resolver keys on are encoded in the
	// deliverable's target (owner/repo#number), independent of the entity's source.
	conversationID := seedSteerConversation(t, s.db, "ppr-"+uuid.New().String()[:8], dbtest.SeedConcluded)
	a := domain.NewPullRequestDeliverable(owner+"/"+repo, 42, "PR_"+owner+"_"+repo+"_42", "feature/x", "main",
		"https://example.test/"+owner+"/"+repo+"/pull/42", "Add thing", "Body.", true)
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(s.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(s.db).Orgs, runmode.LocalDefaultOrgID, a))
	if err != nil {
		t.Fatalf("seed draft PR deliverable: %v", err)
	}
	return stored.ID
}

// --- PR deliverable read (resolver tier coverage for the GitHub-native PR path) ---

// TestDeliverablePR_AddressedByItsEntitysRepositoryRow: an App-only org reads a
// PR deliverable live, with the App installation token, under the name its
// entity's registry row holds now. Once the row is retired the read serves the
// snapshot and a write is refused, without reaching GitHub.
func TestDeliverablePR_AddressedByItsEntitysRepositoryRow(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var paths []string
	var gotAuth string
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "title": "Live title", "body": "Live body"})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedDraftPRDeliverableWithConversation(t, srv, "renamed-pr", "acme", "api", 42)
	art := getDeliverable(t, srv, artID)
	entity, err := sqlitestore.New(srv.db).Entities.GetSystem(context.Background(), runmode.LocalDefaultOrgID, art.EntityID)
	if err != nil || entity == nil {
		t.Fatalf("read the deliverable's entity: got=%v err=%v", entity, err)
	}
	dbseed.RenameRepositoryOnly(t, srv.db, entity.RepositoryID, "acme", "renamed")

	rec := doJSON(t, srv, http.MethodGet, "/api/deliverables/"+artID, nil)
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
	if len(paths) != 1 || paths[0] != "/api/v3/repos/acme/renamed/pulls/42" {
		t.Errorf("live reads = %v, want the one under acme/renamed", paths)
	}
	if gotAuth != "Bearer ghs_111" {
		t.Errorf("PR read with auth %q, want App installation token Bearer ghs_111", gotAuth)
	}
	if out.Kind != domain.DeliverableKindPullRequest {
		t.Errorf("kind = %q, want pull_request", out.Kind)
	}
	// Title and body come from the live PR, not the deliverable's snapshot.
	if out.Details["repo"] != "renamed" || out.Details["title"] != "Live title" ||
		out.Details["body"] != "Live body" || out.Details["number"] != float64(42) {
		t.Errorf("details = %v, want the live PR #42 under its repository's current name", out.Details)
	}

	execSQL(t, srv.db, `UPDATE repositories SET retired_at = CURRENT_TIMESTAMP WHERE id = ?`, entity.RepositoryID)
	paths = nil
	rec = doJSON(t, srv, http.MethodGet, "/api/deliverables/"+artID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get after retire = %d, want 200 (the snapshot); body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("approve after retire = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if len(paths) != 0 {
		t.Errorf("GitHub requests after retire = %v, want none", paths)
	}
}

// TestDeliverableGet_NoCredentials_NotConfigured: a deliverable read on an org
// with no GitHub credential is 409 NOT_CONFIGURED, configuration the caller
// must fix, not a 503 that reads as a transient outage.
func TestDeliverableGet_NoCredentials_NotConfigured(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	logs := captureLog(t)

	artID := seedDraftPRDeliverable(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodGet, "/api/deliverables/"+artID, nil)
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
