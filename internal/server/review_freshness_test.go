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

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// reviewFreshnessOut is the slice of reviewArtifactJSON the TFAC-500 freshness
// tests assert on.
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

// seedFinalizedReviewAt seeds a finalized review-draft artifact (TFAC-494) with a
// stamped finalize-time head (TFAC-500) and one inline comment on a.go anchored to
// that head at the given line. Returns the artifact id. The owner must be "acme"
// so the App resolver (acmeInstall) matches.
func seedFinalizedReviewAt(t *testing.T, s *Server, suffix string, number int, finalizeHead string, line int) string {
	t.Helper()
	artID, _, _ := seedReviewArtifactWithRun(t, s, suffix, "acme", "api", number, "COMMENT")
	art := getArtifact(t, s, artID)
	d, _ := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	d.FinalizedHeadSHA = finalizeHead
	d.FinalizedBaseSHA = "base0" // the finalize-time base tip (for the finalize-frame diff)
	l := line
	d.StagedComments = []domain.ReviewArtifactComment{
		{ID: "c_1", Path: "a.go", Line: &l, Body: domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "nit: rename", CommitSHA: finalizeHead},
	}
	art.DetailsJSON = domain.MarshalReviewArtifactDetails(d)
	if _, err := sqlitestore.New(s.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, *art); err != nil {
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

// getReviewFreshness GETs the review artifact and decodes the freshness slice.
func getReviewFreshness(t *testing.T, s *Server, artID string) reviewFreshnessOut {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, "/api/artifacts/"+artID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out reviewFreshnessOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// TestArtifactDiff_ReviewServesFinalizeFrame pins that a review with a stored
// finalize frame renders FinalizedBaseSHA...FinalizedHeadSHA (the finalize-time
// PR diff) via the compare endpoint, and does NOT fetch the live PR diff (TFAC-500).
func TestArtifactDiff_ReviewServesFinalizeFrame(t *testing.T) {
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

	rec := doJSON(t, srv, http.MethodGet, "/api/artifacts/"+artID+"/diff", nil)
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

// TestArtifactDiff_FramelessReviewServesLiveDiff pins the fallback: a review with
// no stored finalize frame (comment-less or pre-TFAC-500) renders the live PR diff,
// not a compare.
func TestArtifactDiff_FramelessReviewServesLiveDiff(t *testing.T) {
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
	// seedReviewArtifactWithRun records no finalize frame → frameless.
	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rlive", "acme", "api", 7, "COMMENT")

	rec := doJSON(t, srv, http.MethodGet, "/api/artifacts/"+artID+"/diff", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("diff = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !liveHit {
		t.Error("frameless review must fetch the live PR diff")
	}
}

// TestReviewRefresh_RemapsAndDrops pins the Refresh action (TFAC-500): a finalized
// review with one comment that shifts and one that goes outdated is reconciled to
// the live head — the survivor is remapped + re-anchored, the outdated one is
// dropped, and the finalize frame (base+head) is re-pinned to the live PR.
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

	artID, _, _ := seedReviewArtifactWithRun(t, srv, "rref", "acme", "api", 7, "COMMENT")
	art := getArtifact(t, srv, artID)
	d, _ := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	d.FinalizedHeadSHA, d.FinalizedBaseSHA = "finA", "base0"
	la, lb := 3, 2
	d.StagedComments = []domain.ReviewArtifactComment{
		{ID: "c_a", Path: "a.go", Line: &la, Body: "shifts", CommitSHA: "finA"},
		{ID: "c_b", Path: "b.go", Line: &lb, Body: "outdated", CommitSHA: "finA"},
	}
	art.DetailsJSON = domain.MarshalReviewArtifactDetails(d)
	if _, err := sqlitestore.New(srv.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, *art); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+artID+"/review/refresh", nil)
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

	d2, _ := domain.ParseReviewArtifactDetails(getArtifact(t, srv, artID).DetailsJSON)
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
	art := getArtifact(t, srv, unfin)
	d, _ := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	d.ReviewEvent = ""
	art.DetailsJSON = domain.MarshalReviewArtifactDetails(d)
	if _, err := sqlitestore.New(srv.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, *art); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+unfin+"/review/refresh", nil); rec.Code != http.StatusConflict {
		t.Errorf("refresh of unfinalized review = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// Submitted (terminal).
	sub := seedFinalizedReviewAt(t, srv, "rsub", 7, "headA", 3)
	execSQL(t, srv.db, `UPDATE artifacts SET state = ? WHERE id = ?`, domain.ArtifactStateReviewSubmitted, sub)
	if rec := doJSON(t, srv, http.MethodPost, "/api/artifacts/"+sub+"/review/refresh", nil); rec.Code != http.StatusConflict {
		t.Errorf("refresh of submitted review = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestReviewFreshness_Current pins the no-drift fast path: the live PR head equals
// the finalize head, so the comment is "current", the count is a non-nil 0, and NO
// compare call is made (the stub fails on any compare request).
func TestReviewFreshness_Current(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	freshnessAppStub(t, srv, "headA", 0, "", "") // liveHead == finalizeHead; compare must not be hit
	artID := seedFinalizedReviewAt(t, srv, "rfresh", 7, "headA", 3)

	out := getReviewFreshness(t, srv, artID)
	if out.CommitsSinceFinalize == nil || *out.CommitsSinceFinalize != 0 {
		t.Errorf("commits_since_finalize = %v, want a non-nil 0 (PR not advanced)", out.CommitsSinceFinalize)
	}
	if len(out.Comments) != 1 || out.Comments[0].Freshness != "current" {
		t.Fatalf("comment freshness = %+v, want one 'current'", out.Comments)
	}
}

// TestReviewFreshness_Moved pins the shift path: the PR advanced (headA → headB)
// inserting two lines above the comment, so the same code now sits two lines down.
// The comment is "moved", carries its remapped line, and the count reflects the
// commits ahead.
func TestReviewFreshness_Moved(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	files := `[{"filename":"a.go","status":"modified","patch":"@@ -1,5 +1,7 @@\n+newtop1\n+newtop2\n line1\n added2\n added3\n line4\n line5"}]`
	freshnessAppStub(t, srv, "headB", 2, "headA...headB", files)
	artID := seedFinalizedReviewAt(t, srv, "rmoved", 7, "headA", 3)

	out := getReviewFreshness(t, srv, artID)
	if out.CommitsSinceFinalize == nil || *out.CommitsSinceFinalize != 2 {
		t.Errorf("commits_since_finalize = %v, want 2", out.CommitsSinceFinalize)
	}
	if len(out.Comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(out.Comments))
	}
	c := out.Comments[0]
	if c.Freshness != "moved" {
		t.Errorf("freshness = %q, want moved", c.Freshness)
	}
	if c.MappedLine != 5 {
		t.Errorf("mapped_line = %d, want 5 (shifted +2)", c.MappedLine)
	}
}

// TestReviewFreshness_Outdated pins the outdated path: the anchored line was
// deleted between headA and headB, so the comment no longer points at the same
// code. Freshness is "outdated" with no remapped line; the count still resolves.
func TestReviewFreshness_Outdated(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	files := `[{"filename":"a.go","status":"modified","patch":"@@ -1,5 +1,4 @@\n line1\n added2\n-added3\n line4\n line5"}]`
	freshnessAppStub(t, srv, "headB", 1, "headA...headB", files)
	artID := seedFinalizedReviewAt(t, srv, "rold", 7, "headA", 3)

	out := getReviewFreshness(t, srv, artID)
	if out.CommitsSinceFinalize == nil || *out.CommitsSinceFinalize != 1 {
		t.Errorf("commits_since_finalize = %v, want 1", out.CommitsSinceFinalize)
	}
	if len(out.Comments) != 1 || out.Comments[0].Freshness != "outdated" {
		t.Fatalf("comment freshness = %+v, want one 'outdated'", out.Comments)
	}
	if out.Comments[0].MappedLine != 0 {
		t.Errorf("mapped_line = %d, want 0 for outdated", out.Comments[0].MappedLine)
	}
}

// TestReviewFreshness_NoAnchorComment pins that a staged comment with no line
// anchor (line == nil) stays "unknown" with no remapped line — it can't be
// forward-mapped. The PR hasn't advanced (live head == finalize head), so the
// count is 0 and no compare is requested; the unknown verdict is purely the
// missing-anchor branch, not a GitHub failure.
func TestReviewFreshness_NoAnchorComment(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	freshnessAppStub(t, srv, "headA", 0, "", "") // no drift; compare must not be hit
	artID := seedFinalizedReviewAt(t, srv, "rnoanchor", 7, "headA", 3)
	// Null out the staged comment's line anchor.
	art := getArtifact(t, srv, artID)
	d, _ := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	d.StagedComments[0].Line = nil
	art.DetailsJSON = domain.MarshalReviewArtifactDetails(d)
	if _, err := sqlitestore.New(srv.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, *art); err != nil {
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

// TestReviewFreshness_DegradesWhenHeadUnavailable pins the graceful-degradation
// contract: when the live PR head can't be fetched (GitHub 500), the overlay still
// returns 200 with the staged comment, freshness "unknown", and a nil count — the
// human can still read and act on the review.
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
