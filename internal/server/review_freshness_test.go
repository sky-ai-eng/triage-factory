package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// reviewFreshnessOut is the slice of the review deliverable's `details` payload
// the freshness tests assert on.
type reviewFreshnessOut struct {
	CommitsSinceFinalize *int `json:"commits_since_finalize"`
	Comments             []struct {
		ID         string `json:"id"`
		Line       int    `json:"line"`
		Freshness  string `json:"freshness"`
		MappedLine int    `json:"mapped_line"`
		MappedPath string `json:"mapped_path"`
	} `json:"comments"`
}

// seedFinalizedReviewAt seeds a finalized review-draft deliverable with a stamped
// finalize-time head and one inline comment on a.go anchored to that head at
// the given line. Returns the deliverable id. The owner must be "acme"
// so the App resolver (acmeInstall) matches.
func seedFinalizedReviewAt(t *testing.T, s *Server, suffix string, number int, finalizeHead string, line int) string {
	t.Helper()
	artID, _, _ := seedReviewDeliverableWithConversation(t, s, suffix, "acme", "api", number, "COMMENT")
	art := getDeliverable(t, s, artID)
	d, _ := domain.ParseReviewDeliverableDetails(art.DetailsJSON)
	d.FinalizedHeadSHA = finalizeHead
	d.FinalizedBaseSHA = "base0" // the finalize-time base tip (for the finalize-frame diff)
	l := line
	d.StagedComments = []domain.ReviewDeliverableComment{
		{ID: "c_1", Path: "a.go", Line: &l, Body: domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "nit: rename", CommitSHA: finalizeHead},
	}
	art.DetailsJSON = domain.MarshalReviewDeliverableDetails(d)
	if _, err := sqlitestore.New(s.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(s.db).Orgs, runmode.LocalDefaultOrgID, *art)); err != nil {
		t.Fatalf("seed finalized review: %v", err)
	}
	return artID
}

// freshnessAppStub wires an App-backed GitHub stub that serves the PR head and,
// optionally, a single compare diff range. The PR object reports head.sha =
// liveHead; a compare request (the JSON form CompareCommits issues) returns
// aheadBy + the supplied files JSON, or fails the test on an unregistered range.
// compareFiles is the JSON for the response's "files" array; "" means no compare
// is expected (the PR hasn't advanced) and any compare request fails the test.
func freshnessAppStub(t *testing.T, s *Server, liveHead string, aheadBy int, compareRange, compareFiles string) {
	t.Helper()
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7,
			"base":   map[string]any{"ref": "main"},
			"head":   map[string]any{"sha": liveHead},
		})
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/compare/{rng}", func(w http.ResponseWriter, r *http.Request) {
		rng := r.PathValue("rng")
		if compareFiles == "" || rng != compareRange {
			t.Errorf("unexpected compare request for range %q (want %q)", rng, compareRange)
			http.Error(w, "no such compare", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"ahead_by":`+strconv.Itoa(aheadBy)+`,"files":`+compareFiles+`}`)
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, s, stub, acmeInstall())
}

// getReviewFreshness GETs the review deliverable and decodes the freshness slice.
func getReviewFreshness(t *testing.T, s *Server, artID string) reviewFreshnessOut {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, "/api/deliverables/"+artID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Details reviewFreshnessOut `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return envelope.Details
}

// TestDeliverableDiff_ReviewServesFinalizeFrame: a review with a stored finalize
// frame renders FinalizedBaseSHA...FinalizedHeadSHA through the compare endpoint
// and does not fetch the live PR diff.
func TestDeliverableDiff_ReviewServesFinalizeFrame(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	const finalizeDiff = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n"
	var gotRange string
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/compare/{rng}", func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.PathValue("rng")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, finalizeDiff)
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "diff") {
			t.Errorf("a finalize-frame review must not fetch the live PR diff")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())
	artID := seedFinalizedReviewAt(t, srv, "rframe", 7, "headA", 3)

	rec := doJSON(t, srv, http.MethodGet, "/api/deliverables/"+artID+"/diff", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("diff = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotRange != "base0...headA" {
		t.Errorf("compare range = %q, want base0...headA (the finalize frame)", gotRange)
	}
	if !strings.Contains(rec.Body.String(), "+y") {
		t.Errorf("diff body = %q, want the finalize-frame compare diff", rec.Body.String())
	}
}

// TestDeliverableDiff_FramelessReviewServesLiveDiff: a review with no stored
// finalize frame renders the live PR diff, not a compare.
func TestDeliverableDiff_FramelessReviewServesLiveDiff(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var liveHit bool
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/compare/{rng}", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a frameless review must not use the compare endpoint for the diff")
		http.Error(w, "unexpected", http.StatusNotFound)
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "diff") {
			liveHit = true
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "diff --git a/z.go b/z.go\n@@ -1 +1 @@\n-a\n+b\n")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())
	// seedReviewDeliverableWithConversation records no finalize frame → frameless.
	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rlive", "acme", "api", 7, "COMMENT")

	rec := doJSON(t, srv, http.MethodGet, "/api/deliverables/"+artID+"/diff", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("diff = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !liveHit {
		t.Error("frameless review must fetch the live PR diff")
	}
}

// TestReviewRefresh_RemapsAndDrops: Refresh reconciles a finalized review to the
// live head — the shifted comment is remapped and re-anchored, the outdated one
// dropped, and the finalize frame re-pinned to the live PR.
func TestReviewRefresh_RemapsAndDrops(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	files := `[` +
		`{"filename":"a.go","status":"modified","patch":"@@ -1,5 +1,7 @@\n+newtop1\n+newtop2\n line1\n added2\n added3\n line4\n line5"},` +
		`{"filename":"b.go","status":"modified","patch":"@@ -1,3 +1,3 @@\n bline1\n-bline2\n+bline2-changed\n bline3"}` +
		`]`
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7,
			"base":   map[string]any{"ref": "main", "sha": "base1"},
			"head":   map[string]any{"sha": "headB"},
		})
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/compare/{rng}", func(w http.ResponseWriter, r *http.Request) {
		if rng := r.PathValue("rng"); rng != "finA...headB" {
			t.Errorf("compare range = %q, want finA...headB", rng)
		}
		_, _ = io.WriteString(w, `{"ahead_by":2,"files":`+files+`}`)
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rref", "acme", "api", 7, "COMMENT")
	art := getDeliverable(t, srv, artID)
	d, _ := domain.ParseReviewDeliverableDetails(art.DetailsJSON)
	d.FinalizedHeadSHA, d.FinalizedBaseSHA = "finA", "base0"
	la, lb := 3, 2
	d.StagedComments = []domain.ReviewDeliverableComment{
		{ID: "c_a", Path: "a.go", Line: &la, Body: "shifts", CommitSHA: "finA"},
		{ID: "c_b", Path: "b.go", Line: &lb, Body: "outdated", CommitSHA: "finA"},
	}
	art.DetailsJSON = domain.MarshalReviewDeliverableDetails(d)
	if _, err := sqlitestore.New(srv.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(srv.db).Orgs, runmode.LocalDefaultOrgID, *art)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/review/refresh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Moved   int    `json:"moved"`
		Dropped int    `json:"dropped"`
		Head    string `json:"head"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Moved != 1 || out.Dropped != 1 || out.Head != "headB" {
		t.Errorf("refresh summary = %+v, want moved 1 / dropped 1 / head headB", out)
	}

	d2, _ := domain.ParseReviewDeliverableDetails(getDeliverable(t, srv, artID).DetailsJSON)
	if len(d2.StagedComments) != 1 || d2.StagedComments[0].ID != "c_a" {
		t.Fatalf("staged comments = %+v, want only the survivor c_a", d2.StagedComments)
	}
	if c := d2.StagedComments[0]; c.Line == nil || *c.Line != 5 || c.CommitSHA != "headB" {
		t.Errorf("survivor = %+v (line=%v), want remapped to line 5, re-anchored to headB", c, c.Line)
	}
	if d2.FinalizedHeadSHA != "headB" || d2.FinalizedBaseSHA != "base1" {
		t.Errorf("finalize frame = %q...%q, want base1...headB (re-pinned to live)", d2.FinalizedBaseSHA, d2.FinalizedHeadSHA)
	}
}

// refreshOutdatedStub serves a PR whose live head is headB and a compare from
// finA that changes the only line b.go carried — so a comment anchored there at
// finA goes outdated on refresh — plus the review POST, recorded into submitBody
// so a follow-up approve's payload can be asserted.
func refreshOutdatedStub(t *testing.T, srv *Server, submitBody *map[string]any) {
	t.Helper()
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7,
			"base":   map[string]any{"ref": "main", "sha": "base1"},
			"head":   map[string]any{"sha": "headB"},
		})
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/compare/{rng}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ahead_by":1,"files":[`+
			`{"filename":"b.go","status":"modified","patch":"@@ -1,3 +1,3 @@\n bline1\n-bline2\n+bline2-changed\n bline3"}`+
			`]}`)
	})
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(submitBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 12345})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())
}

// seedOutdatedOnlyReview stages a finalized review at finA whose single inline
// comment sits on the b.go line refreshOutdatedStub rewrites, with the given
// summary body.
func seedOutdatedOnlyReview(t *testing.T, srv *Server, suffix, body string) string {
	t.Helper()
	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, suffix, "acme", "api", 7, "COMMENT")
	art := getDeliverable(t, srv, artID)
	d, _ := domain.ParseReviewDeliverableDetails(art.DetailsJSON)
	d.ReviewBody = body
	d.FinalizedHeadSHA, d.FinalizedBaseSHA = "finA", "base0"
	lb := 2
	d.StagedComments = []domain.ReviewDeliverableComment{
		{ID: "c_b", Path: "b.go", Line: &lb, Body: "outdated", CommitSHA: "finA"},
	}
	art.DetailsJSON = domain.MarshalReviewDeliverableDetails(d)
	if _, err := sqlitestore.New(srv.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(srv.db).Orgs, runmode.LocalDefaultOrgID, *art)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return artID
}

// TestReviewRefresh_DropsLastCommentThenSubmitsUnpinned: a Refresh that drops
// every inline comment leaves no position for a commit pin to protect, so the
// approve sends no commit_id and GitHub pins the live head, which cannot be stale.
func TestReviewRefresh_DropsLastCommentThenSubmitsUnpinned(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var submitBody map[string]any
	refreshOutdatedStub(t, srv, &submitBody)
	artID := seedOutdatedOnlyReview(t, srv, "rdropall", "## Review\nsummary only")

	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/review/refresh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	d, _ := domain.ParseReviewDeliverableDetails(getDeliverable(t, srv, artID).DetailsJSON)
	if len(d.StagedComments) != 0 || d.FinalizedHeadSHA != "headB" {
		t.Fatalf("after refresh: comments=%+v head=%q, want none / headB", d.StagedComments, d.FinalizedHeadSHA)
	}

	rec = doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if submitBody == nil {
		t.Fatal("approve must call SubmitReview")
	}
	if pin, present := submitBody["commit_id"]; present {
		t.Errorf("submit carried commit_id=%v, want the key absent so GitHub pins the live head", pin)
	}
	if submitBody["event"] != "COMMENT" {
		t.Errorf("submit event = %v, want COMMENT", submitBody["event"])
	}
}

// TestReviewRefresh_RefusesToEmptyBodylessReview pins the guard the drop needs:
// a COMMENT review with no summary whose only inline comment went outdated would
// refresh into a draft with nothing to submit, so the refresh 409s and persists
// nothing — the human adds a summary and refreshes again.
func TestReviewRefresh_RefusesToEmptyBodylessReview(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var submitBody map[string]any
	refreshOutdatedStub(t, srv, &submitBody)
	artID := seedOutdatedOnlyReview(t, srv, "rnobody", "")

	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/review/refresh", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("refresh = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	d, _ := domain.ParseReviewDeliverableDetails(getDeliverable(t, srv, artID).DetailsJSON)
	if len(d.StagedComments) != 1 || d.FinalizedHeadSHA != "finA" {
		t.Errorf("refused refresh persisted: comments=%+v head=%q, want the draft untouched at finA", d.StagedComments, d.FinalizedHeadSHA)
	}
}

// approveDriftStub serves a PR whose live head is liveHead, one compare range
// (any other range fails the test; "" means none is expected), and the review
// POST recorded into submitBody.
func approveDriftStub(t *testing.T, srv *Server, liveHead, compareRange, compareFiles string, submitBody *map[string]any) {
	t.Helper()
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7,
			"base":   map[string]any{"ref": "main", "sha": "base1"},
			"head":   map[string]any{"sha": liveHead},
		})
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/compare/{rng}", func(w http.ResponseWriter, r *http.Request) {
		if rng := r.PathValue("rng"); compareFiles == "" || rng != compareRange {
			t.Errorf("unexpected compare request for range %q (want %q)", rng, compareRange)
			http.Error(w, "no such compare", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"ahead_by":1,"files":`+compareFiles+`}`)
	})
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(submitBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 12345})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())
}

// TestReviewApprove_ShiftedSinceRefreshPinsLiveHead: when the PR moved after the
// review's frame with a pure shift on the commented line, approve remaps the
// comment, pins the submit to the live head, and stamps that frame on the row.
func TestReviewApprove_ShiftedSinceRefreshPinsLiveHead(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var submitBody map[string]any
	approveDriftStub(t, srv, "headB", "headA...headB",
		`[{"filename":"a.go","status":"modified","patch":"@@ -1,5 +1,7 @@\n+newtop1\n+newtop2\n line1\n line2\n line3\n line4\n line5"}]`,
		&submitBody)
	artID := seedFinalizedReviewAt(t, srv, "rshift", 7, "headA", 3)

	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if submitBody["commit_id"] != "headB" {
		t.Errorf("submit commit_id = %v, want headB (the live head the comment was remapped to)", submitBody["commit_id"])
	}
	comments, _ := submitBody["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("submit comments = %v, want the one remapped comment", submitBody["comments"])
	}
	if c, _ := comments[0].(map[string]any); c["line"] != float64(5) {
		t.Errorf("submit comment line = %v, want 5 (3 shifted by two added lines)", c["line"])
	}
	d, _ := domain.ParseReviewDeliverableDetails(getDeliverable(t, srv, artID).DetailsJSON)
	if d.FinalizedHeadSHA != "headB" || len(d.StagedComments) != 1 || d.StagedComments[0].CommitSHA != "headB" {
		t.Errorf("stamped frame = head %q comments %+v, want the row to describe the headB frame it posted", d.FinalizedHeadSHA, d.StagedComments)
	}
}

// TestReviewApprove_OutdatedSinceRefreshRefuses: when the commented line itself
// changed, approve answers 409, posts nothing, releases the claim and leaves the
// draft on its frame; dropping the comment is the human's call through Refresh.
func TestReviewApprove_OutdatedSinceRefreshRefuses(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var submitBody map[string]any
	approveDriftStub(t, srv, "headB", "headA...headB",
		`[{"filename":"a.go","status":"modified","patch":"@@ -1,5 +1,5 @@\n line1\n line2\n-line3\n+line3-changed\n line4\n line5"}]`,
		&submitBody)
	artID := seedFinalizedReviewAt(t, srv, "rstale", 7, "headA", 3)

	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if submitBody != nil {
		t.Errorf("GitHub review POST was made with %v, want none", submitBody)
	}
	got := getDeliverable(t, srv, artID)
	if got.State != domain.DeliverableStateReviewPending {
		t.Errorf("state = %q, want pending (claim released)", got.State)
	}
	d, _ := domain.ParseReviewDeliverableDetails(got.DetailsJSON)
	if d.FinalizedHeadSHA != "headA" || len(d.StagedComments) != 1 || d.StagedComments[0].CommitSHA != "headA" {
		t.Errorf("refused approve moved the draft: head %q comments %+v, want untouched at headA", d.FinalizedHeadSHA, d.StagedComments)
	}
}

// TestReviewApprove_CurrentHeadSkipsCompare pins the fast path: the live head is
// the review's own frame, so the approve makes no compare call and pins that
// head.
func TestReviewApprove_CurrentHeadSkipsCompare(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var submitBody map[string]any
	approveDriftStub(t, srv, "headA", "", "", &submitBody)
	artID := seedFinalizedReviewAt(t, srv, "rcurr", 7, "headA", 3)

	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if submitBody["commit_id"] != "headA" {
		t.Errorf("submit commit_id = %v, want headA", submitBody["commit_id"])
	}
}

// TestReviewRefresh_Guards pins the state guards: an unfinalized draft and a
// terminal (submitted) review both 409 and make no GitHub call.
func TestReviewRefresh_Guards(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		t.Error("a 409 refresh must not touch GitHub")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	// Unfinalized: clear the ready sentinel.
	unfin := seedFinalizedReviewAt(t, srv, "runfin", 7, "headA", 3)
	art := getDeliverable(t, srv, unfin)
	d, _ := domain.ParseReviewDeliverableDetails(art.DetailsJSON)
	d.ReviewEvent = ""
	art.DetailsJSON = domain.MarshalReviewDeliverableDetails(d)
	if _, err := sqlitestore.New(srv.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(srv.db).Orgs, runmode.LocalDefaultOrgID, *art)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+unfin+"/review/refresh", nil); rec.Code != http.StatusConflict {
		t.Errorf("refresh of unfinalized review = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// Submitted (terminal).
	sub := seedFinalizedReviewAt(t, srv, "rsub", 7, "headA", 3)
	execSQL(t, srv.db, `UPDATE deliverables SET state = ? WHERE id = ?`, domain.DeliverableStateReviewSubmitted, sub)
	if rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+sub+"/review/refresh", nil); rec.Code != http.StatusConflict {
		t.Errorf("refresh of submitted review = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestReviewFreshness_Verdicts pins each comment verdict against the live head:
// current when the PR has not advanced (no compare call), moved with its
// remapped line when lines landed above it, and outdated when its line changed.
func TestReviewFreshness_Verdicts(t *testing.T) {
	for _, tc := range []struct {
		name, liveHead, compareRange, files string
		aheadBy, wantMapped                 int
	}{
		{name: "current", liveHead: "headA"},
		{name: "moved", liveHead: "headB", compareRange: "headA...headB", aheadBy: 2, wantMapped: 5,
			files: `[{"filename":"a.go","status":"modified","patch":"@@ -1,5 +1,7 @@\n+newtop1\n+newtop2\n line1\n added2\n added3\n line4\n line5"}]`},
		{name: "outdated", liveHead: "headB", compareRange: "headA...headB", aheadBy: 1,
			files: `[{"filename":"a.go","status":"modified","patch":"@@ -1,5 +1,4 @@\n line1\n added2\n-added3\n line4\n line5"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			srv := newTestServer(t)
			freshnessAppStub(t, srv, tc.liveHead, tc.aheadBy, tc.compareRange, tc.files)
			artID := seedFinalizedReviewAt(t, srv, "r"+tc.name, 7, "headA", 3)

			out := getReviewFreshness(t, srv, artID)
			if out.CommitsSinceFinalize == nil || *out.CommitsSinceFinalize != tc.aheadBy {
				t.Errorf("commits_since_finalize = %v, want a non-nil %d", out.CommitsSinceFinalize, tc.aheadBy)
			}
			if len(out.Comments) != 1 || out.Comments[0].Freshness != tc.name {
				t.Fatalf("comment freshness = %+v, want one %q", out.Comments, tc.name)
			}
			if out.Comments[0].MappedLine != tc.wantMapped {
				t.Errorf("mapped_line = %d, want %d", out.Comments[0].MappedLine, tc.wantMapped)
			}
		})
	}
}

// TestReviewFreshness_NoAnchorComment: a staged comment with no line anchor
// stays "unknown" with no remapped line. The PR has not advanced, so the verdict
// comes from the missing anchor, not a GitHub failure.
func TestReviewFreshness_NoAnchorComment(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	freshnessAppStub(t, srv, "headA", 0, "", "") // no drift; compare must not be hit
	artID := seedFinalizedReviewAt(t, srv, "rnoanchor", 7, "headA", 3)
	// Null out the staged comment's line anchor.
	art := getDeliverable(t, srv, artID)
	d, _ := domain.ParseReviewDeliverableDetails(art.DetailsJSON)
	d.StagedComments[0].Line = nil
	art.DetailsJSON = domain.MarshalReviewDeliverableDetails(d)
	if _, err := sqlitestore.New(srv.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(srv.db).Orgs, runmode.LocalDefaultOrgID, *art)); err != nil {
		t.Fatalf("null line: %v", err)
	}

	out := getReviewFreshness(t, srv, artID)
	if len(out.Comments) != 1 || out.Comments[0].Freshness != "unknown" {
		t.Fatalf("comment freshness = %+v, want one 'unknown' (no line anchor)", out.Comments)
	}
	if out.Comments[0].MappedLine != 0 {
		t.Errorf("mapped_line = %d, want 0 for an unanchored comment", out.Comments[0].MappedLine)
	}
}

// TestReviewFreshness_DegradesWhenHeadUnavailable: when the live PR head cannot
// be fetched, the overlay still answers 200 with the staged comment marked
// "unknown" and a nil count, so the review stays readable.
func TestReviewFreshness_DegradesWhenHeadUnavailable(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mux := newAppAPIMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())
	artID := seedFinalizedReviewAt(t, srv, "rdeg", 7, "headA", 3)

	out := getReviewFreshness(t, srv, artID)
	if out.CommitsSinceFinalize != nil {
		t.Errorf("commits_since_finalize = %v, want nil when the live head is unavailable", *out.CommitsSinceFinalize)
	}
	if len(out.Comments) != 1 || out.Comments[0].Freshness != "unknown" {
		t.Fatalf("comment freshness = %+v, want one 'unknown'", out.Comments)
	}
}

// TestReviewFreshness_DegradesWhenGitHubUnconfigured pins that a review with no
// GitHub credentials at all still renders: the overlay returns 200, freshness
// "unknown", nil count — no 503.
func TestReviewFreshness_DegradesWhenGitHubUnconfigured(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	// No seedApp: ClientForRepo returns ErrNoGitHubCredentials.
	artID := seedFinalizedReviewAt(t, srv, "rnocreds", 7, "headA", 3)

	out := getReviewFreshness(t, srv, artID)
	if out.CommitsSinceFinalize != nil {
		t.Errorf("commits_since_finalize = %v, want nil with no GitHub creds", *out.CommitsSinceFinalize)
	}
	if len(out.Comments) != 1 || out.Comments[0].Freshness != "unknown" {
		t.Fatalf("comment freshness = %+v, want one 'unknown'", out.Comments)
	}
}
