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
// GraphQL), the artifact flips to open, and the human verdict lands in conversation_memory.
// Approval is a decoupled sidecar: it must NOT touch run status. The fixture
// pre-seeds the run as 'completed', and we assert it STAYS 'completed' (approve
// didn't flip it). Task closure here is a no-op because the fixture blueprint_run
// is still 'running' (not a clean completion); the terminal-on-last closure is
// covered by the dedicated dismiss/closure tests.
func TestArtifactApprove(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var marked bool
	var patchBody map[string]any
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&patchBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	})
	// approve reads the LIVE PR for the content to promote, so GET must serve the
	// current title/body (matching the proposed snapshot here → "as drafted").
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "node_id": "PR_node", "title": "Proposed title", "body": "Proposed body"})
	})
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		marked = true
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"pullRequest": map[string]any{"isDraft": false}}}})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, conversationID, _ := seedDraftPRArtifactWithConversation(t, srv, "appr", "acme", "api", 42)
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if patchBody == nil || !marked {
		t.Errorf("approve must UpdatePR (got %v) and MarkPRReady (got %v)", patchBody, marked)
	}
	// The promoted body is the live content with the agentmeta footer appended.
	if got, _ := patchBody["body"].(string); !strings.HasPrefix(got, "Proposed body") || !strings.Contains(got, "Triage Factory") {
		t.Errorf("UpdatePR body = %q, want live body + footer", got)
	}
	if got := getArtifact(t, srv, artID).State; got != domain.ArtifactStatePROpen {
		t.Errorf("artifact state = %q, want open", got)
	}
	var runStatus string
	if err := srv.db.QueryRow(`SELECT status FROM conversations WHERE id=?`, conversationID).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "completed" {
		t.Errorf("run status = %q, want completed", runStatus)
	}
	var human string
	if err := srv.db.QueryRow(`SELECT COALESCE(human_content,'') FROM conversation_memory WHERE conversation_id=?`, conversationID).Scan(&human); err != nil {
		t.Fatalf("read conversation_memory: %v", err)
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

// TestArtifactTeardown_ResolvesAllArtifacts pins the task-level resolve-all
// gesture (Return-to-queue): a task whose run holds MULTIPLE unresolved artifacts
// — a draft PR and a pending review — has them ALL resolved. The draft PR is
// closed on GitHub; the review is flipped to dismissed with NO GitHub call (it
// was staged TF-side, TFAC-494). Branches are kept.
func TestArtifactTeardown_ResolvesAllArtifacts(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var prClosed, reviewTouchedGitHub bool
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		prClosed = true
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
	})
	// Any review-delete call (GraphQL node lookup or REST delete) would be a
	// regression — a staged review has no GitHub object to retire.
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		reviewTouchedGitHub = true
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	mux.HandleFunc("DELETE /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews/{rid}", func(w http.ResponseWriter, r *http.Request) {
		reviewTouchedGitHub = true
		_ = json.NewEncoder(w).Encode(map[string]any{})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	// seedClaimedPRApprovalFixture hangs a draft PR (#7) off run r_ab; add a
	// finalized review draft on the same run so the task holds two unresolved
	// artifacts of different kinds.
	taskID, conversationID, prArtID := seedClaimedPRApprovalFixture(t, srv, "acme", "api", 7)
	rv := domain.NewReviewArtifact("acme/api", 7, "headsha7", conversationID)
	rv.ConversationID = conversationID
	rv.OrgID = runmode.LocalDefaultOrgID
	rv.TeamID = runmode.LocalDefaultTeamID
	rd, _ := domain.ParseReviewArtifactDetails(rv.DetailsJSON)
	rd.ReviewBody = "body"
	rd.ReviewEvent = "COMMENT"
	rv.DetailsJSON = domain.MarshalReviewArtifactDetails(rd)
	reviewStored, err := sqlitestore.New(srv.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, rv)
	if err != nil {
		t.Fatalf("seed review artifact: %v", err)
	}

	rec := doJSON(t, srv, http.MethodPost, "/api/tasks/"+taskID+"/requeue", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("requeue = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !prClosed {
		t.Error("resolve-all must close the draft PR on GitHub")
	}
	if reviewTouchedGitHub {
		t.Error("resolve-all must NOT make any GitHub call for the staged review")
	}
	if got := getArtifact(t, srv, prArtID).State; got != domain.ArtifactStatePRClosed {
		t.Errorf("PR artifact state = %q, want closed", got)
	}
	if got := getArtifact(t, srv, reviewStored.ID).State; got != domain.ArtifactStateReviewDismissed {
		t.Errorf("review artifact state = %q, want dismissed", got)
	}
}

// TestArtifactDismiss_PR pins the per-artifact dismiss sidecar: POST
// /api/artifacts/{id}/dismiss closes the draft PR on GitHub (ClosePR → state
// closed), flips the artifact to closed, and — crucially — never touches the
// run's lifecycle. The pushed branch is untouched (we send only state=closed).
func TestArtifactDismiss_PR(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var closeState string
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		closeState, _ = body["state"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "closed"})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, conversationID, _ := seedDraftPRArtifactWithConversation(t, srv, "dis", "acme", "api", 42)
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/dismiss", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if closeState != "closed" {
		t.Errorf("ClosePR sent state=%q, want closed", closeState)
	}
	if got := getArtifact(t, srv, artID).State; got != domain.ArtifactStatePRClosed {
		t.Errorf("artifact state = %q, want closed", got)
	}
	// The run lifecycle is untouched — dismiss is a decoupled sidecar.
	var runStatus string
	if err := srv.db.QueryRow(`SELECT status FROM conversations WHERE id=?`, conversationID).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "completed" {
		t.Errorf("run status = %q, want completed (dismiss must not flip run lifecycle)", runStatus)
	}
}

// TestArtifactDismiss_TerminalArtifact409 pins the guard: dismissing an artifact
// that's already resolved (a non-draft PR) is a 409 — there's nothing left to
// resolve. A second dismiss on an already-closed PR exercises the path.
func TestArtifactDismiss_TerminalArtifact409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	artID, _, _ := seedDraftPRArtifactWithConversation(t, srv, "dis409", "acme", "api", 42)
	// Pre-resolve the artifact so the state guard fires before any GitHub call.
	execSQL(t, srv.db, `UPDATE artifacts SET state = ? WHERE id = ?`, domain.ArtifactStatePRClosed, artID)
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/dismiss", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("dismiss of terminal artifact = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestArtifactDismiss_TerminalBlueprintLastArtifactClosesTask pins §3: resolving
// the LAST unresolved artifact on an already-terminal blueprint closes the task
// (done), with no accept/dismiss distinction — a dismiss closes it just like an
// approve would.
func TestArtifactDismiss_TerminalBlueprintLastArtifactClosesTask(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "closed"})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, taskID := seedDraftPRArtifactWithConversation(t, srv, "tolc", "acme", "api", 42)
	// Drive the blueprint to a terminal status so resolving the only artifact is
	// the last-on-terminal trigger.
	execSQL(t, srv.db, `UPDATE blueprint_runs SET status = 'completed' WHERE task_id = ?`, taskID)

	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/dismiss", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var taskStatus string
	if err := srv.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&taskStatus); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if taskStatus != "done" {
		t.Errorf("task.status = %q, want done (last resolution on a terminal blueprint closes the task)", taskStatus)
	}
}

// TestArtifactDismiss_AbortedBlueprintLeavesTaskOpen pins the clean-completion
// gate: resolving the last artifact on a NON-completed terminal blueprint
// (aborted / failed / cancelled) must leave the task open for human attention,
// mirroring terminateBlueprint — a stray draft PR on an aborted blueprint doesn't
// override the "needs a human" disposition.
func TestArtifactDismiss_AbortedBlueprintLeavesTaskOpen(t *testing.T) {
	// The non-completed terminal vocabulary, from the domain constants (compile-
	// checked against the real blueprint_runs.status values terminateBlueprint
	// writes) — only 'completed' is a clean finalization that may close the task.
	for _, terminal := range []domain.BlueprintRunStatus{
		domain.BlueprintRunStatusAborted,
		domain.BlueprintRunStatusFailed,
		domain.BlueprintRunStatusCancelled,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			keyring.MockInit()
			srv := newTestServer(t)
			mux := newAppAPIMux()
			mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "closed"})
			})
			stub := httptest.NewServer(mux)
			t.Cleanup(stub.Close)
			seedApp(t, srv, stub, acmeInstall())

			artID, _, taskID := seedDraftPRArtifactWithConversation(t, srv, "tolab-"+string(terminal), "acme", "api", 42)
			execSQL(t, srv.db, `UPDATE blueprint_runs SET status = ? WHERE task_id = ?`, string(terminal), taskID)

			rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/dismiss", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("dismiss = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var taskStatus string
			if err := srv.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&taskStatus); err != nil {
				t.Fatalf("read task: %v", err)
			}
			if taskStatus == "done" {
				t.Errorf("task.status = done; a %s blueprint must leave the task open for a human", terminal)
			}
		})
	}
}

// TestArtifactDismiss_TaskLessRunClosesNoTask pins the non-blueprint branch
// against the actual model: a future user-triggered run (origin='interactive') is
// neither a blueprint nor a task run — it carries a NULL blueprint_run_id AND a
// NULL task_id (both allowed once origin <> 'blueprint'). Resolving an artifact on
// such a run must be a clean no-op for task closure: GetRunForConversation routes to the
// standalone branch, the run has no task, and the taskID == "" guard short-circuits
// — no task is closed, no error. (No such run exists in production today; this
// pins the forward-compat path so it can't regress into closing a phantom task.)
func TestArtifactDismiss_TaskLessRunClosesNoTask(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "closed"})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	// A task-less, blueprint-less interactive run: origin='interactive' with
	// NULL task_id and NULL blueprint_run_id (tolerated by
	// conversations_origin_requires_parents only for origin <> 'blueprint').
	execSQL(t, srv.db, `INSERT INTO conversations (id, status, trigger_type, origin, outcome, team_id, visibility) VALUES ('r_int', 'completed', 'manual', 'interactive', 'abort', ?, 'team')`, runmode.LocalDefaultTeamID)
	a := domain.NewPullRequestArtifact("acme/api", 42, "PR_node", "feature/x", "main", "https://example.test/acme/api/pull/42", "Proposed title", "Proposed body", true)
	a.ConversationID = "r_int"
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(srv.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a)
	if err != nil {
		t.Fatalf("seed draft PR artifact: %v", err)
	}

	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+stored.ID+"/dismiss", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The artifact resolved (the load-bearing effect), and the closure check no-oped
	// cleanly — a task-less run has no task to close.
	if got := getArtifact(t, srv, stored.ID).State; got != domain.ArtifactStatePRClosed {
		t.Errorf("artifact state = %q, want closed", got)
	}
}

// TestArtifactDismiss_LiveBlueprintLeavesTaskOpen is the counterpart: resolving
// the last artifact while the blueprint is still LIVE (not terminal) leaves the
// task open — the running blueprint keeps going and re-checks task closure when
// it terminates.
func TestArtifactDismiss_LiveBlueprintLeavesTaskOpen(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "closed"})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	// seedDraftPRArtifactWithConversation leaves the blueprint_run 'running' (live).
	artID, _, taskID := seedDraftPRArtifactWithConversation(t, srv, "tolo", "acme", "api", 42)
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/dismiss", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var taskStatus string
	if err := srv.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&taskStatus); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if taskStatus == "done" {
		t.Errorf("task.status = done; a live blueprint must leave the task open on resolve")
	}
}

// TestArtifactUpdate_SingleField_UsesLiveBaseline pins the lost-update fix: a
// PATCH that touches only the title fills the untouched body from the PR's CURRENT
// live value, not a stale cached snapshot — so a body edited directly on GitHub
// isn't silently reverted.
func TestArtifactUpdate_SingleField_UsesLiveBaseline(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var patchBody map[string]any
	mux := newAppAPIMux()
	// The live body diverged from the snapshot (seeded as "Body.") via a direct
	// GitHub edit.
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "title": "Live title", "body": "Body edited on GitHub"})
	})
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&patchBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID := seedDraftPRArtifact(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodPatch, "/api/artifacts/"+artID, map[string]any{"title": "New title"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if patchBody["title"] != "New title" {
		t.Errorf("UpdatePR title = %v, want New title", patchBody["title"])
	}
	if patchBody["body"] != "Body edited on GitHub" {
		t.Errorf("UpdatePR body = %v, want the live body (no lost update); the stale snapshot was %q", patchBody["body"], "Body.")
	}
}

// TestArtifactUpdate_PartialEdit_GetPRFailure_502 pins that when the live
// baseline read fails on a single-field PATCH, the handler fails rather than
// clobbering the untouched field from a stale snapshot.
func TestArtifactUpdate_PartialEdit_GetPRFailure_502(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	patched := false
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		patched = true
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID := seedDraftPRArtifact(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodPatch, "/api/artifacts/"+artID, map[string]any{"title": "New title"})
	if rec.Code < 400 {
		t.Fatalf("patch = %d, want non-2xx when the live baseline read fails", rec.Code)
	}
	if patched {
		t.Error("UpdatePR must not run when the baseline read failed — it would clobber the untouched field")
	}
}

// TestArtifactApprove_MalformedDetails_PromotesFromLive pins the data-loss fix:
// even when details_json is unparseable, approve promotes the PR from its LIVE
// content — never empty title/body that would wipe the PR.
func TestArtifactApprove_MalformedDetails_PromotesFromLive(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var patchBody map[string]any
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "node_id": "PR_node", "title": "Live title", "body": "Live body"})
	})
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&patchBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	})
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"pullRequest": map[string]any{"isDraft": false}}}})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedDraftPRArtifactWithConversation(t, srv, "appmal", "acme", "api", 42)
	if _, err := srv.db.Exec(`UPDATE artifacts SET details_json='{not valid json' WHERE id=?`, artID); err != nil {
		t.Fatalf("corrupt details: %v", err)
	}
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if title, _ := patchBody["title"].(string); title != "Live title" {
		t.Errorf("UpdatePR title = %q, want live title (never empty from malformed details)", title)
	}
	if got, _ := patchBody["body"].(string); !strings.HasPrefix(got, "Live body") {
		t.Errorf("UpdatePR body = %q, want live body (+footer), never empty", got)
	}
}

// TestArtifactApprove_FooterIdempotent pins that approving a PR whose live body
// already carries a footer (a prior partially-failed approve) strips it before
// re-appending, so exactly one footer survives.
func TestArtifactApprove_FooterIdempotent(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var patchBody map[string]any
	liveBody := "Real body\n\n---\n*This PR was partially generated by AI using [Triage Factory](https://github.com/sky-ai-eng/triage-factory).*\n\nTime: 1s | Cost: $0.001"
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "node_id": "PR_node", "title": "T", "body": liveBody})
	})
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&patchBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	})
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"pullRequest": map[string]any{"isDraft": false}}}})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedDraftPRArtifactWithConversation(t, srv, "appftr", "acme", "api", 42)
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got, _ := patchBody["body"].(string)
	if n := strings.Count(got, "partially generated by AI using [Triage Factory]"); n != 1 {
		t.Errorf("footer appears %d times, want exactly 1 (idempotent); body=%q", n, got)
	}
	if !strings.HasPrefix(got, "Real body") {
		t.Errorf("stripped body lost its real content: %q", got)
	}
}

// TestArtifactApprove_NonDraft_409 pins the state guard: approving an artifact
// that's no longer a draft (already open or closed) is a conflict and performs
// no GitHub mutation — a stale double-click can't trigger a spurious footer
// rewrite.
func TestArtifactApprove_NonDraft_409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mutated := false
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		mutated = true
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	})
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		mutated = true
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedDraftPRArtifactWithConversation(t, srv, "appnd", "acme", "api", 42)
	if _, err := srv.db.Exec(`UPDATE artifacts SET state=? WHERE id=?`, domain.ArtifactStatePROpen, artID); err != nil {
		t.Fatalf("flip artifact to open: %v", err)
	}
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve on non-draft = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if mutated {
		t.Error("approve on a non-draft must not perform any GitHub mutation")
	}
}

// seedDraftPRArtifactWithConversation mints a completed (terminal) run chain (entity → … →
// run) and a draft pull_request artifact hung off it, returning all three ids.
func seedDraftPRArtifactWithConversation(t *testing.T, s *Server, suffix, owner, repo string, number int) (artifactID, conversationID, taskID string) {
	t.Helper()
	conversationID = seedSteerConversation(t, s.db, suffix, "completed")
	taskID = fixtureUUID("t_" + suffix)
	// conversation_memory row so the human-verdict UPDATE has a target (the termination
	// upsert guarantees this in production).
	if err := sqlitestore.New(s.db).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, conversationID, fixtureUUID("e_"+suffix), "", "agent self-report"); err != nil {
		t.Fatalf("seed agent memory: %v", err)
	}
	a := domain.NewPullRequestArtifact(owner+"/"+repo, number, "PR_node", "feature/x", "main",
		fmt.Sprintf("https://example.test/%s/%s/pull/%d", owner, repo, number), "Proposed title", "Proposed body", true)
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(s.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a)
	if err != nil {
		t.Fatalf("seed draft PR artifact: %v", err)
	}
	return stored.ID, conversationID, taskID
}

// seedClaimedPRApprovalFixture builds a claimed task (the shape /requeue expects)
// whose completed run opened a draft PR. Returns (taskID, conversationID, artifactID).
func seedClaimedPRApprovalFixture(t *testing.T, s *Server, owner, repo string, number int) (taskID, conversationID, artifactID string) {
	t.Helper()
	const eventType = "github:pr:ci_check_passed"
	execSQL(t, s.db, `INSERT INTO entities (id, source, source_id, kind, state) VALUES ('e_ab', 'github', ?, 'pr', 'active')`, fmt.Sprintf("%s/%s#%d", owner, repo, number))
	execSQL(t, s.db, `INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_ab', 'e_ab', ?, '')`, eventType)
	execSQL(t, s.db, `INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_ab', 'P', 'b', ?, ?)`, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
	execSQL(t, s.db, `INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_agent_id) VALUES ('00000000-0000-4000-8000-000000000023', 'e_ab', ?, 'ev_ab', 'queued', ?)`, eventType, runmode.LocalDefaultAgentID)
	brID := seedBlueprintRunSQLite(t, s.db, "00000000-0000-4000-8000-000000000023")
	execSQL(t, s.db, `INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, blueprint_step_index) VALUES ('r_ab', '00000000-0000-4000-8000-000000000023', 'p_ab', 'completed', 'manual', ?, 0)`, brID)
	if err := sqlitestore.New(s.db).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, "r_ab", "e_ab", "", "agent self-report"); err != nil {
		t.Fatalf("seed agent memory: %v", err)
	}
	a := domain.NewPullRequestArtifact(owner+"/"+repo, number, "PR_node", "feature/x", "main",
		fmt.Sprintf("https://example.test/%s/%s/pull/%d", owner, repo, number), "Proposed title", "Proposed body", true)
	a.ConversationID = "r_ab"
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(s.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a)
	if err != nil {
		t.Fatalf("seed draft PR artifact: %v", err)
	}
	return "00000000-0000-4000-8000-000000000023", "r_ab", stored.ID
}
