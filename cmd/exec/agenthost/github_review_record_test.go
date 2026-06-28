package agenthost

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// reviewDiffServer is a fake GitHub serving the two reads add-review-comment
// makes: the PR diff (GetPRDiff — a.go with new-side lines 1..5 commentable, for
// the range validation) and the PR object (GetPRBasic — for the head SHA each
// comment anchors to). The head SHA is read live from *headSHA so a test can flip
// it between calls to simulate a force-push mid-review.
func reviewDiffServer(t *testing.T, headSHA *string) *httptest.Server {
	t.Helper()
	const diff = "diff --git a/a.go b/a.go\n" +
		"--- a/a.go\n+++ b/a.go\n" +
		"@@ -1,3 +1,5 @@\n line1\n+added2\n+added3\n line4\n line5\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "diff") {
			_, _ = io.WriteString(w, diff)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"number":7,"head":{"sha":"`+*headSHA+`"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestLocalClient_GithubCreatePendingReview_RecordsLocalDraft pins that
// start-review lands one run-scoped `review` artifact with ZERO GitHub writes:
// state=pending (no ready sentinel), empty ExternalID (no GitHub review until
// approval), the head SHA pinned into details, deduped on
// github:review:owner/repo#<number>:<run_id> — across both write paths. The
// returned handle is the artifact id.
func TestLocalClient_GithubCreatePendingReview_RecordsLocalDraft(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			// No GitHub call should happen during start-review; fail loudly if one does.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected GitHub call during start-review: %s %s", r.Method, r.URL.Path)
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(srv.Close)
			stores, info, client := newGithubRecordingClient(t, srv.URL, eventTriggered)

			handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
			if err != nil {
				t.Fatalf("GithubCreatePendingReview: %v", err)
			}
			if handle == "" {
				t.Fatal("expected a non-empty local review handle")
			}

			arts := listRunArtifacts(t, stores, info.RunID)
			if len(arts) != 1 {
				t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
			}
			a := arts[0]
			if a.ID != handle {
				t.Errorf("handle = %q, want the artifact id %q", handle, a.ID)
			}
			wantDedup := "github:review:octo/repo#7:" + info.RunID
			if a.Kind != domain.ArtifactKindReview || a.State != domain.ArtifactStateReviewPending ||
				a.Target != "octo/repo#7" || a.ExternalID != "" || a.DedupKey != wantDedup {
				t.Errorf("review artifact mismatch: %+v (want dedup %q)", a, wantDedup)
			}
			d, derr := domain.ParseReviewArtifactDetails(a.DetailsJSON)
			if derr != nil {
				t.Fatalf("ParseReviewArtifactDetails: %v", derr)
			}
			if d.HeadSHA != "headsha7" || d.Number != 7 {
				t.Errorf("details mismatch: %+v", d)
			}
			if d.ReviewEvent != "" {
				t.Errorf("fresh review must have no ready sentinel: %+v", d)
			}
		})
	}
}

// TestLocalClient_GithubAddPendingReviewComment_StagesLocally pins that
// add-review-comment validates the range against the live diff and stages the
// comment into details.staged_comments with a local id — no GitHub write.
func TestLocalClient_GithubAddPendingReviewComment_StagesLocally(t *testing.T) {
	head := "commit_v1"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}

	// An in-diff line (3) stages.
	cid, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit: rename", 3, nil)
	if err != nil {
		t.Fatalf("GithubAddPendingReviewComment: %v", err)
	}
	if cid == "" {
		t.Fatal("expected a non-empty staged comment id")
	}

	arts := listRunArtifacts(t, stores, info.RunID)
	d, _ := domain.ParseReviewArtifactDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 1 || d.StagedComments[0].ID != cid {
		t.Fatalf("comment not staged: %+v", d.StagedComments)
	}
	if d.StagedComments[0].Line == nil || *d.StagedComments[0].Line != 3 {
		t.Errorf("staged line = %+v, want 3", d.StagedComments[0].Line)
	}
	// The comment is anchored to the PR head it was validated against.
	if d.StagedComments[0].CommitSHA != "commit_v1" {
		t.Errorf("staged CommitSHA = %q, want commit_v1 (the validated head)", d.StagedComments[0].CommitSHA)
	}

	// An out-of-diff line (99) is rejected before staging.
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "bad", 99, nil); err == nil {
		t.Error("expected an out-of-diff line to be rejected")
	}
	arts = listRunArtifacts(t, stores, info.RunID)
	d, _ = domain.ParseReviewArtifactDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 1 {
		t.Errorf("a rejected comment must not be staged, got %d", len(d.StagedComments))
	}
}

// TestLocalClient_FinalizeReviewDraft pins submit-review's host behavior: no
// GitHub call, the agent's draft (body + event + the locally staged comments)
// snapshotted into the artifact, the ready sentinel set, and the TFAC-358
// anti-double-submit guard.
func TestLocalClient_FinalizeReviewDraft(t *testing.T) {
	head := "commit_v1"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "headsha7", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "nit: rename", 3, nil); err != nil {
		t.Fatalf("GithubAddPendingReviewComment: %v", err)
	}

	if err := client.FinalizeReviewDraft(context.Background(), handle, "COMMENT", "## Review body"); err != nil {
		t.Fatalf("FinalizeReviewDraft: %v", err)
	}

	arts := listRunArtifacts(t, stores, info.RunID)
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(arts))
	}
	if arts[0].State != domain.ArtifactStateReviewPending {
		t.Errorf("artifact must stay pending until approval, got %q", arts[0].State)
	}
	d, _ := domain.ParseReviewArtifactDetails(arts[0].DetailsJSON)
	if d.ReviewEvent != "COMMENT" || d.ReviewBody != "## Review body" {
		t.Errorf("ready sentinel / staged body not set: %+v", d)
	}
	if len(d.Proposed.Comments) != 1 || d.Proposed.Comments[0].Path != "a.go" {
		t.Errorf("proposed comments not snapshotted from the staged set: %+v", d.Proposed)
	}

	// Anti-double-submit (TFAC-358): a second finalize hard-errors.
	err = client.FinalizeReviewDraft(context.Background(), handle, "COMMENT", "## again")
	if !errors.Is(err, ErrReviewAlreadyFinalized) {
		t.Errorf("second submit-review = %v, want ErrReviewAlreadyFinalized", err)
	}
}

// TestLocalClient_MultipleReviewDrafts_ResolveByHandle pins that when one run
// holds several review drafts (run-scoped dedup, one per PR — TFAC-494),
// add-review-comment and submit-review act on the draft named by the handle, not
// just the first pending review. A naive "first pending review" lookup would
// stage/finalize against the wrong PR's draft.
func TestLocalClient_MultipleReviewDrafts_ResolveByHandle(t *testing.T) {
	head := "commit_v1"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	// Two drafts on different PRs in the same run.
	h7, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "sha7", nil)
	if err != nil {
		t.Fatalf("create review 7: %v", err)
	}
	h8, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 8, "sha8", nil)
	if err != nil {
		t.Fatalf("create review 8: %v", err)
	}
	if h7 == h8 {
		t.Fatalf("two drafts must get distinct handles, both = %q", h7)
	}

	// Stage a comment on the SECOND draft's handle; it must land on PR 8's draft.
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", h8, "a.go", "on 8", 3, nil); err != nil {
		t.Fatalf("add comment to review 8: %v", err)
	}
	// Finalize the SECOND draft by its handle.
	if err := client.FinalizeReviewDraft(context.Background(), h8, "COMMENT", "## review 8"); err != nil {
		t.Fatalf("finalize review 8: %v", err)
	}

	arts := listRunArtifacts(t, stores, info.RunID)
	byID := map[string]domain.ReviewArtifactDetails{}
	for _, a := range arts {
		d, _ := domain.ParseReviewArtifactDetails(a.DetailsJSON)
		byID[a.ID] = d
	}
	// PR 8's draft got the comment + the ready sentinel.
	d8 := byID[h8]
	if len(d8.StagedComments) != 1 || d8.ReviewEvent != "COMMENT" || d8.ReviewBody != "## review 8" {
		t.Errorf("review 8 draft: %+v, want one staged comment + finalized COMMENT", d8)
	}
	// PR 7's draft is untouched — no comment, no sentinel.
	d7 := byID[h7]
	if len(d7.StagedComments) != 0 || d7.ReviewEvent != "" {
		t.Errorf("review 7 draft must be untouched, got %+v", d7)
	}

	// An unknown handle is rejected (not silently routed to the first draft).
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", "nope", "a.go", "x", 3, nil); err == nil {
		t.Error("an unknown handle must be rejected")
	}
}

// TestLocalClient_PerCommentHeadSHA_CapturedAcrossForcePush pins that each staged
// comment records the PR head it was validated against — so a force-push between
// two adds yields two comments anchored to different commits (the signal the
// submit-time agreement check refuses on, instead of mis-anchoring).
func TestLocalClient_PerCommentHeadSHA_CapturedAcrossForcePush(t *testing.T) {
	head := "commit_v1"
	srv := reviewDiffServer(t, &head)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	handle, err := client.GithubCreatePendingReview(context.Background(), "octo", "repo", 7, "commit_v1", nil)
	if err != nil {
		t.Fatalf("create review: %v", err)
	}
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "first", 3, nil); err != nil {
		t.Fatalf("add comment 1: %v", err)
	}
	// The PR is force-pushed: the live head advances.
	head = "commit_v2"
	if _, err := client.GithubAddPendingReviewComment(context.Background(), "octo", "repo", handle, "a.go", "second", 4, nil); err != nil {
		t.Fatalf("add comment 2: %v", err)
	}

	arts := listRunArtifacts(t, stores, info.RunID)
	d, _ := domain.ParseReviewArtifactDetails(arts[0].DetailsJSON)
	if len(d.StagedComments) != 2 {
		t.Fatalf("want 2 staged comments, got %d", len(d.StagedComments))
	}
	if d.StagedComments[0].CommitSHA != "commit_v1" || d.StagedComments[1].CommitSHA != "commit_v2" {
		t.Errorf("per-comment anchors = %q,%q, want commit_v1,commit_v2",
			d.StagedComments[0].CommitSHA, d.StagedComments[1].CommitSHA)
	}
}
