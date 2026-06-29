package agenthost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// reviewFreshnessServer is a fake GitHub for the finalize-review freshness gate
// (TFAC-499). It serves two kinds of read:
//
//   - the PR object (any non-diff GET) → base.ref "main" + head.sha = headSHA, so
//     reconcile learns the current head and add-review-comment learns the base ref;
//   - a compare diff (Accept: diff) → routed by the "<base>...<head>" range parsed
//     out of /compare/, looked up in diffs. An unrequested range is a test bug, so
//     it fails loudly rather than silently returning an empty (= "unchanged") diff.
//
// The add-review-comment validation hits compare/main...<anchor>; the finalize
// reconcile hits compare/<anchor>...<head>. Distinct ranges, so one map drives both.
func reviewFreshnessServer(t *testing.T, headSHA string, diffs map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "diff") {
			rng := ""
			if i := strings.Index(r.URL.Path, "/compare/"); i >= 0 {
				rng = r.URL.Path[i+len("/compare/"):]
			}
			diff, ok := diffs[rng]
			if !ok {
				t.Errorf("unexpected compare diff request for range %q (path %s)", rng, r.URL.Path)
				http.Error(w, "no such compare", http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, diff)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"number":7,"base":{"ref":"main"},"head":{"sha":"`+headSHA+`"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// validationDiffAGo is the base...anchor diff add-review-comment validates against:
// a.go with new-side lines 1..5 commentable, so a comment on line 3 stages.
const validationDiffAGo = "diff --git a/a.go b/a.go\n" +
	"--- a/a.go\n+++ b/a.go\n" +
	"@@ -1,3 +1,5 @@\n line1\n+added2\n+added3\n line4\n line5\n"

// TestLocalClient_FinalizeReview_Freshness_CleanAnchorAtHead pins the clean path:
// a comment anchored at the PR's current head needs no forward map, so finalize
// passes with the comment untouched and makes NO compare call (the server fails on
// any unexpected range — only the staging validation range is registered).
func TestLocalClient_FinalizeReview_Freshness_CleanAnchorAtHead(t *testing.T) {
	srv := reviewFreshnessServer(t, "head1", map[string]string{
		"main...head1": validationDiffAGo,
	})
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "head1", nil)
	if err != nil {
		t.Fatalf("start review: %v", err)
	}
	cid, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit", 3, nil, "head1")
	if err != nil {
		t.Fatalf("add comment: %v", err)
	}

	if err := client.FinalizeReviewDraft(context.Background(), handle, "COMMENT", "## body"); err != nil {
		t.Fatalf("finalize (clean) must pass: %v", err)
	}

	d, _ := domain.ParseReviewArtifactDetails(listRunArtifacts(t, stores, info.RunID)[0].DetailsJSON)
	if d.ReviewEvent != "COMMENT" {
		t.Errorf("ready sentinel not set: %+v", d)
	}
	c := byID(d.StagedComments, cid)
	if c.Line == nil || *c.Line != 3 {
		t.Errorf("clean comment line = %+v, want 3 (unchanged)", c.Line)
	}
	if c.CommitSHA != "head1" {
		t.Errorf("clean comment CommitSHA = %q, want head1", c.CommitSHA)
	}
}

// TestLocalClient_FinalizeReview_Freshness_ShiftAutoRemaps pins the pure-shift
// path: the PR advanced (head1 → head2) inserting two lines above the comment, so
// its content is identical but its line moved. Finalize auto-remaps the stored
// line silently and re-anchors it to the current head — no agent action, no error.
func TestLocalClient_FinalizeReview_Freshness_ShiftAutoRemaps(t *testing.T) {
	shift := "diff --git a/a.go b/a.go\n" +
		"--- a/a.go\n+++ b/a.go\n" +
		"@@ -1,5 +1,7 @@\n+newtop1\n+newtop2\n line1\n added2\n added3\n line4\n line5\n"
	srv := reviewFreshnessServer(t, "head2", map[string]string{
		"main...head1":  validationDiffAGo,
		"head1...head2": shift,
	})
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "head1", nil)
	if err != nil {
		t.Fatalf("start review: %v", err)
	}
	cid, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit", 3, nil, "head1")
	if err != nil {
		t.Fatalf("add comment: %v", err)
	}

	if err := client.FinalizeReviewDraft(context.Background(), handle, "COMMENT", "## body"); err != nil {
		t.Fatalf("finalize (pure shift) must pass: %v", err)
	}

	d, _ := domain.ParseReviewArtifactDetails(listRunArtifacts(t, stores, info.RunID)[0].DetailsJSON)
	c := byID(d.StagedComments, cid)
	if c.Line == nil || *c.Line != 5 {
		t.Errorf("shifted comment line = %+v, want auto-remapped 5", c.Line)
	}
	if c.CommitSHA != "head2" {
		t.Errorf("shifted comment CommitSHA = %q, want re-anchored head2", c.CommitSHA)
	}
	if c.Path != "a.go" {
		t.Errorf("shifted comment path = %q, want a.go (no rename)", c.Path)
	}
}

// TestLocalClient_FinalizeReview_Freshness_OutdatedFails pins the outdated path:
// the anchored line was edited/deleted between head1 and head2, so finalize fails
// with an actionable, per-comment error and persists NOTHING — the ready sentinel
// stays unset and the staged comment keeps its original line + anchor, so the agent
// can fix it and retry.
func TestLocalClient_FinalizeReview_Freshness_OutdatedFails(t *testing.T) {
	deleted := "diff --git a/a.go b/a.go\n" +
		"--- a/a.go\n+++ b/a.go\n" +
		"@@ -1,5 +1,4 @@\n line1\n added2\n-added3\n line4\n line5\n"
	srv := reviewFreshnessServer(t, "head2", map[string]string{
		"main...head1":  validationDiffAGo,
		"head1...head2": deleted,
	})
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "head1", nil)
	if err != nil {
		t.Fatalf("start review: %v", err)
	}
	cid, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit: rename added3", 3, nil, "head1")
	if err != nil {
		t.Fatalf("add comment: %v", err)
	}

	err = client.FinalizeReviewDraft(context.Background(), handle, "COMMENT", "## body")
	if err == nil {
		t.Fatal("finalize with an outdated comment must fail")
	}
	msg := err.Error()
	for _, want := range []string{"outdated", "a.go:3", cid, "comment-delete"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q; got: %s", want, msg)
		}
	}

	// Nothing persisted: the ready sentinel stays unset and the comment keeps its
	// original line + anchor, so the agent retries from where it was.
	d, _ := domain.ParseReviewArtifactDetails(listRunArtifacts(t, stores, info.RunID)[0].DetailsJSON)
	if d.ReviewEvent != "" {
		t.Errorf("a failed finalize must NOT arm the ready sentinel, got %q", d.ReviewEvent)
	}
	c := byID(d.StagedComments, cid)
	if c.Line == nil || *c.Line != 3 || c.CommitSHA != "head1" {
		t.Errorf("a failed finalize must not mutate the staged comment, got line=%+v sha=%q", c.Line, c.CommitSHA)
	}
}

// TestLocalClient_FinalizeReview_Freshness_MixedMultiComment pins the mixed case:
// one comment shifts cleanly (would auto-remap) and one is outdated. Any outdated
// comment fails the whole finalize, and the error lists ONLY the outdated one;
// because the gate fails before persisting, the shifted comment's in-memory remap
// is discarded too, so both comments survive unchanged for the retry.
func TestLocalClient_FinalizeReview_Freshness_MixedMultiComment(t *testing.T) {
	// Validation diff for both files (a.go line 3, b.go line 2 commentable).
	validation := validationDiffAGo +
		"diff --git a/b.go b/b.go\n" +
		"--- a/b.go\n+++ b/b.go\n" +
		"@@ -1,1 +1,3 @@\n bline1\n+bline2\n+bline3\n"
	// Reconcile diff head1...head2: a.go shifts (+2 at top → line 3 maps to 5),
	// b.go line 2 is edited (outdated). Both comments share anchor head1, so the
	// reconcile fetches this one compare once.
	reconcile := "diff --git a/a.go b/a.go\n" +
		"--- a/a.go\n+++ b/a.go\n" +
		"@@ -1,5 +1,7 @@\n+newtop1\n+newtop2\n line1\n added2\n added3\n line4\n line5\n" +
		"diff --git a/b.go b/b.go\n" +
		"--- a/b.go\n+++ b/b.go\n" +
		"@@ -1,3 +1,3 @@\n bline1\n-bline2\n+bline2-changed\n bline3\n"
	srv := reviewFreshnessServer(t, "head2", map[string]string{
		"main...head1":  validation,
		"head1...head2": reconcile,
	})
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "head1", nil)
	if err != nil {
		t.Fatalf("start review: %v", err)
	}
	cidA, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "shifts", 3, nil, "head1")
	if err != nil {
		t.Fatalf("add comment a: %v", err)
	}
	cidB, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "b.go", "outdated", 2, nil, "head1")
	if err != nil {
		t.Fatalf("add comment b: %v", err)
	}

	err = client.FinalizeReviewDraft(context.Background(), handle, "COMMENT", "## body")
	if err == nil {
		t.Fatal("finalize must fail when any comment is outdated")
	}
	msg := err.Error()
	if !strings.Contains(msg, "b.go:2") || !strings.Contains(msg, cidB) {
		t.Errorf("error must list the outdated b.go comment; got: %s", msg)
	}
	if strings.Contains(msg, cidA) {
		t.Errorf("error must NOT list the fresh (shifted) a.go comment %s; got: %s", cidA, msg)
	}

	// Nothing persisted: the sentinel is unset and neither comment moved (the a.go
	// remap was in-memory only, discarded with the failing finalize).
	d, _ := domain.ParseReviewArtifactDetails(listRunArtifacts(t, stores, info.RunID)[0].DetailsJSON)
	if d.ReviewEvent != "" {
		t.Errorf("a failed finalize must not arm the ready sentinel, got %q", d.ReviewEvent)
	}
	ca := byID(d.StagedComments, cidA)
	if ca.Line == nil || *ca.Line != 3 || ca.CommitSHA != "head1" {
		t.Errorf("a.go comment must be unchanged after the failed finalize, got line=%+v sha=%q", ca.Line, ca.CommitSHA)
	}
	cb := byID(d.StagedComments, cidB)
	if cb.Line == nil || *cb.Line != 2 || cb.CommitSHA != "head1" {
		t.Errorf("b.go comment must be unchanged after the failed finalize, got line=%+v sha=%q", cb.Line, cb.CommitSHA)
	}
}
