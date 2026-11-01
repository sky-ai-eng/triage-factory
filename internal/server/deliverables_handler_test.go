package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// getDeliverable re-reads a deliverable by id straight from the store (no RLS in
// SQLite), for asserting post-conditions.
func getDeliverable(t *testing.T, s *Server, id string) *domain.Deliverable {
	t.Helper()
	a, err := sqlitestore.New(s.db).Deliverables.Get(context.Background(), runmode.LocalDefaultOrgID, id)
	if err != nil {
		t.Fatalf("re-read deliverable: %v", err)
	}
	if a == nil {
		t.Fatalf("deliverable %s not found", id)
	}
	return a
}

// TestDeliverableUpdate_GitHubFailure_Pessimistic pins the pessimistic contract: a
// GitHub rejection (422) returns non-2xx and the snapshot is NOT moved — no
// silent success over a write GitHub refused.
func TestDeliverableUpdate_GitHubFailure_Pessimistic(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Validation Failed","errors":[{"message":"title is too long"}]}`, http.StatusUnprocessableEntity)
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID := seedDraftPRDeliverable(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodPatch, "/api/deliverables/"+artID+"/pr", map[string]any{"title": "New", "body": "New body"})
	if rec.Code < 400 {
		t.Fatalf("patch = %d, want non-2xx on GitHub failure; body=%s", rec.Code, rec.Body.String())
	}
	// Snapshot must be untouched (still the agent's draft).
	d, _ := domain.ParsePRDeliverableDetails(getDeliverable(t, srv, artID).DetailsJSON)
	if d.Snapshot.Title != "Add thing" || d.Snapshot.Body != "Body." {
		t.Errorf("snapshot moved on a failed UpdatePR: %+v", d.Snapshot)
	}
}

// TestDeliverableWrites_KindScoped: a write shaped for one deliverable kind is
// refused on the other kind's route, with no GitHub call, no audit row and no
// state change. Every stub answers 500 and records the hit.
func TestDeliverableWrites_KindScoped(t *testing.T) {
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

	prID := seedDraftPRDeliverable(t, srv, "acme", "api")
	reviewID, _, _ := seedReviewDeliverableWithConversation(t, srv, "kindscope", "acme", "api", 7, "COMMENT")

	for name, tc := range map[string]struct {
		method string
		path   string
		body   any
		want   int
	}{
		"review body on a PR deliverable":    {http.MethodPatch, "/api/deliverables/" + prID + "/review", map[string]any{"body": "lgtm"}, http.StatusConflict},
		"PR body on a review deliverable":    {http.MethodPatch, "/api/deliverables/" + reviewID + "/pr", map[string]any{"title": "New"}, http.StatusConflict},
		"comment edit on a PR deliverable":   {http.MethodPatch, "/api/deliverables/" + prID + "/comments/c_1", map[string]any{"body": "x"}, http.StatusConflict},
		"comment delete on a PR deliverable": {http.MethodDelete, "/api/deliverables/" + prID + "/comments/c_1", nil, http.StatusConflict},
		"refresh on a PR deliverable":        {http.MethodPost, "/api/deliverables/" + prID + "/review/refresh", nil, http.StatusConflict},
		"dismiss on a review deliverable":    {http.MethodPost, "/api/deliverables/" + reviewID + "/dismiss", nil, http.StatusConflict},
		// A zero-field PR patch names nothing to send GitHub.
		"zero-field PR patch": {http.MethodPatch, "/api/deliverables/" + prID + "/pr", map[string]any{}, http.StatusBadRequest},
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
	// Both deliverables are untouched.
	if got := getDeliverable(t, srv, prID).State; got != domain.DeliverableStatePRDraft {
		t.Errorf("PR deliverable state = %q, want draft (unchanged)", got)
	}
	if got := getDeliverable(t, srv, reviewID).State; got != domain.DeliverableStateReviewPending {
		t.Errorf("review deliverable state = %q, want pending (unchanged)", got)
	}
}

// TestDeliverableApprove_LedgerNamesWhoPerformedEachAct: the agent's writes and a
// person's approval share one conversation's ledger and, on a manual run, one
// actor, so performed_by is the only column that tells the rows apart.
func TestDeliverableApprove_LedgerNamesWhoPerformedEachAct(t *testing.T) {
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

	artID, conversationID, _ := seedDraftPRDeliverableWithConversation(t, srv, "performer", "acme", "api", 42)
	stores := sqlitestore.New(srv.db)
	agenthost.RecordExternalWrite(context.Background(), stores, agenthost.ConversationInfo{
		OrgID:          runmode.LocalDefaultOrgID,
		UserID:         runmode.LocalDefaultUserID,
		ConversationID: conversationID,
		TeamID:         runmode.LocalDefaultTeamID,
	}, nil, &domain.ExternalAction{
		Provider:   domain.ProviderGitHub,
		Action:     domain.ActionCommentPosted,
		Target:     "acme/api#42",
		Credential: domain.CredentialGitHubApp,
	})

	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	actions, _, err := stores.ExternalActions.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID, domain.ExternalActionListOpts{})
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	want := map[string]string{
		domain.ActionCommentPosted: domain.PerformedByAgent,
		domain.ActionPRMarkedReady: domain.PerformedByPerson,
	}
	if len(actions) != len(want) {
		t.Fatalf("conversation's actions = %+v, want one %s and one %s", actions, domain.ActionCommentPosted, domain.ActionPRMarkedReady)
	}
	for _, a := range actions {
		if a.PerformedBy != want[a.Action] {
			t.Errorf("%s row performed_by = %q, want %q", a.Action, a.PerformedBy, want[a.Action])
		}
		if a.ActorUserID != runmode.LocalDefaultUserID {
			t.Errorf("%s row actor = %q, want the local user on both rows", a.Action, a.ActorUserID)
		}
	}
}

// TestDeliverableApprove_ClosesOnlyWhileTheTaskIsStillTheRunsToClose: approving
// the last unresolved deliverable closes a task still bot-claimed by its newest
// run, and not one a person took over or a later delegation superseded.
func TestDeliverableApprove_ClosesOnlyWhileTheTaskIsStillTheRunsToClose(t *testing.T) {
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
				execSQL(t, srv.db, `UPDATE tasks SET claimed_by_agent_id = NULL, claimed_by_user_id = ?, status = 'in_progress' WHERE id = ?`,
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

			artID, _, taskID := seedDraftPRDeliverableWithConversation(t, srv, "aptc", "acme", "api", 42)
			// The run that opened the PR finished clean — without that there is
			// nothing to close on and every arm would agree for the wrong reason.
			execSQL(t, srv.db, `UPDATE blueprint_runs SET status = 'completed' WHERE task_id = ?`, taskID)
			tc.moveOn(t, srv, taskID)

			rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			// The approval itself lands whatever the task did — only the
			// closure is guarded.
			if got := getDeliverable(t, srv, artID).State; got != domain.DeliverableStatePROpen {
				t.Fatalf("deliverable state = %q, want open", got)
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

// TestDeliverableAbandon_ClosesDraftPR: dismissing a task resolves its
// conversation's deliverables: the draft PR is closed on GitHub, and the staged
// review, which has no GitHub object, is dismissed with no GitHub call.
func TestDeliverableAbandon_ClosesDraftPR(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var closeState string
	var reviewTouchedGitHub bool
	mux := newAppAPIMux()
	mux.HandleFunc("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		closeState, _ = body["state"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
	})
	for _, pattern := range []string{"POST /api/graphql", "DELETE /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews/{rid}"} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			reviewTouchedGitHub = true
			_, _ = w.Write([]byte(`{"data":{}}`))
		})
	}
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	taskID, conversationID, artID := seedClaimedPRApprovalFixture(t, srv, "acme", "api", 7)
	rv := domain.NewReviewDeliverable("acme/api", 7, "PR_acme_api_7", "headsha7", conversationID)
	rv.ConversationID = conversationID
	rv.OrgID = runmode.LocalDefaultOrgID
	rv.TeamID = runmode.LocalDefaultTeamID
	rd, _ := domain.ParseReviewDeliverableDetails(rv.DetailsJSON)
	rd.ReviewBody = "body"
	rd.ReviewEvent = "COMMENT"
	rv.DetailsJSON = domain.MarshalReviewDeliverableDetails(rd)
	review, err := sqlitestore.New(srv.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(srv.db).Orgs, runmode.LocalDefaultOrgID, rv))
	if err != nil {
		t.Fatalf("seed review deliverable: %v", err)
	}

	rec := doJSON(t, srv, http.MethodPatch, "/api/tasks/"+taskID,
		map[string]any{"status": "dismissed", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if closeState != "closed" {
		t.Errorf("ClosePR sent state=%q, want closed", closeState)
	}
	if reviewTouchedGitHub {
		t.Error("dismissing the task made a GitHub call for the staged review")
	}
	if got := getDeliverable(t, srv, artID).State; got != domain.DeliverableStatePRClosed {
		t.Errorf("deliverable state = %q, want closed", got)
	}
	if got := getDeliverable(t, srv, review.ID).State; got != domain.DeliverableStateReviewDismissed {
		t.Errorf("review deliverable state = %q, want dismissed", got)
	}
}

// TestDeliverableDismiss_TerminalDeliverable409 pins the guard: dismissing a deliverable
// that's already resolved (a non-draft PR) is a 409 — there's nothing left to
// resolve. A second dismiss on an already-closed PR exercises the path.
func TestDeliverableDismiss_TerminalDeliverable409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	artID, _, _ := seedDraftPRDeliverableWithConversation(t, srv, "dis409", "acme", "api", 42)
	// Pre-resolve the deliverable so the state guard fires before any GitHub call.
	execSQL(t, srv.db, `UPDATE deliverables SET state = ? WHERE id = ?`, domain.DeliverableStatePRClosed, artID)
	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/dismiss", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("dismiss of terminal deliverable = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestDeliverableDismiss_ClosesTaskOnlyOnCompletedBlueprint: dismissing a draft PR
// closes it on GitHub and leaves the conversation alone; as the task's last
// unresolved deliverable it closes the task only if the blueprint run completed.
func TestDeliverableDismiss_ClosesTaskOnlyOnCompletedBlueprint(t *testing.T) {
	for _, tc := range []struct {
		runStatus domain.BlueprintRunStatus
		wantDone  bool
	}{
		{domain.BlueprintRunStatusCompleted, true},
		{domain.BlueprintRunStatusAborted, false},
		{domain.BlueprintRunStatusFailed, false},
		{domain.BlueprintRunStatusCancelled, false},
		{domain.BlueprintRunStatusRunning, false},
	} {
		t.Run(string(tc.runStatus), func(t *testing.T) {
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

			artID, conversationID, taskID := seedDraftPRDeliverableWithConversation(t, srv, "dis-"+string(tc.runStatus), "acme", "api", 42)
			execSQL(t, srv.db, `UPDATE blueprint_runs SET status = ? WHERE task_id = ?`, string(tc.runStatus), taskID)

			rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/dismiss", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("dismiss = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if closeState != "closed" {
				t.Errorf("ClosePR sent state=%q, want closed", closeState)
			}
			dismissed := getDeliverable(t, srv, artID)
			if dismissed.State != domain.DeliverableStatePRClosed {
				t.Errorf("deliverable state = %q, want closed", dismissed.State)
			}
			if d, _ := domain.ParsePRDeliverableDetails(dismissed.DetailsJSON); d.Resolution != domain.PRResolutionDismissed {
				t.Errorf("resolution = %q, want dismissed", d.Resolution)
			}
			if !storedConcluded(t, srv.db, conversationID) {
				t.Error("conversation no longer concluded (dismiss must not flip conversation lifecycle)")
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

// TestDeliverableDismiss_TaskLessConversationClosesNoTask: a deliverable on an
// interactive conversation with no task and no blueprint run resolves, and the
// task-closure check finds nothing to close rather than failing.
func TestDeliverableDismiss_TaskLessConversationClosesNoTask(t *testing.T) {
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
	a := domain.NewPullRequestDeliverable("acme/api", 42, "PR_node", "feature/x", "main", "https://example.test/acme/api/pull/42", "Proposed title", "Proposed body", true)
	a.ConversationID = "r_int"
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(srv.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(srv.db).Orgs, runmode.LocalDefaultOrgID, a))
	if err != nil {
		t.Fatalf("seed draft PR deliverable: %v", err)
	}

	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+stored.ID+"/dismiss", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The deliverable resolved (the load-bearing effect), and the closure check no-oped
	// cleanly — a task-less conversation has no task to close.
	if got := getDeliverable(t, srv, stored.ID).State; got != domain.DeliverableStatePRClosed {
		t.Errorf("deliverable state = %q, want closed", got)
	}
}

// TestDeliverableUpdate_SingleField_UsesLiveBaseline: a PATCH that names only the
// title fills the body from the PR's live value, not the cached snapshot, so an
// edit made on GitHub is not reverted.
func TestDeliverableUpdate_SingleField_UsesLiveBaseline(t *testing.T) {
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

	artID := seedDraftPRDeliverable(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodPatch, "/api/deliverables/"+artID+"/pr", map[string]any{"title": "New title"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if patchBody["title"] != "New title" {
		t.Errorf("UpdatePR title = %v, want New title", patchBody["title"])
	}
	if patchBody["body"] != "Body edited on GitHub" {
		t.Errorf("UpdatePR body = %v, want the live body (no lost update); the stale snapshot was %q", patchBody["body"], "Body.")
	}
	// The deliverable's snapshot follows what GitHub now holds; the proposed
	// agent draft stays frozen.
	d, _ := domain.ParsePRDeliverableDetails(getDeliverable(t, srv, artID).DetailsJSON)
	if d.Snapshot.Title != "New title" || d.Snapshot.Body != "Body edited on GitHub" {
		t.Errorf("snapshot = %+v, want the edited title over the live body", d.Snapshot)
	}
	if d.Proposed.Title != "Add thing" || d.Proposed.Body != "Body." {
		t.Errorf("proposed = %+v, want frozen agent draft", d.Proposed)
	}
}

// TestDeliverableUpdate_PartialEdit_GetPRFailure_502 pins that when the live
// baseline read fails on a single-field PATCH, the handler fails rather than
// clobbering the untouched field from a stale snapshot.
func TestDeliverableUpdate_PartialEdit_GetPRFailure_502(t *testing.T) {
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

	artID := seedDraftPRDeliverable(t, srv, "acme", "api")
	rec := doJSON(t, srv, http.MethodPatch, "/api/deliverables/"+artID+"/pr", map[string]any{"title": "New title"})
	if rec.Code < 400 {
		t.Fatalf("patch = %d, want non-2xx when the live baseline read fails", rec.Code)
	}
	if patched {
		t.Error("UpdatePR must not run when the baseline read failed — it would clobber the untouched field")
	}
}

// TestDeliverableApprove_MalformedDetails_StillPromotes: an unparseable
// details_json still lets approve mark the PR ready and flip the deliverable,
// writing nothing to the PR's title or body.
func TestDeliverableApprove_MalformedDetails_StillPromotes(t *testing.T) {
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

	artID, _, _ := seedDraftPRDeliverableWithConversation(t, srv, "appmal", "acme", "api", 42)
	if _, err := srv.db.Exec(`UPDATE deliverables SET details_json='{not valid json' WHERE id=?`, artID); err != nil {
		t.Fatalf("corrupt details: %v", err)
	}
	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !marked {
		t.Error("approve must MarkPRReady even when details are unparseable")
	}
	if patched {
		t.Error("approve must leave the PR's title and body alone")
	}
	if got := getDeliverable(t, srv, artID).State; got != domain.DeliverableStatePROpen {
		t.Errorf("deliverable state = %q, want open", got)
	}
}

// TestDeliverableApprove_BodyByteIdentical: approval never rewrites the PR, so a
// concurrent edit on GitHub survives; the snapshot it records is the live body
// verbatim, disclosure footer included. The stub fails on any PATCH.
func TestDeliverableApprove_BodyByteIdentical(t *testing.T) {
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

	artID, conversationID, _ := seedDraftPRDeliverableWithConversation(t, srv, "appftr", "acme", "api", 42)
	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The snapshot the flip records is the live body verbatim, footer and all.
	d, err := domain.ParsePRDeliverableDetails(getDeliverable(t, srv, artID).DetailsJSON)
	if err != nil {
		t.Fatalf("parse details: %v", err)
	}
	if d.Snapshot.Body != liveBody {
		t.Errorf("recorded snapshot body = %q, want the live body verbatim %q", d.Snapshot.Body, liveBody)
	}
	// Approval is a sidecar: the conversation stays concluded, and a verdict
	// about a deliverable is written into no memory.
	if !storedConcluded(t, srv.db, conversationID) {
		t.Error("conversation no longer concluded")
	}
	assertAgentMemoryUntouched(t, srv, conversationID)
}

// seedDraftPRDeliverableWithConversation mints a completed (terminal) conversation
// chain (entity → … → conversation) and a draft pull_request deliverable hung off
// it, returning all three ids.
func seedDraftPRDeliverableWithConversation(t *testing.T, s *Server, suffix, owner, repo string, number int) (deliverableID, conversationID, taskID string) {
	t.Helper()
	conversationID = seedSteerConversation(t, s.db, suffix, dbtest.SeedConcluded)
	taskID = fixtureUUID("t_" + suffix)
	// The fixture's premise is a delegated run that opened a draft PR, so its
	// task is bot-claimed — the state the terminal-on-last closing hooks
	// require before a resolution may close anything.
	execSQL(t, s.db, `UPDATE tasks SET claimed_by_agent_id = ?, status = 'in_progress' WHERE id = ?`, runmode.LocalDefaultAgentID, taskID)
	// The conversation's own memory, as its completion gate would have filed it
	// — what assertAgentMemoryUntouched checks the approval paths leave alone.
	if _, err := sqlitestore.New(s.db).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, conversationID, "", "agent self-report", domain.MemorySourceAgent); err != nil {
		t.Fatalf("seed agent memory: %v", err)
	}
	a := domain.NewPullRequestDeliverable(owner+"/"+repo, number, fmt.Sprintf("PR_%s_%s_%d", owner, repo, number), "feature/x", "main",
		fmt.Sprintf("https://example.test/%s/%s/pull/%d", owner, repo, number), "Proposed title", "Proposed body", true)
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	a = linkPullRequestEntity(t, s, dbtest.InOrgScope(t, sqlitestore.New(s.db).Orgs, runmode.LocalDefaultOrgID, a))
	stored, err := sqlitestore.New(s.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a)
	if err != nil {
		t.Fatalf("seed draft PR deliverable: %v", err)
	}
	return stored.ID, conversationID, taskID
}

// linkPullRequestEntity links a pull request or review deliverable to its pull
// request's entity, creating it as the poller would, because the handlers
// resolve the pull request through that entity.
func linkPullRequestEntity(t *testing.T, s *Server, a domain.Deliverable) domain.Deliverable {
	t.Helper()
	nodeID, _ := domain.DeliverablePullRequestNodeID(a)
	e, _, err := sqlitestore.New(s.db).Entities.FindOrCreateSystem(context.Background(), runmode.LocalDefaultOrgID, "github", a.Scope, a.Target, nodeID, "pr", "Proposed title", a.URL)
	if err != nil {
		t.Fatalf("seed the pull request entity for %s: %v", a.Target, err)
	}
	a.EntityID = e.ID
	return a
}

// assertAgentMemoryUntouched checks that the conversation still holds exactly
// the memory its completion gate filed. Every deliverable verb below runs it: a
// conversation_memory row is the agent's own account of what it tried, and no
// human verdict about a deliverable is written into it — the deliverable row carries
// its own state, and a resolution reaches the agent as deliverable feedback.
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
// conversationID, deliverableID).
func seedClaimedPRApprovalFixture(t *testing.T, s *Server, owner, repo string, number int) (taskID, conversationID, deliverableID string) {
	t.Helper()
	const eventType = "github:pr:ci_check_passed"
	repoID := dbseed.GitHubRepository(t, s.db, runmode.LocalDefaultOrgID, "https://github.com", owner, repo)
	execSQL(t, s.db, `INSERT INTO entities (id, source, source_id, kind, state, scope, repository_id) VALUES ('e_ab', 'github', ?, 'pr', 'active', 'https://github.com', ?)`, fmt.Sprintf("%s/%s#%d", owner, repo, number), repoID)
	execSQL(t, s.db, `INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_ab', 'e_ab', ?, '')`, eventType)
	execSQL(t, s.db, `INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_ab', 'P', 'b', ?, ?)`, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
	execSQL(t, s.db, `INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_agent_id) VALUES ('00000000-0000-4000-8000-000000000023', 'e_ab', ?, 'ev_ab', 'in_progress', ?)`, eventType, runmode.LocalDefaultAgentID)
	brID := seedBlueprintRunSQLite(t, s.db, "00000000-0000-4000-8000-000000000023")
	execSQL(t, s.db, `INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, blueprint_step_index, completed_at) VALUES ('r_ab', '00000000-0000-4000-8000-000000000023', 'p_ab', 'open', 'manual', ?, 0, CURRENT_TIMESTAMP)`, brID)
	if _, err := sqlitestore.New(s.db).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, "r_ab", "", "agent self-report", domain.MemorySourceAgent); err != nil {
		t.Fatalf("seed agent memory: %v", err)
	}
	a := domain.NewPullRequestDeliverable(owner+"/"+repo, number, fmt.Sprintf("PR_%s_%s_%d", owner, repo, number), "feature/x", "main",
		fmt.Sprintf("https://example.test/%s/%s/pull/%d", owner, repo, number), "Proposed title", "Proposed body", true)
	a.ConversationID = "r_ab"
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(s.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(s.db).Orgs, runmode.LocalDefaultOrgID, a))
	if err != nil {
		t.Fatalf("seed draft PR deliverable: %v", err)
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
	srv.SetReconciler(reconcile.NewReconciler(srv.ghResolver, stores, reconcile.TrustRecords, nil))
	return rs
}

// TestDeliverableReject_PR: a rejection deletes the head branch, closes the draft
// PR, flips the PR deliverable to closed, audits both writes, and leaves the
// conversation and its memory alone.
func TestDeliverableReject_PR(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	rs := newRejectStub(t, srv, livePR("open", true, false), liveNode("OPEN", true, false), http.StatusNoContent, "")

	artID, conversationID, _ := seedDraftPRDeliverableWithConversation(t, srv, "rej", "acme", "api", 42)

	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/reject", nil)
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
	if resp.State != domain.DeliverableStatePRClosed || resp.Branch != "feature/x" {
		t.Errorf("response = %+v, want closed / feature/x", resp)
	}

	if want := []string{"/api/v3/repos/acme/api/git/refs/heads/feature/x"}; !equalStrings(rs.deletePaths, want) {
		t.Errorf("DELETE paths = %v, want %v", rs.deletePaths, want)
	}
	if rs.closeState != "closed" {
		t.Errorf("ClosePR sent state=%q, want closed", rs.closeState)
	}

	pr := getDeliverable(t, srv, artID)
	if pr.State != domain.DeliverableStatePRClosed {
		t.Errorf("PR deliverable state = %q, want closed", pr.State)
	}
	details, err := domain.ParsePRDeliverableDetails(pr.DetailsJSON)
	if err != nil || details.Resolution != domain.PRResolutionRejected {
		t.Errorf("PR details = %+v (err %v), want resolution=rejected", details, err)
	}
	if note := domain.DeliverableResolutionNote(*pr); !strings.Contains(note, "rejected") || !strings.Contains(note, "feature/x") {
		t.Errorf("agent note = %q, want the rejection naming the branch", note)
	}
	if details.Proposed.Title != "Proposed title" {
		t.Errorf("proposed snapshot must survive the flip; got %q", details.Proposed.Title)
	}
	arts, _, err := sqlitestore.New(srv.db).Deliverables.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID, db.Unwindowed)
	if err != nil {
		t.Fatalf("list conversation deliverables: %v", err)
	}
	if len(arts) != 1 || arts[0].ID != artID {
		t.Errorf("conversation deliverables = %+v, want the PR alone: a branch delete writes no deliverable", arts)
	}

	acts, _, err := sqlitestore.New(srv.db).ExternalActions.ListByOrgSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionBranchDeleted})
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(acts) != 1 || acts[0].Target != "acme/api" || acts[0].ExternalID != "refs/heads/feature/x" || acts[0].ConversationID != conversationID {
		t.Errorf("branch_deleted audit rows = %+v, want one on acme/api refs/heads/feature/x for the drafting conversation", acts)
	}
	if wantURL, _ := domain.BranchWebURL(pr.Scope, "acme/api", "refs/heads/feature/x"); len(acts) == 1 && (wantURL == "" || acts[0].URL != wantURL) {
		t.Errorf("branch_deleted url = %q, want the branch on the deliverable's host %q", acts[0].URL, wantURL)
	}
	// The branch delete is about the repository, by its registry row.
	repo, err := sqlitestore.New(srv.db).Repos.GetByRefSystem(context.Background(), runmode.LocalDefaultOrgID, domain.RepoRefFromSlug(pr.Scope, "acme/api"))
	if err != nil || repo == nil {
		t.Fatalf("read acme/api's registry row: %v, %v", repo, err)
	}
	if len(acts) == 1 && (acts[0].RepositoryID != repo.ID || acts[0].EntityID != "") {
		t.Errorf("branch_deleted references (entity %q, repository %q), want repository %s", acts[0].EntityID, acts[0].RepositoryID, repo.ID)
	}
	// The object is the branch, by its ref, as a push to it names it.
	if len(acts) == 1 && (acts[0].ObjectKind != domain.ObjectKindBranch || acts[0].ObjectKey != "refs/heads/feature/x") {
		t.Errorf("branch_deleted names (%q, %q), want (branch, refs/heads/feature/x)", acts[0].ObjectKind, acts[0].ObjectKey)
	}
	closedActs, _, err := sqlitestore.New(srv.db).ExternalActions.ListByOrgSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionPRClosed})
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(closedActs) != 1 {
		t.Errorf("pr_closed audit rows = %d, want 1", len(closedActs))
	} else if closedActs[0].EntityID != pr.EntityID || closedActs[0].ObjectKind != domain.ObjectKindPullRequest {
		t.Errorf("pr_closed references entity %q naming %q, want the PR deliverable's %q naming pull_request",
			closedActs[0].EntityID, closedActs[0].ObjectKind, pr.EntityID)
	}

	assertAgentMemoryUntouched(t, srv, conversationID)

	if !storedConcluded(t, srv.db, conversationID) {
		t.Error("conversation no longer concluded (reject must not flip conversation lifecycle)")
	}
}

// TestDeliverableReject_BranchAlreadyGone_ReconcilesAnd409: when the branch was
// deleted out of band, the verb reconciles the row against GitHub and answers 409
// without writing anything of its own: no close, no rejection, no audit row.
func TestDeliverableReject_BranchAlreadyGone_ReconcilesAnd409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	// REST still says draft (the race window); GraphQL — what the reconciler
	// reads — already says closed.
	rs := newRejectStub(t, srv, livePR("open", true, false), liveNode("CLOSED", true, false), http.StatusUnprocessableEntity, `{"message":"Reference does not exist"}`)

	artID, conversationID, _ := seedDraftPRDeliverableWithConversation(t, srv, "gone", "acme", "api", 42)
	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/reject", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reject = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "feature/x") || !strings.Contains(rec.Body.String(), httpx.ReasonAlreadyTerminal) {
		t.Errorf("error body %s should name the branch under ALREADY_TERMINAL", rec.Body.String())
	}
	if rs.closeState != "" {
		t.Errorf("ClosePR must not run when the branch was not deleted here; sent state=%q", rs.closeState)
	}
	pr := getDeliverable(t, srv, artID)
	if pr.State != domain.DeliverableStatePRClosed {
		t.Errorf("deliverable state = %q, want closed (reconciled from GitHub)", pr.State)
	}
	details, err := domain.ParsePRDeliverableDetails(pr.DetailsJSON)
	if err != nil || details.Resolution != domain.PRResolutionGitHub {
		t.Errorf("PR details = %+v (err %v), want resolution=github — the reconciler resolved it, not the verb", details, err)
	}
	if note := domain.DeliverableResolutionNote(*pr); !strings.Contains(note, "on GitHub") || strings.Contains(note, "rejected") || strings.Contains(note, "dismissed") {
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

// TestDeliverableReject_ResolvedOnGitHub409: a draft merged, closed or marked
// ready on GitHub since the overlay loaded gets no DELETE; the row is reconciled
// and the answer is 409.
func TestDeliverableReject_ResolvedOnGitHub409(t *testing.T) {
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
			artID, _, _ := seedDraftPRDeliverableWithConversation(t, srv, "oob", "acme", "api", 42)
			rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/reject", nil)
			if rec.Code != http.StatusConflict {
				t.Fatalf("reject = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantWord) {
				t.Errorf("error body %s should say the PR was %s", rec.Body.String(), tc.wantWord)
			}
			if len(rs.deletePaths) != 0 {
				t.Errorf("no DELETE may be sent for a PR resolved on GitHub; got %v", rs.deletePaths)
			}
			pr := getDeliverable(t, srv, artID)
			if pr.State == domain.DeliverableStatePRDraft {
				t.Errorf("deliverable still draft; the verb should have reconciled it")
			}
			if d, _ := domain.ParsePRDeliverableDetails(pr.DetailsJSON); d.Resolution != domain.PRResolutionGitHub {
				t.Errorf("resolution = %q, want github", d.Resolution)
			}
		})
	}
}

// TestDeliverableApprove_ResolvedOnGitHub409: approving a draft someone closed on
// GitHub is a 409 and a reconcile, not a ready-for-review mutation GitHub would
// refuse.
func TestDeliverableApprove_ResolvedOnGitHub409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	rs := newRejectStub(t, srv, livePR("closed", true, false), liveNode("CLOSED", true, false), http.StatusNoContent, "")
	artID, _, _ := seedDraftPRDeliverableWithConversation(t, srv, "aoob", "acme", "api", 42)
	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if rs.marked {
		t.Error("MarkPRReady must not run on a PR already closed on GitHub")
	}
	pr := getDeliverable(t, srv, artID)
	if pr.State != domain.DeliverableStatePRClosed {
		t.Errorf("deliverable state = %q, want closed (reconciled)", pr.State)
	}
	acts, _, err := sqlitestore.New(srv.db).ExternalActions.ListByOrgSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionPRMarkedReady})
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(acts) != 0 {
		t.Errorf("no pr_marked_ready audit row for an approval that did not happen; got %d", len(acts))
	}
}

// TestDeliverableReject_DeleteRefused_NothingChanges: when GitHub refuses the
// branch delete the handler answers 502, sends no close, and leaves the
// deliverable a draft.
func TestDeliverableReject_DeleteRefused_NothingChanges(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	rs := newRejectStub(t, srv, livePR("open", true, false), liveNode("OPEN", true, false), http.StatusForbidden, `{"message":"Resource not accessible by integration"}`)

	artID, _, _ := seedDraftPRDeliverableWithConversation(t, srv, "refused", "acme", "api", 42)
	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/reject", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("reject = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "feature/x") {
		t.Errorf("error body %s should name the branch it could not delete", rec.Body.String())
	}
	if rs.closeState != "" {
		t.Errorf("ClosePR must not run after a refused delete; sent state=%q", rs.closeState)
	}
	if got := getDeliverable(t, srv, artID).State; got != domain.DeliverableStatePRDraft {
		t.Errorf("deliverable state = %q, want draft (nothing changed)", got)
	}
	acts, _, err := sqlitestore.New(srv.db).ExternalActions.ListByOrgSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionBranchDeleted})
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(acts) != 0 {
		t.Errorf("no branch_deleted audit row may exist after a refused delete; got %d", len(acts))
	}
}

// TestDeliverableReject_TerminalDeliverable409: once a draft is resolved, a
// second reject and an approve racing the reject are both a clean 409 with no
// second GitHub write.
func TestDeliverableReject_TerminalDeliverable409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	rs := newRejectStub(t, srv, livePR("open", true, false), liveNode("OPEN", true, false), http.StatusNoContent, "")

	artID, _, _ := seedDraftPRDeliverableWithConversation(t, srv, "rej409", "acme", "api", 42)
	if rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/reject", nil); rec.Code != http.StatusOK {
		t.Fatalf("first reject = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/reject", nil); rec.Code != http.StatusConflict {
		t.Errorf("second reject = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil); rec.Code != http.StatusConflict {
		t.Errorf("approve after reject = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if len(rs.deletePaths) != 1 || rs.marked {
		t.Errorf("GitHub writes = deletes %v, marked ready %v; want the first reject's one delete and nothing else", rs.deletePaths, rs.marked)
	}
}

// TestDeliverableReject_UnknownHeadBranch409: a PR row that never recorded its
// head branch has nothing to delete, so reject answers 409 rather than deleting a
// guessed ref.
func TestDeliverableReject_UnknownHeadBranch409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	rs := newRejectStub(t, srv, livePR("open", true, false), liveNode("OPEN", true, false), http.StatusNoContent, "")

	artID, _, _ := seedDraftPRDeliverableWithConversation(t, srv, "nohead", "acme", "api", 42)
	execSQL(t, srv.db, `UPDATE deliverables SET details_json = ? WHERE id = ?`,
		domain.MarshalPRDeliverableDetails(domain.PRDeliverableDetails{Base: "main"}), artID)
	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/reject", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reject = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if len(rs.deletePaths) != 0 {
		t.Errorf("no DELETE may be sent for an unknown head; got %v", rs.deletePaths)
	}
	if got := getDeliverable(t, srv, artID).State; got != domain.DeliverableStatePRDraft {
		t.Errorf("deliverable state = %q, want draft", got)
	}
}

// TestDeliverableRoutes_AnDeliverableOnAnotherHost: a deliverable recorded on a
// host other than the org's, where owner/repo#N may be a different pull request,
// reads back its stored snapshot and answers writes 409 without calling GitHub.
func TestDeliverableRoutes_AnDeliverableOnAnotherHost(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var calls atomic.Int32
	mux := newAppAPIMux()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/repos/") || strings.Contains(r.URL.Path, "/graphql") {
			calls.Add(1)
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedDraftPRDeliverableWithConversation(t, srv, "otherhost", "acme", "api", 42)
	if _, err := srv.db.Exec(`UPDATE deliverables SET scope = 'https://old.example.com' WHERE id = ?`, artID); err != nil {
		t.Fatalf("move deliverable to another host: %v", err)
	}

	rec := doJSON(t, srv, http.MethodGet, "/api/deliverables/"+artID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Details struct {
			Title string `json:"title"`
		} `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Details.Title != "Proposed title" {
		t.Errorf("title = %q, want the stored snapshot's", got.Details.Title)
	}

	rec = doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if e := firstError(t, rec); !strings.Contains(e.Message, "https://old.example.com") {
		t.Errorf("approve message = %q, want it to name the deliverable's host", e.Message)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("GitHub saw %d repository calls for a deliverable on another host, want 0", n)
	}
	if got := getDeliverable(t, srv, artID).State; got != domain.DeliverableStatePRDraft {
		t.Errorf("deliverable state = %q, want draft (unchanged)", got)
	}
}

// TestDeliverableActions_RefuseAnUnverifiedPullRequest: every route that
// writes to a pull request under TF's credential refuses a row TF has not
// confirmed its run opened, before it reaches GitHub. A pending row with no
// reconciler to settle it is refused as not yet confirmed, and a foreign one
// as never to be acted on.
func TestDeliverableActions_RefuseAnUnverifiedPullRequest(t *testing.T) {
	for _, verification := range []string{domain.DeliverableVerificationPending, domain.DeliverableVerificationForeign} {
		t.Run(verification, func(t *testing.T) {
			keyring.MockInit()
			srv := newTestServer(t)
			artID, _, _ := seedDraftPRDeliverableWithConversation(t, srv, "unv-"+verification, "acme", "api", 42)
			execSQL(t, srv.db, `UPDATE deliverables SET verification = ? WHERE id = ?`, verification, artID)

			for _, call := range []struct{ method, path string }{
				{http.MethodPost, "/api/deliverables/" + artID + "/approve"},
				{http.MethodPost, "/api/deliverables/" + artID + "/dismiss"},
				{http.MethodPost, "/api/deliverables/" + artID + "/reject"},
				{http.MethodPatch, "/api/deliverables/" + artID + "/pr"},
			} {
				var body any
				if call.method == http.MethodPatch {
					body = map[string]any{"title": "t", "body": "b"}
				}
				rec := doJSON(t, srv, call.method, call.path, body)
				var resp struct {
					Errors []struct {
						Reason string `json:"reason"`
					} `json:"errors"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &resp)
				if rec.Code != http.StatusConflict || len(resp.Errors) != 1 || resp.Errors[0].Reason != httpx.ReasonDeliverableUnverified {
					t.Errorf("%s %s = %d %s, want 409 %s", call.method, call.path, rec.Code, rec.Body.String(), httpx.ReasonDeliverableUnverified)
				}
			}
			if got := getDeliverable(t, srv, artID); got.State != domain.DeliverableStatePRDraft {
				t.Errorf("deliverable state = %q, want the draft untouched", got.State)
			}
		})
	}
}

// TestDeliverableReject_SettlesAPendingRowOnDemand: a pending row is settled
// by the click itself, so a person acting on a run's pull request does not
// wait on the reconciler's next pass.
func TestDeliverableReject_SettlesAPendingRowOnDemand(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	newRejectStub(t, srv, livePR("open", true, false), liveNode("OPEN", true, false), http.StatusNoContent, "")
	artID, _, _ := seedDraftPRDeliverableWithConversation(t, srv, "rejpend", "acme", "api", 42)
	execSQL(t, srv.db, `UPDATE deliverables SET verification = ? WHERE id = ?`, domain.DeliverableVerificationPending, artID)

	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/reject", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := getDeliverable(t, srv, artID); got.Verification != domain.DeliverableVerificationVerified || got.State != domain.DeliverableStatePRClosed {
		t.Errorf("deliverable = (%q, %q), want verified and closed", got.Verification, got.State)
	}
}
