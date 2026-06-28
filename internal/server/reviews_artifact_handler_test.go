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

// seedReviewArtifactWithRun mints a completed (terminal) run chain and a
// finalized review-draft artifact hung off it (TFAC-494): state=pending, ready
// sentinel set, the head SHA pinned, one staged inline comment, and the agent's
// draft snapshotted into proposed. The whole review is staged TF-side — no GitHub
// object exists until approval. The run is completed because approval/dismiss are
// decoupled sidecars that never flip run lifecycle (TFAC-379). Returns
// (artifactID, runID, taskID).
func seedReviewArtifactWithRun(t *testing.T, s *Server, suffix, owner, repo string, number int, event string) (artifactID, runID, taskID string) {
	t.Helper()
	runID = seedSteerRun(t, s.db, suffix, "completed")
	taskID = "t_" + suffix
	if err := sqlitestore.New(s.db).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, runID, "e_"+suffix, "", "agent self-report"); err != nil {
		t.Fatalf("seed agent memory: %v", err)
	}
	line := 3
	a := domain.NewReviewArtifact(owner+"/"+repo, number, "headsha"+suffix, runID)
	a.RunID = runID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	d, _ := domain.ParseReviewArtifactDetails(a.DetailsJSON)
	d.ReviewBody = "## Review\nlgtm"
	d.ReviewEvent = event
	// The staged comment carries the badge baked in — what the agent drafted. The
	// proposed snapshot is identical, so the verdict diff resolves to "as drafted".
	staged := []domain.ReviewArtifactComment{
		{ID: "c_1", Path: "a.go", Line: &line, Body: domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "nit: rename"},
	}
	d.StagedComments = staged
	d.Proposed = domain.ReviewArtifactProposed{Body: "## Review\nlgtm", Event: event, Comments: staged}
	a.DetailsJSON = domain.MarshalReviewArtifactDetails(d)
	stored, err := sqlitestore.New(s.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a)
	if err != nil {
		t.Fatalf("seed review artifact: %v", err)
	}
	return stored.ID, runID, taskID
}

// TestReviewArtifactGet_SeverityRoundTrip pins the read: GET returns the artifact
// with its TF-side staged comments — no GitHub call — each with its severity
// parsed back out of the body (the chip) and the clean body shown.
func TestReviewArtifactGet_SeverityRoundTrip(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rget", "acme", "api", 7, "COMMENT")
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
	// A draft has no GitHub review id yet.
	if out.ReviewID != "" || out.ReviewEvent != "COMMENT" {
		t.Errorf("review id/event = %q/%q, want empty/COMMENT", out.ReviewID, out.ReviewEvent)
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

// TestReviewArtifactApprove pins the atomic submit-on-approval flow: SubmitReview
// POSTs the staged body+event+footer+comments to GitHub, the artifact flips
// pending → submitted and gains the submitted review's id + URL, the run
// completes, and the human verdict lands in run_memory.
func TestReviewArtifactApprove(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var submitBody map[string]any
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&submitBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 12345})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, runID, _ := seedReviewArtifactWithRun(t, srv, "rappr", "acme", "api", 7, "COMMENT")
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The submit reached GitHub atomically: staged event, body + footer, commit
	// pin, and the staged comment all in one POST.
	if submitBody == nil {
		t.Fatal("approve must call SubmitReview")
	}
	if submitBody["event"] != "COMMENT" {
		t.Errorf("submit event = %v, want COMMENT", submitBody["event"])
	}
	if submitBody["commit_id"] != "headsharappr" {
		t.Errorf("submit commit_id = %v, want the pinned head SHA", submitBody["commit_id"])
	}
	if got, _ := submitBody["body"].(string); !strings.HasPrefix(got, "## Review") || !strings.Contains(got, "Triage Factory") {
		t.Errorf("submit body = %q, want staged body + agentmeta footer", got)
	}
	comments, _ := submitBody["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("submit comments = %v, want the one staged comment", submitBody["comments"])
	}

	art := getArtifact(t, srv, artID)
	if art.State != domain.ArtifactStateReviewSubmitted {
		t.Errorf("artifact state = %q, want submitted", art.State)
	}
	if art.ExternalID != "12345" {
		t.Errorf("artifact ExternalID = %q, want the submitted review id 12345", art.ExternalID)
	}
	if !strings.Contains(art.URL, "pullrequestreview-12345") {
		t.Errorf("artifact URL = %q, want a review anchor", art.URL)
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
	var submitHit bool
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		submitHit = true
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "r409", "acme", "api", 7, "COMMENT")
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
	if submitHit {
		t.Error("a 409 approve must not touch GitHub")
	}
}

// TestReviewArtifactApprove_NilLineComment_422 pins that a staged comment with no
// line anchor can't be submitted: approve 422s before any GitHub call rather than
// silently dropping it.
func TestReviewArtifactApprove_NilLineComment_422(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var submitHit bool
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		submitHit = true
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rnil", "acme", "api", 7, "COMMENT")
	// Null out the staged comment's line anchor.
	art := getArtifact(t, srv, artID)
	d, _ := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	d.StagedComments[0].Line = nil
	art.DetailsJSON = domain.MarshalReviewArtifactDetails(d)
	if _, err := sqlitestore.New(srv.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, *art); err != nil {
		t.Fatalf("null line: %v", err)
	}
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/approve", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("approve = %d, want 422 on an unpositioned comment; body=%s", rec.Code, rec.Body.String())
	}
	if submitHit {
		t.Error("a 422 must not reach GitHub")
	}
	if getArtifact(t, srv, artID).State != domain.ArtifactStateReviewPending {
		t.Error("artifact must stay pending after a 422")
	}
}

// TestReviewArtifactCommentUpdate_RebakesBadge pins that a staged-comment edit
// re-bakes the comment's existing severity onto the human's clean body, with no
// GitHub call.
func TestReviewArtifactCommentUpdate_RebakesBadge(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rcu", "acme", "api", 7, "COMMENT")
	rec := doJSON(t, srv, http.MethodPut, "/api/artifacts/"+artID+"/comments/c_1", map[string]any{"body": "rename to fooBar"})
	if rec.Code != http.StatusOK {
		t.Fatalf("comment update = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	d, _ := domain.ParseReviewArtifactDetails(getArtifact(t, srv, artID).DetailsJSON)
	if len(d.StagedComments) != 1 {
		t.Fatalf("staged comments = %d, want 1", len(d.StagedComments))
	}
	// The MAJOR badge (recovered from the staged comment) must be re-baked on, and
	// the human's clean prose preserved.
	got := d.StagedComments[0].Body
	if !strings.HasPrefix(got, domain.SeverityBadgeMarkdown(domain.SeverityMajor)) {
		t.Errorf("updated body = %q, want the MAJOR badge re-baked in", got)
	}
	if !strings.HasSuffix(got, "rename to fooBar") {
		t.Errorf("updated body = %q, want the human's clean prose preserved", got)
	}
}

// TestReviewArtifactCommentUpdate_NotFound_404 pins that an edit targeting a
// comment id not in the staged set is rejected with 404.
func TestReviewArtifactCommentUpdate_NotFound_404(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rcw", "acme", "api", 7, "COMMENT")
	rec := doJSON(t, srv, http.MethodPut, "/api/artifacts/"+artID+"/comments/c_missing", map[string]any{"body": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update = %d, want 404 for a comment not in the staged set; body=%s", rec.Code, rec.Body.String())
	}
}

// TestReviewArtifactCommentDelete pins delete on the staged set (found → 200,
// removed; missing → 404), with no GitHub call.
func TestReviewArtifactCommentDelete(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rcd", "acme", "api", 7, "COMMENT")

	rec := doJSON(t, srv, http.MethodDelete, "/api/artifacts/"+artID+"/comments/c_1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if d, _ := domain.ParseReviewArtifactDetails(getArtifact(t, srv, artID).DetailsJSON); len(d.StagedComments) != 0 {
		t.Errorf("staged comments = %d after delete, want 0", len(d.StagedComments))
	}

	rec = doJSON(t, srv, http.MethodDelete, "/api/artifacts/"+artID+"/comments/c_missing", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete = %d, want 404 for a comment not in the staged set; body=%s", rec.Code, rec.Body.String())
	}
}

// TestReviewArtifactUpdate_InvalidEvent_400 pins early validation of the staged
// verdict: an invalid (or empty) review_event is rejected at PATCH time and the
// stored sentinel is left unchanged (an empty event would otherwise un-park the run).
func TestReviewArtifactUpdate_InvalidEvent_400(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rie", "acme", "api", 7, "COMMENT")
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

// TestReviewArtifactUpdate_NonPending_409 pins that staging is rejected once the
// review is no longer pending: a PATCH on a submitted/dismissed artifact returns
// 409 and doesn't write body/event the user can't act on.
func TestReviewArtifactUpdate_NonPending_409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rnp", "acme", "api", 7, "COMMENT")
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

// TestReviewArtifactDismiss pins the per-artifact dismiss for reviews: POST
// /api/artifacts/{id}/dismiss flips the artifact pending → dismissed with NO
// GitHub call (the review is staged TF-side, TFAC-494) and never touches the run
// lifecycle (the decoupled sidecar, TFAC-379).
func TestReviewArtifactDismiss(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	artID, runID, _ := seedReviewArtifactWithRun(t, srv, "rdis", "acme", "api", 7, "COMMENT")
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/dismiss", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d, want 200; body=%s", rec.Code, rec.Body.String())
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
	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rdis409", "acme", "api", 7, "COMMENT")
	execSQL(t, srv.db, `UPDATE artifacts SET state = ? WHERE id = ?`, domain.ArtifactStateReviewSubmitted, artID)
	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/dismiss", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("dismiss of submitted review = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}
