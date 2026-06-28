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

// reviewDiffServer is a fake GitHub serving just the PR diff GetPRDiff fetches —
// a.go with new-side lines 1..5 commentable — so the add-review-comment range
// validation has something live to check against. Any other request gets {}.
func reviewDiffServer(t *testing.T) *httptest.Server {
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
		_, _ = io.WriteString(w, `{}`)
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
	srv := reviewDiffServer(t)
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
	srv := reviewDiffServer(t)
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
