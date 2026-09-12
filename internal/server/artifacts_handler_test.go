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
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
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
	rec := doJSON(t, srv, http.MethodPatch, "/api/artifacts/"+artID+"/pr", map[string]any{"title": "Edited title", "body": "Edited body"})
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
	rec := doJSON(t, srv, http.MethodPatch, "/api/artifacts/"+artID+"/pr", map[string]any{"title": "New", "body": "New body"})
	if rec.Code < 400 {
		t.Fatalf("patch = %d, want non-2xx on GitHub failure; body=%s", rec.Code, rec.Body.String())
	}
	// Snapshot must be untouched (still the agent's draft).
	d, _ := domain.ParsePRArtifactDetails(getArtifact(t, srv, artID).DetailsJSON)
	if d.Snapshot.Title != "Add thing" || d.Snapshot.Body != "Body." {
		t.Errorf("snapshot moved on a failed UpdatePR: %+v", d.Snapshot)
	}
}

// TestArtifactWrites_KindScoped is the whole point of splitting the artifact
// write surface: a body shaped for one kind can no longer reach the other
// kind's write path.
//
// The hole it closes is specific. The merged PATCH dispatched on the ROW's
// kind, so a review-shaped body landing on a PR artifact decoded to all-nil
// pointers, sailed past the "partial edit" branch, and performed a real
// UpdatePR — rewriting the PR with its own current content — plus an audit row,
// and answered 200. The mirror case, a PR-shaped body on a review artifact, was
// ignored with a 200 that promised a write nobody made.
//
// Every stub here fails the test on contact, so "no upstream call" is asserted
// by the GitHub client never being reached at all rather than by inspecting
// what it was asked to do.
func TestArtifactWrites_KindScoped(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var upstreamHits []string
	mux := newAppAPIMux()
	for _, pattern := range []string{
		"PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}",
		"GET /api/v3/repos/{owner}/{repo}/pulls/{number}",
		"POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews",
		"POST /api/graphql",
	} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			upstreamHits = append(upstreamHits, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		})
	}
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	prID := seedDraftPRArtifact(t, srv, "acme", "api")
	reviewID, _, _ := seedReviewArtifactWithConversation(t, srv, "kindscope", "acme", "api", 7, "COMMENT")

	for name, tc := range map[string]struct {
		method string
		path   string
		body   any
		want   int
	}{
		"review body on a PR artifact":    {http.MethodPatch, "/api/artifacts/" + prID + "/review", map[string]any{"body": "lgtm"}, http.StatusConflict},
		"PR body on a review artifact":    {http.MethodPatch, "/api/artifacts/" + reviewID + "/pr", map[string]any{"title": "New"}, http.StatusConflict},
		"comment edit on a PR artifact":   {http.MethodPatch, "/api/artifacts/" + prID + "/comments/c_1", map[string]any{"body": "x"}, http.StatusConflict},
		"comment delete on a PR artifact": {http.MethodDelete, "/api/artifacts/" + prID + "/comments/c_1", nil, http.StatusConflict},
		"refresh on a PR artifact":        {http.MethodPost, "/api/artifacts/" + prID + "/review/refresh", nil, http.StatusConflict},
		"dismiss on a review artifact":    {http.MethodPost, "/api/artifacts/" + reviewID + "/dismiss", nil, http.StatusConflict},
		// A zero-field PR patch is the other half of the same hole: it named
		// nothing, so there is nothing to send GitHub, and the old route sent
		// the PR its own content back anyway.
		"zero-field PR patch": {http.MethodPatch, "/api/artifacts/" + prID + "/pr", map[string]any{}, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, srv, tc.method, tc.path, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d; body=%s", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	if len(upstreamHits) != 0 {
		t.Errorf("refused writes reached GitHub: %v", upstreamHits)
	}
	// And no audit row: external_actions records org-credential writes, so one
	// here would claim a write that never happened.
	var actions int
	if err := srv.db.QueryRow(`SELECT COUNT(*) FROM external_actions`).Scan(&actions); err != nil {
		t.Fatalf("count external_actions: %v", err)
	}
	if actions != 0 {
		t.Errorf("external_actions = %d after refused writes, want 0", actions)
	}
	// Both artifacts are untouched.
	if got := getArtifact(t, srv, prID).State; got != domain.ArtifactStatePRDraft {
		t.Errorf("PR artifact state = %q, want draft (unchanged)", got)
	}
	if got := getArtifact(t, srv, reviewID).State; got != domain.ArtifactStateReviewPending {
		t.Errorf("review artifact state = %q, want pending (unchanged)", got)
	}
}

// TestArtifactGet_ServesEveryKind pins the read union: an artifact whose kind
// has no composed representation answers with itself — the shared envelope,
// `kind` naming the shape, and its stored details_json under `details`. This
// route used to 404 those rows, which said "no such artifact" about one the
// conversation-scoped list was serving happily.
func TestArtifactGet_ServesEveryKind(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	conversationID := seedSteerConversation(t, srv.db, "anykind", "completed")

	branch, ok := domain.NewBranchArtifact("acme/api", "refs/heads/feature/x", "deadbeef", true)
	if !ok {
		t.Fatal("NewBranchArtifact refused a well-formed ref")
	}
	branch.ConversationID = conversationID
	branch.OrgID = runmode.LocalDefaultOrgID
	branch.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(srv.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, branch)
	if err != nil {
		t.Fatalf("seed branch artifact: %v", err)
	}

	rec := doJSON(t, srv, http.MethodGet, "/api/artifacts/"+stored.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get branch artifact = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID      string         `json:"id"`
		Kind    string         `json:"kind"`
		State   string         `json:"state"`
		Target  string         `json:"target"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID != stored.ID || out.Kind != domain.ArtifactKindBranch || out.Target != "acme/api" {
		t.Errorf("envelope = %+v, want the branch artifact's own coordinates", out)
	}
	if out.Details["sha"] != "deadbeef" {
		t.Errorf("details = %v, want the stored branch payload", out.Details)
	}
}

// TestArtifactApprove pins the promote-on-approval flow: the draft is marked
// ready (MarkPRReady via GraphQL), the artifact flips to open, and the human
// verdict lands in conversation_memory — and the PR's title and body are never
// written, because the disclosure footer is already on the body from creation
// and approval has nothing to add. Approval is a decoupled sidecar: it must
// NOT touch conversation status. The fixture pre-seeds the conversation as
// 'completed', and we assert it STAYS 'completed' (approve didn't flip it).
// Task closure here is a no-op because the fixture blueprint_run is still
// 'running' (not a clean completion); the terminal-on-last closure is covered
// by the dedicated dismiss/closure tests.
func TestArtifactApprove(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var marked, patched bool
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		patched = true
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	})
	// approve reads the LIVE PR for the content it records, so GET must serve
	// the current title/body (matching the proposed snapshot here → "as drafted").
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "node_id": "PR_node", "state": "open", "draft": true, "title": "Proposed title", "body": "Proposed body"})
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
	if !marked {
		t.Error("approve must MarkPRReady")
	}
	if patched {
		t.Error("approve must leave the PR's title and body alone")
	}
	if got := getArtifact(t, srv, artID).State; got != domain.ArtifactStatePROpen {
		t.Errorf("artifact state = %q, want open", got)
	}
	var convStatus string
	if err := srv.db.QueryRow(`SELECT status FROM conversations WHERE id=?`, conversationID).Scan(&convStatus); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if convStatus != "completed" {
		t.Errorf("conversation status = %q, want completed", convStatus)
	}
	// Approval writes no memory: the conversation's row is the agent's own
	// account of what it tried, and a verdict about an artifact is not that.
	assertAgentMemoryUntouched(t, srv, conversationID)
}

// TestArtifactApprove_ClosesOnlyWhileTheTaskIsStillTheRunsToClose pins the
// terminal-on-last guards from the request side: approving the last unresolved
// artifact on a task still bot-claimed by its newest run closes it, and the
// same approval on a task that has moved on does not. Artifacts carry across
// requeue, claim and re-delegation, so an approval can land on a carried PR
// long after the run that opened it stopped being what the task is about —
// and closing then would take it to done under whoever holds it today.
//
// Every arm shares one fixture, so the control proves the closure fires here
// and each other arm isolates what stops it. Both guards earn their place: a
// handover clears the agent claim without minting a run, a re-delegation mints
// a run without clearing the claim.
func TestArtifactApprove_ClosesOnlyWhileTheTaskIsStillTheRunsToClose(t *testing.T) {
	for _, tc := range []struct {
		name     string
		moveOn   func(t *testing.T, srv *Server, taskID string)
		wantDone bool
	}{
		{
			name:     "still_the_bots_and_still_its_newest_run",
			moveOn:   func(*testing.T, *Server, string) {},
			wantDone: true,
		},
		{
			name: "a_human_took_the_task_over",
			moveOn: func(t *testing.T, srv *Server, taskID string) {
				execSQL(t, srv.db, `UPDATE tasks SET claimed_by_agent_id = NULL, claimed_by_user_id = ? WHERE id = ?`,
					runmode.LocalDefaultUserID, taskID)
			},
		},
		{
			name: "a_later_delegation_superseded_the_run",
			moveOn: func(t *testing.T, srv *Server, taskID string) {
				seedBlueprintRunSQLite(t, srv.db, taskID)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			srv := newTestServer(t)
			mux := newAppAPIMux()
			mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "node_id": "PR_node", "state": "open", "draft": true, "title": "Proposed title", "body": "Proposed body"})
			})
			mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"pullRequest": map[string]any{"isDraft": false}}}})
			})
			stub := httptest.NewServer(mux)
			t.Cleanup(stub.Close)
			seedApp(t, srv, stub, acmeInstall())

			artID, _, taskID := seedDraftPRArtifactWithConversation(t, srv, "aptc", "acme", "api", 42)
			// The run that opened the PR finished clean — without that there is
			// nothing to close on and every arm would agree for the wrong reason.
			execSQL(t, srv.db, `UPDATE blueprint_runs SET status = 'completed' WHERE task_id = ?`, taskID)
			tc.moveOn(t, srv, taskID)

			rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			// The approval itself lands whatever the task did — only the
			// closure is guarded.
			if got := getArtifact(t, srv, artID).State; got != domain.ArtifactStatePROpen {
				t.Fatalf("artifact state = %q, want open", got)
			}
			var taskStatus string
			if err := srv.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&taskStatus); err != nil {
				t.Fatalf("read task: %v", err)
			}
			if done := taskStatus == "done"; done != tc.wantDone {
				t.Errorf("task.status = %q (done = %v), want done = %v", taskStatus, done, tc.wantDone)
			}
		})
	}
}

// TestArtifactAbandon_ClosesDraftPR pins the "Return to queue" path: requeueing
// a task whose conversation opened a draft PR closes that PR on GitHub (ClosePR
// → state closed) and flips its artifact to closed. The pushed branch is
// untouched.
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
// gesture (Return-to-queue): a task whose conversation holds MULTIPLE
// unresolved artifacts — a draft PR and a pending review — has them ALL
// resolved. The draft PR is closed on GitHub; the review is flipped to
// dismissed with NO GitHub call (it was staged TF-side, TFAC-494). Branches are
// kept.
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

	// seedClaimedPRApprovalFixture hangs a draft PR (#7) off conversation
	// r_ab; add a finalized review draft on the same conversation so the
	// task holds two unresolved artifacts of different kinds.
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
// conversation's lifecycle. The pushed branch is untouched (we send only state=closed).
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
	dismissed := getArtifact(t, srv, artID)
	if dismissed.State != domain.ArtifactStatePRClosed {
		t.Errorf("artifact state = %q, want closed", dismissed.State)
	}
	if d, _ := domain.ParsePRArtifactDetails(dismissed.DetailsJSON); d.Resolution != domain.PRResolutionDismissed {
		t.Errorf("resolution = %q, want dismissed", d.Resolution)
	}
	// The conversation lifecycle is untouched — dismiss is a decoupled sidecar.
	var convStatus string
	if err := srv.db.QueryRow(`SELECT status FROM conversations WHERE id=?`, conversationID).Scan(&convStatus); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if convStatus != "completed" {
		t.Errorf("conversation status = %q, want completed (dismiss must not flip conversation lifecycle)", convStatus)
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

// TestArtifactDismiss_TaskLessConversationClosesNoTask pins the non-blueprint branch
// against the actual model: a future user-triggered conversation
// (origin='interactive') is neither blueprint- nor task-linked — it carries a
// NULL blueprint_run_id AND a NULL task_id (both allowed once origin <>
// 'blueprint'). Resolving an artifact on such a conversation must be a clean
// no-op for task closure: GetRunForConversation routes to the standalone
// branch, the conversation has no task, and the taskID == "" guard short-
// circuits — no task is closed, no error. (No such conversation exists in
// production today; this pins the forward-compat path so it can't regress into
// closing a phantom task.)
func TestArtifactDismiss_TaskLessConversationClosesNoTask(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "closed"})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	// A task-less, blueprint-less interactive conversation: origin='interactive'
	// with NULL task_id and NULL blueprint_run_id (tolerated by
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
	// cleanly — a task-less conversation has no task to close.
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
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "open", "draft": true, "title": "Live title", "body": "Body edited on GitHub"})
	})
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&patchBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID := seedDraftPRArtifact(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodPatch, "/api/artifacts/"+artID+"/pr", map[string]any{"title": "New title"})
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
	rec := doJSON(t, srv, http.MethodPatch, "/api/artifacts/"+artID+"/pr", map[string]any{"title": "New title"})
	if rec.Code < 400 {
		t.Fatalf("patch = %d, want non-2xx when the live baseline read fails", rec.Code)
	}
	if patched {
		t.Error("UpdatePR must not run when the baseline read failed — it would clobber the untouched field")
	}
}

// TestArtifactApprove_MalformedDetails_StillPromotes pins that an unparseable
// details_json costs only what the row could not carry forward: approve still
// marks the PR ready and flips the artifact, and still writes nothing to the
// PR's title or body.
func TestArtifactApprove_MalformedDetails_StillPromotes(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var marked, patched bool
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "node_id": "PR_node", "state": "open", "draft": true, "title": "Live title", "body": "Live body"})
	})
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		patched = true
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	})
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		marked = true
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
	if !marked {
		t.Error("approve must MarkPRReady even when details are unparseable")
	}
	if patched {
		t.Error("approve must leave the PR's title and body alone")
	}
	if got := getArtifact(t, srv, artID).State; got != domain.ArtifactStatePROpen {
		t.Errorf("artifact state = %q, want open", got)
	}
}

// TestArtifactApprove_BodyByteIdentical pins that approval never rewrites the
// PR body: the disclosure footer the body already carries from creation is the
// only one it will ever carry, and a human's concurrent edit on GitHub cannot
// be clobbered by an approval that reads-then-writes. The stub fails the test
// on any PATCH rather than merely recording one.
func TestArtifactApprove_BodyByteIdentical(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	liveBody := "Real body\n\n---\n*This PR was partially generated by AI using [Triage Factory](https://github.com/sky-ai-eng/triage-factory).*"
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "node_id": "PR_node", "state": "open", "draft": true, "title": "T", "body": liveBody})
	})
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		t.Error("approve must not PATCH the pull request")
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
	// The snapshot the flip records is the live body verbatim, footer and all.
	d, err := domain.ParsePRArtifactDetails(getArtifact(t, srv, artID).DetailsJSON)
	if err != nil {
		t.Fatalf("parse details: %v", err)
	}
	if d.Snapshot.Body != liveBody {
		t.Errorf("recorded snapshot body = %q, want the live body verbatim %q", d.Snapshot.Body, liveBody)
	}
}

// TestArtifactApprove_NonDraft_409 pins the state guard: approving an artifact
// that's no longer a draft (already open or closed) is a conflict and performs
// no GitHub mutation — a stale double-click can't record a second approval.
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

// seedDraftPRArtifactWithConversation mints a completed (terminal) conversation
// chain (entity → … → conversation) and a draft pull_request artifact hung off
// it, returning all three ids.
func seedDraftPRArtifactWithConversation(t *testing.T, s *Server, suffix, owner, repo string, number int) (artifactID, conversationID, taskID string) {
	t.Helper()
	conversationID = seedSteerConversation(t, s.db, suffix, "completed")
	taskID = fixtureUUID("t_" + suffix)
	// The fixture's premise is a delegated run that opened a draft PR, so its
	// task is bot-claimed — the state the terminal-on-last closing hooks
	// require before a resolution may close anything.
	execSQL(t, s.db, `UPDATE tasks SET claimed_by_agent_id = ? WHERE id = ?`, runmode.LocalDefaultAgentID, taskID)
	// The conversation's own memory, as its completion gate would have filed it
	// — what assertAgentMemoryUntouched checks the approval paths leave alone.
	if _, err := sqlitestore.New(s.db).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, conversationID, "", "agent self-report", domain.MemorySourceAgent); err != nil {
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

// assertAgentMemoryUntouched checks that the conversation still holds exactly
// the memory its completion gate filed. Every artifact verb below runs it: a
// conversation_memory row is the agent's own account of what it tried, and no
// human verdict about an artifact is written into it — the artifact row carries
// its own state, and a resolution reaches the agent as artifact feedback.
func assertAgentMemoryUntouched(t *testing.T, s *Server, conversationID string) {
	t.Helper()
	mem, err := sqlitestore.New(s.db).TaskMemory.GetForConversationSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("GetForConversationSystem: %v", err)
	}
	if mem == nil {
		t.Fatalf("no conversation_memory row for %s", conversationID)
	}
	if mem.Source != domain.MemorySourceAgent || mem.Content != "agent self-report" {
		t.Errorf("memory = (Source=%q, Content=%q), want the agent's own row untouched", mem.Source, mem.Content)
	}
}

// seedClaimedPRApprovalFixture builds a claimed task (the shape /requeue
// expects) whose completed conversation opened a draft PR. Returns (taskID,
// conversationID, artifactID).
func seedClaimedPRApprovalFixture(t *testing.T, s *Server, owner, repo string, number int) (taskID, conversationID, artifactID string) {
	t.Helper()
	const eventType = "github:pr:ci_check_passed"
	execSQL(t, s.db, `INSERT INTO entities (id, source, source_id, kind, state) VALUES ('e_ab', 'github', ?, 'pr', 'active')`, fmt.Sprintf("%s/%s#%d", owner, repo, number))
	execSQL(t, s.db, `INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_ab', 'e_ab', ?, '')`, eventType)
	execSQL(t, s.db, `INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_ab', 'P', 'b', ?, ?)`, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
	execSQL(t, s.db, `INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_agent_id) VALUES ('00000000-0000-4000-8000-000000000023', 'e_ab', ?, 'ev_ab', 'queued', ?)`, eventType, runmode.LocalDefaultAgentID)
	brID := seedBlueprintRunSQLite(t, s.db, "00000000-0000-4000-8000-000000000023")
	execSQL(t, s.db, `INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, blueprint_step_index) VALUES ('r_ab', '00000000-0000-4000-8000-000000000023', 'p_ab', 'completed', 'manual', ?, 0)`, brID)
	if _, err := sqlitestore.New(s.db).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, "r_ab", "", "agent self-report", domain.MemorySourceAgent); err != nil {
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

// rejectStub is the GitHub stub a resolve verb exercises: the live PR GET
// (answering `live`, the REST shape), the refs DELETE (answering deleteStatus
// with deleteBody), the PR-close PATCH, and a GraphQL arm serving the same PR
// as `liveNode` to the reconciler, so a verb that finds the row stale can be
// seen to reconcile it. It records what the handler sent so tests can pin the
// order-of-operations contract.
type rejectStub struct {
	deletePaths []string
	closeState  string
	marked      bool
}

// livePR is a REST PR object as GetPRBasic reads it. draftPR is the shape the
// verbs expect to find; closedPR / mergedPR / readyPR are the out-of-band
// resolutions they must refuse to act on.
func livePR(state string, draft, merged bool) map[string]any {
	return map[string]any{"number": 42, "node_id": "PR_node", "state": state, "draft": draft, "merged": merged, "title": "Proposed title", "body": "Proposed body"}
}

// liveNode is the same PR as the reconciler's GraphQL nodes query sees it.
func liveNode(state string, draft, merged bool) string {
	return fmt.Sprintf(`{"id":"PR_node","number":42,"repository":{"nameWithOwner":"acme/api"},"url":"https://example.test/acme/api/pull/42","state":%q,"isDraft":%t,"merged":%t,"latestReviews":{"nodes":[]}}`, state, draft, merged)
}

func newRejectStub(t *testing.T, srv *Server, live map[string]any, node string, deleteStatus int, deleteBody string) *rejectStub {
	t.Helper()
	rs := &rejectStub{}
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(live)
	})
	mux.HandleFunc("DELETE /api/v3/repos/{owner}/{repo}/git/refs/heads/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		rs.deletePaths = append(rs.deletePaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(deleteStatus)
		_, _ = w.Write([]byte(deleteBody))
	})
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rs.closeState, _ = body["state"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "closed"})
	})
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "markPullRequestReadyForReview"):
			rs.marked = true
			_, _ = w.Write([]byte(`{"data":{"markPullRequestReadyForReview":{"pullRequest":{"isDraft":false}}}}`))
		case strings.Contains(req.Query, "nodes(ids:"):
			fmt.Fprintf(w, `{"data":{"nodes":[%s]}}`, node)
		default:
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())
	stores := sqlitestore.New(srv.db)
	srv.SetReconciler(reconcile.NewReconciler(srv.ghResolver, stores.Artifacts, nil))
	return rs
}

// seedBranchArtifact records the pushed-branch artifact the run's push would
// have captured for the draft PR's head, so a reject has a sibling to retire.
func seedBranchArtifact(t *testing.T, s *Server, conversationID, repoPath, ref string) string {
	t.Helper()
	a, ok := domain.NewBranchArtifact(repoPath, ref, "abc123", true)
	if !ok {
		t.Fatalf("NewBranchArtifact(%q, %q) refused", repoPath, ref)
	}
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(s.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a)
	if err != nil {
		t.Fatalf("seed branch artifact: %v", err)
	}
	return stored.ID
}

// TestArtifactReject_PR pins the whole rejection: the head branch is deleted
// from the upstream, the draft PR closed, the PR artifact flipped to closed
// carrying branch_deleted, the run's branch artifact retired, both writes
// audited — and, as with dismiss, the conversation lifecycle untouched and its
// memory left alone.
func TestArtifactReject_PR(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	rs := newRejectStub(t, srv, livePR("open", true, false), liveNode("OPEN", true, false), http.StatusNoContent, "")

	artID, conversationID, _ := seedDraftPRArtifactWithConversation(t, srv, "rej", "acme", "api", 42)
	branchID := seedBranchArtifact(t, srv, conversationID, "acme/api", "refs/heads/feature/x")

	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/reject", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		State  string `json:"state"`
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.State != domain.ArtifactStatePRClosed || resp.Branch != "feature/x" {
		t.Errorf("response = %+v, want closed / feature/x", resp)
	}

	if want := []string{"/api/v3/repos/acme/api/git/refs/heads/feature/x"}; !equalStrings(rs.deletePaths, want) {
		t.Errorf("DELETE paths = %v, want %v", rs.deletePaths, want)
	}
	if rs.closeState != "closed" {
		t.Errorf("ClosePR sent state=%q, want closed", rs.closeState)
	}

	pr := getArtifact(t, srv, artID)
	if pr.State != domain.ArtifactStatePRClosed {
		t.Errorf("PR artifact state = %q, want closed", pr.State)
	}
	details, err := domain.ParsePRArtifactDetails(pr.DetailsJSON)
	if err != nil || details.Resolution != domain.PRResolutionRejected {
		t.Errorf("PR details = %+v (err %v), want resolution=rejected", details, err)
	}
	if note := domain.ArtifactResolutionNote(*pr); !strings.Contains(note, "rejected") || !strings.Contains(note, "feature/x") {
		t.Errorf("agent note = %q, want the rejection naming the branch", note)
	}
	if details.Proposed.Title != "Proposed title" {
		t.Errorf("proposed snapshot must survive the flip; got %q", details.Proposed.Title)
	}
	if got := getArtifact(t, srv, branchID).State; got != domain.ArtifactStateBranchDeleted {
		t.Errorf("branch artifact state = %q, want deleted", got)
	}

	acts, _, err := sqlitestore.New(srv.db).ExternalActions.ListByOrgSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionBranchDeleted})
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(acts) != 1 || acts[0].Target != "acme/api" || acts[0].ExternalID != "refs/heads/feature/x" || acts[0].ConversationID != conversationID {
		t.Errorf("branch_deleted audit rows = %+v, want one on acme/api refs/heads/feature/x for the drafting conversation", acts)
	}
	closedActs, _, err := sqlitestore.New(srv.db).ExternalActions.ListByOrgSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionPRClosed})
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(closedActs) != 1 {
		t.Errorf("pr_closed audit rows = %d, want 1", len(closedActs))
	}

	assertAgentMemoryUntouched(t, srv, conversationID)

	var convStatus string
	if err := srv.db.QueryRow(`SELECT status FROM conversations WHERE id=?`, conversationID).Scan(&convStatus); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if convStatus != "completed" {
		t.Errorf("conversation status = %q, want completed (reject must not flip conversation lifecycle)", convStatus)
	}
}

// TestArtifactReject_BranchAlreadyGone_ReconcilesAnd409 pins the race where
// the branch was deleted out-of-band between the overlay loading and the
// click, GitHub not yet showing the auto-close: the delete finds no ref, and
// rather than recording a rejection over a deletion it never made, the verb
// reconciles the row against GitHub and answers 409 with nothing of its own
// written — no close, no rejection stamp, no branch_deleted audit row.
func TestArtifactReject_BranchAlreadyGone_ReconcilesAnd409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	// REST still says draft (the race window); GraphQL — what the reconciler
	// reads — already says closed.
	rs := newRejectStub(t, srv, livePR("open", true, false), liveNode("CLOSED", true, false), http.StatusUnprocessableEntity, `{"message":"Reference does not exist"}`)

	artID, conversationID, _ := seedDraftPRArtifactWithConversation(t, srv, "gone", "acme", "api", 42)
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/reject", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reject = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "feature/x") || !strings.Contains(rec.Body.String(), httpx.ReasonAlreadyTerminal) {
		t.Errorf("error body %s should name the branch under ALREADY_TERMINAL", rec.Body.String())
	}
	if rs.closeState != "" {
		t.Errorf("ClosePR must not run when the branch was not deleted here; sent state=%q", rs.closeState)
	}
	pr := getArtifact(t, srv, artID)
	if pr.State != domain.ArtifactStatePRClosed {
		t.Errorf("artifact state = %q, want closed (reconciled from GitHub)", pr.State)
	}
	details, err := domain.ParsePRArtifactDetails(pr.DetailsJSON)
	if err != nil || details.Resolution != domain.PRResolutionGitHub {
		t.Errorf("PR details = %+v (err %v), want resolution=github — the reconciler resolved it, not the verb", details, err)
	}
	if note := domain.ArtifactResolutionNote(*pr); !strings.Contains(note, "on GitHub") || strings.Contains(note, "rejected") || strings.Contains(note, "dismissed") {
		t.Errorf("agent note = %q, must report an out-of-band close, not a human verdict", note)
	}
	for _, action := range []string{domain.ActionBranchDeleted, domain.ActionPRClosed} {
		acts, _, err := sqlitestore.New(srv.db).ExternalActions.ListByOrgSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: action})
		if err != nil {
			t.Fatalf("list actions: %v", err)
		}
		if len(acts) != 0 {
			t.Errorf("no %s audit row may claim a write this verb did not make; got %d", action, len(acts))
		}
	}
	assertAgentMemoryUntouched(t, srv, conversationID)
}

// TestArtifactReject_ResolvedOnGitHub409 pins the live-state guard: a draft
// merged (or closed, or marked ready) on GitHub since the overlay loaded is
// not this verb's to touch — least of all its branch — so no DELETE is sent,
// the row is reconciled, and the answer is 409.
func TestArtifactReject_ResolvedOnGitHub409(t *testing.T) {
	cases := []struct {
		name     string
		live     map[string]any
		node     string
		wantWord string
	}{
		{"merged", livePR("closed", false, true), liveNode("MERGED", false, true), "merged"},
		{"closed", livePR("closed", true, false), liveNode("CLOSED", true, false), "closed"},
		{"marked ready", livePR("open", false, false), liveNode("OPEN", false, false), "ready for review"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			srv := newTestServer(t)
			rs := newRejectStub(t, srv, tc.live, tc.node, http.StatusNoContent, "")
			artID, _, _ := seedDraftPRArtifactWithConversation(t, srv, "oob", "acme", "api", 42)
			rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/reject", nil)
			if rec.Code != http.StatusConflict {
				t.Fatalf("reject = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantWord) {
				t.Errorf("error body %s should say the PR was %s", rec.Body.String(), tc.wantWord)
			}
			if len(rs.deletePaths) != 0 {
				t.Errorf("no DELETE may be sent for a PR resolved on GitHub; got %v", rs.deletePaths)
			}
			pr := getArtifact(t, srv, artID)
			if pr.State == domain.ArtifactStatePRDraft {
				t.Errorf("artifact still draft; the verb should have reconciled it")
			}
			if d, _ := domain.ParsePRArtifactDetails(pr.DetailsJSON); d.Resolution != domain.PRResolutionGitHub {
				t.Errorf("resolution = %q, want github", d.Resolution)
			}
		})
	}
}

// TestArtifactApprove_ResolvedOnGitHub409 is the same guard on the other verb:
// "Open PR" on a draft someone closed on GitHub is a 409 and a reconcile, not
// a ready-for-review mutation GitHub would reject into a 502 while the row sat
// draft until the next pass.
func TestArtifactApprove_ResolvedOnGitHub409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	rs := newRejectStub(t, srv, livePR("closed", true, false), liveNode("CLOSED", true, false), http.StatusNoContent, "")
	artID, _, _ := seedDraftPRArtifactWithConversation(t, srv, "aoob", "acme", "api", 42)
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if rs.marked {
		t.Error("MarkPRReady must not run on a PR already closed on GitHub")
	}
	pr := getArtifact(t, srv, artID)
	if pr.State != domain.ArtifactStatePRClosed {
		t.Errorf("artifact state = %q, want closed (reconciled)", pr.State)
	}
	acts, _, err := sqlitestore.New(srv.db).ExternalActions.ListByOrgSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionPRMarkedReady})
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(acts) != 0 {
		t.Errorf("no pr_marked_ready audit row for an approval that did not happen; got %d", len(acts))
	}
}

// TestArtifactReject_DeleteRefused_NothingChanges pins the pessimistic order:
// when GitHub refuses the branch delete the handler answers 502, sends no
// close, and leaves the artifact a draft — the user can retry, dismiss, or
// open the PR conventionally.
func TestArtifactReject_DeleteRefused_NothingChanges(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	rs := newRejectStub(t, srv, livePR("open", true, false), liveNode("OPEN", true, false), http.StatusForbidden, `{"message":"Resource not accessible by integration"}`)

	artID, _, _ := seedDraftPRArtifactWithConversation(t, srv, "refused", "acme", "api", 42)
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/reject", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("reject = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "feature/x") {
		t.Errorf("error body %s should name the branch it could not delete", rec.Body.String())
	}
	if rs.closeState != "" {
		t.Errorf("ClosePR must not run after a refused delete; sent state=%q", rs.closeState)
	}
	if got := getArtifact(t, srv, artID).State; got != domain.ArtifactStatePRDraft {
		t.Errorf("artifact state = %q, want draft (nothing changed)", got)
	}
	acts, _, err := sqlitestore.New(srv.db).ExternalActions.ListByOrgSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionBranchDeleted})
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(acts) != 0 {
		t.Errorf("no branch_deleted audit row may exist after a refused delete; got %d", len(acts))
	}
}

// TestArtifactReject_TerminalArtifact409 pins the guard, and the race from the
// ticket in both directions: a reject after a resolution is a clean 409, and an
// approve racing a completed reject is the same clean 409 — never a panic, never
// a second GitHub write.
func TestArtifactReject_TerminalArtifact409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	newRejectStub(t, srv, livePR("open", true, false), liveNode("OPEN", true, false), http.StatusNoContent, "")

	artID, _, _ := seedDraftPRArtifactWithConversation(t, srv, "rej409", "acme", "api", 42)
	if rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/reject", nil); rec.Code != http.StatusOK {
		t.Fatalf("first reject = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/reject", nil); rec.Code != http.StatusConflict {
		t.Errorf("second reject = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil); rec.Code != http.StatusConflict {
		t.Errorf("approve after reject = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestArtifactReject_UnknownHeadBranch409 pins the data guard: a PR row that
// never recorded its head branch cannot carry a rejection (there is nothing to
// delete), and the answer is a 409 pointing at dismiss — not a delete of some
// guessed ref.
func TestArtifactReject_UnknownHeadBranch409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	rs := newRejectStub(t, srv, livePR("open", true, false), liveNode("OPEN", true, false), http.StatusNoContent, "")

	artID, _, _ := seedDraftPRArtifactWithConversation(t, srv, "nohead", "acme", "api", 42)
	execSQL(t, srv.db, `UPDATE artifacts SET details_json = ? WHERE id = ?`,
		domain.MarshalPRArtifactDetails(domain.PRArtifactDetails{Base: "main"}), artID)
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/reject", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reject = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if len(rs.deletePaths) != 0 {
		t.Errorf("no DELETE may be sent for an unknown head; got %v", rs.deletePaths)
	}
	if got := getArtifact(t, srv, artID).State; got != domain.ArtifactStatePRDraft {
		t.Errorf("artifact state = %q, want draft", got)
	}
}
