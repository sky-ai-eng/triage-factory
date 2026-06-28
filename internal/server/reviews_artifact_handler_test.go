package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedReviewArtifactWithRun mints a completed (terminal) run chain and a finalized
// review artifact hung off it (state=pending, ready sentinel set, agent draft
// snapshotted into proposed). Returns (artifactID, runID, taskID). reviewID is
// the GitHub review node id stored as ExternalID.
func seedReviewArtifactWithRun(t *testing.T, s *Server, suffix, owner, repo string, number int, reviewID, event string) (artifactID, runID, taskID string) {
	t.Helper()
	runID = seedSteerRun(t, s.db, suffix, "completed")
	taskID = "t_" + suffix
	if err := sqlitestore.New(s.db).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, runID, "e_"+suffix, "", "agent self-report"); err != nil {
		t.Fatalf("seed agent memory: %v", err)
	}
	line := 3
	a := domain.NewReviewArtifact(owner+"/"+repo, number, "PR_node", reviewID)
	a.RunID = runID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	d, _ := domain.ParseReviewArtifactDetails(a.DetailsJSON)
	d.ReviewBody = "## Review\nlgtm"
	d.ReviewEvent = event
	d.Proposed = domain.ReviewArtifactProposed{
		Body:  "## Review\nlgtm",
		Event: event,
		// The proposed comment carries the badge baked in — exactly what the agent
		// drafted on GitHub. The live GraphQL stub returns the same, so the verdict
		// diff resolves to "as drafted".
		Comments: []domain.ReviewArtifactComment{
			{ID: "PRRC_1", Path: "a.go", Line: &line, Body: domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "nit: rename"},
		},
	}
	a.DetailsJSON = domain.MarshalReviewArtifactDetails(d)
	stored, err := sqlitestore.New(s.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a)
	if err != nil {
		t.Fatalf("seed review artifact: %v", err)
	}
	return stored.ID, runID, taskID
}

// pendingReviewGraphQL returns the GraphQL JSON for GetPendingReview with one
// inline comment whose body carries a baked severity badge.
func pendingReviewGraphQL(reviewID, commentID, badgedBody string) string {
	resp := map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"pullRequest": map[string]any{
					"reviews": map[string]any{
						"nodes": []any{
							map[string]any{
								"id":              reviewID,
								"viewerDidAuthor": true,
								"comments": map[string]any{
									"nodes": []any{
										map[string]any{"id": commentID, "path": "a.go", "line": 3, "startLine": nil, "body": badgedBody},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

// TestReviewArtifactGet_SeverityRoundTrip pins the read: GET returns the artifact
// + the LIVE pending-review comments, each with its severity parsed back out of
// the GitHub body (the chip) and the clean body shown.
func TestReviewArtifactGet_SeverityRoundTrip(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	badged := domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "nit: rename"
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pendingReviewGraphQL("PRR_1", "PRRC_1", badged)))
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rget", "acme", "api", 7, "PRR_1", "COMMENT")
	rec := doJSON(t, srv, http.MethodGet, "/api/artifacts/"+artID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ReviewBody  string `json:"review_body"`
		ReviewEvent string `json:"review_event"`
		ReviewID    string `json:"review_id"`
		Comments    []struct {
			Body     string `json:"body"`
			Severity string `json:"severity"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ReviewID != "PRR_1" || out.ReviewEvent != "COMMENT" {
		t.Errorf("review id/event = %q/%q, want PRR_1/COMMENT", out.ReviewID, out.ReviewEvent)
	}
	if len(out.Comments) != 1 {
		t.Fatalf("len(comments) = %d, want 1", len(out.Comments))
	}
	if out.Comments[0].Severity != domain.SeverityMajor {
		t.Errorf("comment severity = %q, want MAJOR (parsed from the body badge)", out.Comments[0].Severity)
	}
	if out.Comments[0].Body != "nit: rename" {
		t.Errorf("comment body = %q, want the clean body with the badge stripped", out.Comments[0].Body)
	}
}

// TestReviewArtifactApprove pins the submit-on-approval flow: SubmitExistingReview
// posts the staged body+event+footer to GitHub, the artifact flips pending →
// submitted, the run completes, and the human verdict lands in run_memory.
func TestReviewArtifactApprove(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	badged := domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "nit: rename"
	var submitVars map[string]any
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "submitPullRequestReview"):
			submitVars = req.Variables
			_, _ = w.Write([]byte(`{"data":{"submitPullRequestReview":{"pullRequestReview":{"id":"PRR_1","state":"COMMENTED"}}}}`))
		default: // GetPendingReview
			_, _ = w.Write([]byte(pendingReviewGraphQL("PRR_1", "PRRC_1", badged)))
		}
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, runID, _ := seedReviewArtifactWithRun(t, srv, "rappr", "acme", "api", 7, "PRR_1", "COMMENT")
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The submit reached GitHub with the staged event and the body + footer.
	if submitVars == nil {
		t.Fatal("approve must call SubmitExistingReview")
	}
	if submitVars["event"] != "COMMENT" {
		t.Errorf("submit event = %v, want COMMENT", submitVars["event"])
	}
	if got, _ := submitVars["body"].(string); !strings.HasPrefix(got, "## Review") || !strings.Contains(got, "Triage Factory") {
		t.Errorf("submit body = %q, want staged body + agentmeta footer", got)
	}
	if got := getArtifact(t, srv, artID).State; got != domain.ArtifactStateReviewSubmitted {
		t.Errorf("artifact state = %q, want submitted", got)
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

// TestReviewArtifactDismiss pins the per-artifact dismiss for reviews: POST
// /api/artifacts/{id}/dismiss deletes the GitHub pending review (best-effort) and
// flips the artifact pending → dismissed, never touching the run lifecycle.
func TestReviewArtifactDismiss(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var deleted bool
	mux := newAppAPIMux()
	// DeletePendingReview resolves the review node id to its databaseId via
	// GraphQL, then issues a REST DELETE keyed on that database id.
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"node":{"databaseId":555}}}`))
	})
	mux.HandleFunc("DELETE /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews/{rid}", func(w http.ResponseWriter, r *http.Request) {
		deleted = true
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, runID, _ := seedReviewArtifactWithRun(t, srv, "rdis", "acme", "api", 7, "PRR_1", "COMMENT")
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/dismiss", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !deleted {
		t.Errorf("dismiss must delete the GitHub pending review")
	}
	if got := getArtifact(t, srv, artID).State; got != domain.ArtifactStateReviewDismissed {
		t.Errorf("artifact state = %q, want dismissed", got)
	}
	var runStatus string
	if err := srv.db.QueryRow(`SELECT status FROM runs WHERE id=?`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "completed" {
		t.Errorf("run status = %q, want completed (dismiss must not flip run lifecycle)", runStatus)
	}
}

// TestReviewArtifactDismiss_NonPending_409 pins the state guard: dismissing an
// already-submitted (terminal) review returns 409.
func TestReviewArtifactDismiss_NonPending_409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rdis409", "acme", "api", 7, "PRR_1", "COMMENT")
	execSQL(t, srv.db, `UPDATE artifacts SET state = ? WHERE id = ?`, domain.ArtifactStateReviewSubmitted, artID)
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/dismiss", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("dismiss of submitted review = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestReviewArtifactApprove_NonPending_409 pins the state guard: a stale/double
// approve on an already-submitted review returns 409 and makes no GitHub call.
func TestReviewArtifactApprove_NonPending_409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var graphqlHit bool
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		graphqlHit = true
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "r409", "acme", "api", 7, "PRR_1", "COMMENT")
	// Flip it to submitted out-of-band.
	art := getArtifact(t, srv, artID)
	art.State = domain.ArtifactStateReviewSubmitted
	if _, err := sqlitestore.New(srv.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, *art); err != nil {
		t.Fatalf("flip submitted: %v", err)
	}
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve = %d, want 409 on a non-pending review; body=%s", rec.Code, rec.Body.String())
	}
	if graphqlHit {
		t.Error("a 409 approve must not touch GitHub")
	}
}

// TestReviewArtifactCommentUpdate_RebakesBadge pins that a comment edit re-bakes
// the comment's existing severity onto the human's clean body, and is pessimistic
// (a GitHub failure surfaces as non-2xx).
func TestReviewArtifactCommentUpdate_RebakesBadge(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	badged := domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "nit: rename"
	var updateBody string
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "updatePullRequestReviewComment"):
			updateBody, _ = req.Variables["body"].(string)
			_, _ = w.Write([]byte(`{"data":{"updatePullRequestReviewComment":{"pullRequestReviewComment":{"id":"PRRC_1"}}}}`))
		default: // GetPendingReview (recovers the current severity)
			_, _ = w.Write([]byte(pendingReviewGraphQL("PRR_1", "PRRC_1", badged)))
		}
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rcu", "acme", "api", 7, "PRR_1", "COMMENT")
	rec := doJSON(t, srv, http.MethodPut, "/api/artifacts/"+artID+"/comments/PRRC_1", map[string]any{"body": "rename to fooBar"})
	if rec.Code != http.StatusOK {
		t.Fatalf("comment update = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The MAJOR badge (recovered from the live comment) must be re-baked onto the
	// human's clean body.
	if !strings.HasPrefix(updateBody, domain.SeverityBadgeMarkdown(domain.SeverityMajor)) {
		t.Errorf("updated body = %q, want the MAJOR badge re-baked in", updateBody)
	}
	if !strings.HasSuffix(updateBody, "rename to fooBar") {
		t.Errorf("updated body = %q, want the human's clean prose preserved", updateBody)
	}
}

// TestReviewArtifactCommentUpdate_Pessimistic pins the pessimistic contract: a
// GitHub failure on the comment edit surfaces as non-2xx (no silent success).
func TestReviewArtifactCommentUpdate_Pessimistic(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	badged := domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "nit"
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "updatePullRequestReviewComment") {
			http.Error(w, `{"errors":[{"message":"comment is on an outdated line"}]}`, http.StatusUnprocessableEntity)
			return
		}
		_, _ = w.Write([]byte(pendingReviewGraphQL("PRR_1", "PRRC_1", badged)))
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rcup", "acme", "api", 7, "PRR_1", "COMMENT")
	rec := doJSON(t, srv, http.MethodPut, "/api/artifacts/"+artID+"/comments/PRRC_1", map[string]any{"body": "new"})
	if rec.Code < 400 {
		t.Fatalf("comment update = %d, want non-2xx on GitHub failure; body=%s", rec.Code, rec.Body.String())
	}
}

// TestReviewArtifactCommentUpdate_WrongReview_404 pins the scoping guard: a
// commentId that isn't part of THIS artifact's pending review is rejected (404)
// and never reaches the GitHub update mutation, even though the mutation keys only
// on the comment node id.
func TestReviewArtifactCommentUpdate_WrongReview_404(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	badged := domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "nit"
	var updateCalled bool
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "updatePullRequestReviewComment") {
			updateCalled = true
			_, _ = w.Write([]byte(`{"data":{"updatePullRequestReviewComment":{"pullRequestReviewComment":{"id":"x"}}}}`))
			return
		}
		// This review's only comment is PRRC_owned — not the one the request edits.
		_, _ = w.Write([]byte(pendingReviewGraphQL("PRR_1", "PRRC_owned", badged)))
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rcw", "acme", "api", 7, "PRR_1", "COMMENT")
	rec := doJSON(t, srv, http.MethodPut, "/api/artifacts/"+artID+"/comments/PRRC_elsewhere", map[string]any{"body": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update = %d, want 404 for a comment outside this review; body=%s", rec.Code, rec.Body.String())
	}
	if updateCalled {
		t.Error("a wrong-review update (404) must not reach the GitHub update mutation")
	}
}

// TestReviewArtifactCommentDelete pins delete on the live review (found → 200,
// delete mutation keyed on the node id) and the wrong-review guard (404, no
// delete) — the delete mutation keys only on the comment id, so the membership
// check is what scopes it to this artifact's review.
func TestReviewArtifactCommentDelete(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	badged := domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "nit"
	var deleted string
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "deletePullRequestReviewComment") {
			deleted, _ = req.Variables["id"].(string)
			_, _ = w.Write([]byte(`{"data":{"deletePullRequestReviewComment":{"pullRequestReviewComment":{"id":"PRRC_1"}}}}`))
			return
		}
		_, _ = w.Write([]byte(pendingReviewGraphQL("PRR_1", "PRRC_1", badged)))
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rcd", "acme", "api", 7, "PRR_1", "COMMENT")

	rec := doJSON(t, srv, http.MethodDelete, "/api/artifacts/"+artID+"/comments/PRRC_1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if deleted != "PRRC_1" {
		t.Errorf("deleted node id = %q, want PRRC_1", deleted)
	}

	deleted = ""
	rec = doJSON(t, srv, http.MethodDelete, "/api/artifacts/"+artID+"/comments/PRRC_elsewhere", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete = %d, want 404 for a comment outside this review; body=%s", rec.Code, rec.Body.String())
	}
	if deleted != "" {
		t.Error("a wrong-review delete (404) must not reach the GitHub delete mutation")
	}
}

// TestReviewArtifactUpdate_InvalidEvent_400 pins early validation of the staged
// verdict: an invalid (or empty) review_event is rejected at PATCH time and the
// stored sentinel is left unchanged (an empty event would otherwise un-park the run).
func TestReviewArtifactUpdate_InvalidEvent_400(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rie", "acme", "api", 7, "PRR_1", "COMMENT")
	for _, bad := range []string{"", "approve", "LGTM"} {
		rec := doJSON(t, srv, http.MethodPatch, "/api/artifacts/"+artID, map[string]any{"review_event": bad})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH review_event=%q = %d, want 400", bad, rec.Code)
		}
	}
	d, _ := domain.ParseReviewArtifactDetails(getArtifact(t, srv, artID).DetailsJSON)
	if d.ReviewEvent != "COMMENT" {
		t.Errorf("review_event = %q after rejected PATCHes, want COMMENT (unchanged)", d.ReviewEvent)
	}
}

// TestReviewArtifactApprove_ReviewIDDrift_409 pins the consistency guard: when the
// live pending review on the PR isn't the one recorded on the artifact, approve
// 409s before any submit rather than diffing the verdict against a different review.
func TestReviewArtifactApprove_ReviewIDDrift_409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var submitCalled bool
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "submitPullRequestReview") {
			submitCalled = true
			_, _ = w.Write([]byte(`{"data":{"submitPullRequestReview":{"pullRequestReview":{"id":"x"}}}}`))
			return
		}
		// The live pending review id differs from the recorded PRR_1.
		_, _ = w.Write([]byte(pendingReviewGraphQL("PRR_DIFFERENT", "PRRC_1", "nit")))
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rdrift", "acme", "api", 7, "PRR_1", "COMMENT")
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve = %d, want 409 on review-id drift; body=%s", rec.Code, rec.Body.String())
	}
	if submitCalled {
		t.Error("a drift 409 must not submit anything to GitHub")
	}
	if got := getArtifact(t, srv, artID).State; got != domain.ArtifactStateReviewPending {
		t.Errorf("artifact state = %q after 409, want still pending", got)
	}
}

// TestReviewArtifactUpdate_NonPending_409 pins that staging is rejected once the
// review is no longer pending: a PATCH on a submitted/dismissed artifact returns
// 409 and doesn't write body/event the user can't act on.
func TestReviewArtifactUpdate_NonPending_409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rnp", "acme", "api", 7, "PRR_1", "COMMENT")
	// Flip to submitted out-of-band.
	art := getArtifact(t, srv, artID)
	art.State = domain.ArtifactStateReviewSubmitted
	if _, err := sqlitestore.New(srv.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, *art); err != nil {
		t.Fatalf("flip submitted: %v", err)
	}
	rec := doJSON(t, srv, http.MethodPatch, "/api/artifacts/"+artID, map[string]any{"review_body": "edit"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("PATCH on submitted review = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	// The staged body must be unchanged.
	if d, _ := domain.ParseReviewArtifactDetails(getArtifact(t, srv, artID).DetailsJSON); d.ReviewBody != "## Review\nlgtm" {
		t.Errorf("review_body = %q after rejected PATCH, want unchanged", d.ReviewBody)
	}
}

// TestReviewArtifactCommentOps_ReviewIDDrift_409 pins that an edit/delete on a
// pending review whose live id no longer matches the artifact's recorded id (the
// review was replaced out-of-band) is rejected with 409 before any GitHub
// mutation — even when the target comment is present in the live (wrong) review.
func TestReviewArtifactCommentOps_ReviewIDDrift_409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	badged := domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "nit"
	var mutated bool
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "updatePullRequestReviewComment") || strings.Contains(req.Query, "deletePullRequestReviewComment") {
			mutated = true
			_, _ = w.Write([]byte(`{"data":{}}`))
			return
		}
		// The live pending review id (PRR_LIVE) differs from the recorded PRR_1,
		// but it does contain PRRC_1 — the id check must fire before the membership
		// check.
		_, _ = w.Write([]byte(pendingReviewGraphQL("PRR_LIVE", "PRRC_1", badged)))
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rcdrift", "acme", "api", 7, "PRR_1", "COMMENT")

	rec := doJSON(t, srv, http.MethodPut, "/api/artifacts/"+artID+"/comments/PRRC_1", map[string]any{"body": "x"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("update on drifted review = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, srv, http.MethodDelete, "/api/artifacts/"+artID+"/comments/PRRC_1", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete on drifted review = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if mutated {
		t.Error("a drift 409 must not reach the GitHub update/delete mutation")
	}
}
