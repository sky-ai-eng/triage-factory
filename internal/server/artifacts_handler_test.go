package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// getArtifact re-reads an artifact by id straight from the store (no RLS in
// SQLite), for asserting post-conditions.
func getArtifact(t *testing.T, s *Server, id string) *domain.Artifact {
	t.Helper()
	a, err := sqlitestore.New(s.db).Artifacts.Get(context.Background(), runmode.LocalDefaultOrgID, id)
	if err != nil {
		t.Fatalf("re-read artifact: %v", err)
	}
	if a == nil {
		t.Fatalf("artifact %s not found", id)
	}
	return a
}

// TestArtifactUpdate_Success pins the 1:1 live edit: PATCH writes title/body to
// GitHub via UpdatePR and refreshes the artifact's mutable snapshot while the
// proposed (agent draft) snapshot stays frozen.
func TestArtifactUpdate_Success(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var gotPatch map[string]any
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotPatch)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID := seedDraftPRArtifact(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodPatch, "/api/artifacts/"+artID, map[string]any{"title": "Edited title", "body": "Edited body"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The live edit reached GitHub with the full title+body (whole-field replace).
	if gotPatch["title"] != "Edited title" || gotPatch["body"] != "Edited body" {
		t.Errorf("UpdatePR body = %v, want edited title+body", gotPatch)
	}
	// Snapshot moved; proposed stayed the agent's draft.
	d, _ := domain.ParsePRArtifactDetails(getArtifact(t, srv, artID).DetailsJSON)
	if d.Snapshot.Title != "Edited title" || d.Snapshot.Body != "Edited body" {
		t.Errorf("snapshot = %+v, want edited", d.Snapshot)
	}
	if d.Proposed.Title != "Add thing" || d.Proposed.Body != "Body." {
		t.Errorf("proposed = %+v, want frozen agent draft", d.Proposed)
	}
}

// TestArtifactUpdate_GitHubFailure_Pessimistic pins the pessimistic contract: a
// GitHub rejection (422) returns non-2xx and the snapshot is NOT moved — no
// silent success over a write GitHub refused.
func TestArtifactUpdate_GitHubFailure_Pessimistic(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Validation Failed","errors":[{"message":"title is too long"}]}`, http.StatusUnprocessableEntity)
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID := seedDraftPRArtifact(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodPatch, "/api/artifacts/"+artID, map[string]any{"title": "New", "body": "New body"})
	if rec.Code < 400 {
		t.Fatalf("patch = %d, want non-2xx on GitHub failure; body=%s", rec.Code, rec.Body.String())
	}
	// Snapshot must be untouched (still the agent's draft).
	d, _ := domain.ParsePRArtifactDetails(getArtifact(t, srv, artID).DetailsJSON)
	if d.Snapshot.Title != "Add thing" || d.Snapshot.Body != "Body." {
		t.Errorf("snapshot moved on a failed UpdatePR: %+v", d.Snapshot)
	}
}

// TestArtifactApprove pins the promote-on-approval flow: the body gets the
// agentmeta footer (UpdatePR), the draft is marked ready (MarkPRReady via
// GraphQL), the artifact flips to open, the run completes, and the human verdict
// lands in run_memory. Task closure rides the blueprint resume (spawner-driven),
// covered by the delegate blueprint tests; here the run has no spawner so we
// assert the deterministic server-side bookkeeping.
func TestArtifactApprove(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var patched, marked bool
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		patched = true
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "node_id": "PR_node"})
	})
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		marked = true
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"pullRequest": map[string]any{"isDraft": false}}}})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, runID, _ := seedDraftPRArtifactWithRun(t, srv, "appr", "acme", "api", 42)
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !patched || !marked {
		t.Errorf("approve must UpdatePR (got %v) and MarkPRReady (got %v)", patched, marked)
	}
	if got := getArtifact(t, srv, artID).State; got != domain.ArtifactStatePROpen {
		t.Errorf("artifact state = %q, want open", got)
	}
	var runStatus string
	if err := srv.db.QueryRow(`SELECT status FROM runs WHERE id=?`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "completed" {
		t.Errorf("run status = %q, want completed", runStatus)
	}
	var human string
	if err := srv.db.QueryRow(`SELECT COALESCE(human_content,'') FROM run_memory WHERE run_id=?`, runID).Scan(&human); err != nil {
		t.Fatalf("read run_memory: %v", err)
	}
	if !strings.Contains(human, "as drafted") {
		t.Errorf("human_content = %q, want the 'as drafted' verdict (proposed==final)", human)
	}
}

// TestArtifactAbandon_ClosesDraftPR pins the "Return to queue" path: requeueing a
// task whose run opened a draft PR closes that PR on GitHub (ClosePR → state
// closed) and flips its artifact to closed. The pushed branch is untouched.
func TestArtifactAbandon_ClosesDraftPR(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var closeState string
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		closeState, _ = body["state"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	taskID, _, artID := seedClaimedPRApprovalFixture(t, srv, "acme", "api", 7)
	rec := doJSON(t, srv, http.MethodPost, "/api/tasks/"+taskID+"/requeue", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("requeue = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if closeState != "closed" {
		t.Errorf("ClosePR sent state=%q, want closed", closeState)
	}
	if got := getArtifact(t, srv, artID).State; got != domain.ArtifactStatePRClosed {
		t.Errorf("artifact state = %q, want closed", got)
	}
}

// seedDraftPRArtifactWithRun mints a pending_approval run chain (entity → … →
// run) and a draft pull_request artifact hung off it, returning all three ids.
func seedDraftPRArtifactWithRun(t *testing.T, s *Server, suffix, owner, repo string, number int) (artifactID, runID, taskID string) {
	t.Helper()
	runID = seedSteerRun(t, s.db, suffix, "pending_approval")
	taskID = "t_" + suffix
	// run_memory row so the human-verdict UPDATE has a target (the termination
	// upsert guarantees this in production).
	if err := sqlitestore.New(s.db).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, runID, "e_"+suffix, "", "agent self-report"); err != nil {
		t.Fatalf("seed agent memory: %v", err)
	}
	a := domain.NewPullRequestArtifact(owner+"/"+repo, number, "PR_node", "feature/x", "main",
		fmt.Sprintf("https://example.test/%s/%s/pull/%d", owner, repo, number), "Proposed title", "Proposed body", true)
	a.RunID = runID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(s.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a)
	if err != nil {
		t.Fatalf("seed draft PR artifact: %v", err)
	}
	return stored.ID, runID, taskID
}

// seedClaimedPRApprovalFixture builds a claimed task (the shape /requeue expects)
// whose pending_approval run opened a draft PR. Returns (taskID, runID, artifactID).
func seedClaimedPRApprovalFixture(t *testing.T, s *Server, owner, repo string, number int) (taskID, runID, artifactID string) {
	t.Helper()
	const eventType = "github:pr:ci_check_passed"
	execSQL(t, s.db, `INSERT INTO entities (id, source, source_id, kind, state) VALUES ('e_ab', 'github', ?, 'pr', 'active')`, fmt.Sprintf("%s/%s#%d", owner, repo, number))
	execSQL(t, s.db, `INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_ab', 'e_ab', ?, '')`, eventType)
	execSQL(t, s.db, `INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_ab', 'P', 'b', ?, ?)`, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
	execSQL(t, s.db, `INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_agent_id) VALUES ('t_ab', 'e_ab', ?, 'ev_ab', 'queued', ?)`, eventType, runmode.LocalDefaultAgentID)
	brID := seedBlueprintRunSQLite(t, s.db, "t_ab")
	execSQL(t, s.db, `INSERT INTO runs (id, task_id, prompt_id, status, trigger_type, blueprint_run_id) VALUES ('r_ab', 't_ab', 'p_ab', 'pending_approval', 'manual', ?)`, brID)
	if err := sqlitestore.New(s.db).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, "r_ab", "e_ab", "", "agent self-report"); err != nil {
		t.Fatalf("seed agent memory: %v", err)
	}
	a := domain.NewPullRequestArtifact(owner+"/"+repo, number, "PR_node", "feature/x", "main",
		fmt.Sprintf("https://example.test/%s/%s/pull/%d", owner, repo, number), "Proposed title", "Proposed body", true)
	a.RunID = "r_ab"
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(s.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a)
	if err != nil {
		t.Fatalf("seed draft PR artifact: %v", err)
	}
	return "t_ab", "r_ab", stored.ID
}
