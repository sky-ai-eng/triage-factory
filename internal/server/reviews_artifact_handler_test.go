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

// seedReviewArtifactWithRun mints a pending_approval run chain and a finalized
// review artifact hung off it (state=pending, ready sentinel set, agent draft
// snapshotted into proposed). Returns (artifactID, runID, taskID). reviewID is
// the GitHub review node id stored as ExternalID.
func seedReviewArtifactWithRun(t *testing.T, s *Server, suffix, owner, repo string, number int, reviewID, event string) (artifactID, runID, taskID string) {
	t.Helper()
	runID = seedSteerRun(t, s.db, suffix, "pending_approval")
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
