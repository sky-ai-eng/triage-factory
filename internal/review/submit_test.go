package review

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// recordingSubmitter captures the one GitHub call SubmitStaged makes.
type recordingSubmitter struct {
	calls    int
	owner    string
	repo     string
	number   int
	commitID string
	event    string
	body     string
	comments []ghclient.SubmitReviewComment
	err      error
}

func (s *recordingSubmitter) SubmitReview(_ context.Context, owner, repo string, number int, commitSHA, event, body string, comments []ghclient.SubmitReviewComment) (int, string, error) {
	s.calls++
	s.owner, s.repo, s.number = owner, repo, number
	s.commitID, s.event, s.body, s.comments = commitSHA, event, body, comments
	if s.err != nil {
		return 0, event, s.err
	}
	return 4242, event, nil
}

func linePtr(n int) *int { return &n }

// TestSubmitStaged_PayloadAndLink pins the publish contract both callers depend
// on — the human-approval handler and the auto-post posture go
// through this one function, so what it sends IS what a review looks like on
// GitHub regardless of which one triggered it.
func TestSubmitStaged_PayloadAndLink(t *testing.T) {
	sub := &recordingSubmitter{}
	details := domain.ReviewArtifactDetails{
		HeadSHA:     "start_review_head",
		ReviewBody:  "## Review",
		ReviewEvent: "COMMENT",
		StagedComments: []domain.ReviewArtifactComment{
			{Path: "a.go", Line: linePtr(3), Body: "nit", CommitSHA: "finalize_head"},
			{Path: "b.go", Line: linePtr(9), StartLine: linePtr(7), Body: "range", CommitSHA: "finalize_head"},
		},
	}

	res, err := SubmitStaged(context.Background(), sub, SubmitInput{
		Owner: "octo", Repo: "repo", Number: 7,
		Details: details,
		Footer:  "\n\n— footer",
		WebBase: func() string { return "https://ghe.example.com" },
	})
	if err != nil {
		t.Fatalf("SubmitStaged: %v", err)
	}
	if sub.calls != 1 {
		t.Fatalf("SubmitReview called %d times, want exactly 1", sub.calls)
	}
	// The whole review pins to ONE commit — the head every comment was
	// reconciled to at finalize, not the start-review head.
	if sub.commitID != "finalize_head" {
		t.Errorf("commit_id = %q, want the comments' reconciled head", sub.commitID)
	}
	if sub.body != "## Review\n\n— footer" {
		t.Errorf("body = %q, want the staged body with the footer appended", sub.body)
	}
	if len(sub.comments) != 2 || sub.comments[0].Path != "a.go" || sub.comments[0].Line != 3 {
		t.Fatalf("comments = %+v, want both staged comments verbatim", sub.comments)
	}
	if sub.comments[1].StartLine == nil || *sub.comments[1].StartLine != 7 {
		t.Errorf("comment[1].StartLine = %v, want the staged multi-line range preserved", sub.comments[1].StartLine)
	}
	if res.ReviewID != 4242 || res.Event != "COMMENT" {
		t.Errorf("result = %+v, want the submitted id + event", res)
	}
	// The deep link follows the ORG's GitHub host, so a GHES/GHEC org doesn't
	// get a dead github.com URL.
	if res.URL != "https://ghe.example.com/octo/repo/pull/7#pullrequestreview-4242" {
		t.Errorf("URL = %q, want the org-host review anchor", res.URL)
	}
}

// TestSubmitStaged_CommentlessPinsStartHead pins the fallback: a pure approve /
// body-only review has no inline anchor, so the pin is the head recorded at
// start-review.
func TestSubmitStaged_CommentlessPinsStartHead(t *testing.T) {
	sub := &recordingSubmitter{}
	if _, err := SubmitStaged(context.Background(), sub, SubmitInput{
		Owner: "octo", Repo: "repo", Number: 7,
		Details: domain.ReviewArtifactDetails{HeadSHA: "start_review_head", ReviewEvent: "APPROVE"},
	}); err != nil {
		t.Fatalf("SubmitStaged: %v", err)
	}
	if sub.commitID != "start_review_head" {
		t.Errorf("commit_id = %q, want the start-review head", sub.commitID)
	}
	if sub.comments == nil || len(sub.comments) != 0 {
		t.Errorf("comments = %+v, want an empty (non-nil) set", sub.comments)
	}
}

// TestSubmitStaged_UnanchoredCommentFailsBeforeGitHub pins that a comment with
// no line anchor stops the publish BEFORE anything reaches GitHub — which is
// what lets the approval path answer 422 and the auto-post path leave the draft
// staged, both with nothing half-applied.
func TestSubmitStaged_UnanchoredCommentFailsBeforeGitHub(t *testing.T) {
	sub := &recordingSubmitter{}
	_, err := SubmitStaged(context.Background(), sub, SubmitInput{
		Owner: "octo", Repo: "repo", Number: 7,
		Details: domain.ReviewArtifactDetails{
			HeadSHA:        "h",
			ReviewEvent:    "COMMENT",
			StagedComments: []domain.ReviewArtifactComment{{Path: "a.go", Body: "floating"}},
		},
	})
	var unanchored *UnanchoredCommentError
	if !errors.As(err, &unanchored) {
		t.Fatalf("err = %v, want an UnanchoredCommentError", err)
	}
	if unanchored.Path != "a.go" {
		t.Errorf("Path = %q, want the offending comment's path", unanchored.Path)
	}
	if sub.calls != 0 {
		t.Errorf("GitHub was called %d times, want 0 — the guard runs before the submit", sub.calls)
	}
}

// TestSubmitStaged_LegacyUnanchoredCommentsPinToStartHead pins that comments
// with no CommitSHA — rows staged before per-comment anchoring existed —
// constrain the pin not at all rather than counting as disagreement. The review
// falls back to the start-review head, which is the only frame it has.
func TestSubmitStaged_LegacyUnanchoredCommentsPinToStartHead(t *testing.T) {
	sub := &recordingSubmitter{}
	if _, err := SubmitStaged(context.Background(), sub, SubmitInput{
		Owner: "octo", Repo: "repo", Number: 7,
		Details: domain.ReviewArtifactDetails{
			HeadSHA:     "start_review_head",
			ReviewEvent: "COMMENT",
			StagedComments: []domain.ReviewArtifactComment{
				{Path: "a.go", Line: linePtr(3), Body: "legacy"},
				{Path: "b.go", Line: linePtr(4), Body: "also legacy"},
			},
		},
	}); err != nil {
		t.Fatalf("SubmitStaged: %v", err)
	}
	if sub.commitID != "start_review_head" {
		t.Errorf("commit_id = %q, want the start-review head", sub.commitID)
	}
}

// TestSubmitStaged_DivergentAnchorsRefused pins the invariant the commit pin
// depends on rather than assuming it. Finalize reconciles every comment to one
// head and Refresh re-pins the survivors together, so two live anchors is a bug
// upstream — but the submit carries ONE commit_id, so picking one anyway would
// read the other comment's line in a frame it was never validated against. It
// is refused before any GitHub call.
func TestSubmitStaged_DivergentAnchorsRefused(t *testing.T) {
	sub := &recordingSubmitter{}
	_, err := SubmitStaged(context.Background(), sub, SubmitInput{
		Owner: "octo", Repo: "repo", Number: 7,
		Details: domain.ReviewArtifactDetails{
			HeadSHA:     "start_review_head",
			ReviewEvent: "COMMENT",
			StagedComments: []domain.ReviewArtifactComment{
				{Path: "a.go", Line: linePtr(3), Body: "old frame", CommitSHA: "head_one"},
				{Path: "b.go", Line: linePtr(4), Body: "new frame", CommitSHA: "head_two"},
			},
		},
	})
	var divergent *DivergentAnchorsError
	if !errors.As(err, &divergent) {
		t.Fatalf("err = %v, want a DivergentAnchorsError", err)
	}
	if divergent.First != "head_one" || divergent.Second != "head_two" {
		t.Errorf("error names %q and %q, want both disagreeing anchors", divergent.First, divergent.Second)
	}
	if sub.calls != 0 {
		t.Errorf("GitHub was called %d times, want 0 — the check runs before the submit", sub.calls)
	}
}

// TestSubmitStaged_SubmitFailurePropagates pins that a GitHub failure surfaces
// verbatim (each caller decides what to do about it) and yields no URL.
func TestSubmitStaged_SubmitFailurePropagates(t *testing.T) {
	boom := errors.New("502 bad gateway")
	res, err := SubmitStaged(context.Background(), &recordingSubmitter{err: boom}, SubmitInput{
		Owner: "octo", Repo: "repo", Number: 7,
		Details: domain.ReviewArtifactDetails{HeadSHA: "h", ReviewEvent: "COMMENT", ReviewBody: "b"},
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the GitHub error propagated", err)
	}
	if res.URL != "" || res.ReviewID != 0 {
		t.Errorf("result = %+v, want the zero value on failure", res)
	}
}

// TestPostureSubmits is the decision table the setting resolves to. The
// identity arm is the reason the setting exists: an App-backed review posts as a
// bot, a PAT-backed one posts as the person who lent the token.
func TestPostureSubmits(t *testing.T) {
	blocker := []domain.ReviewArtifactComment{
		{Path: "a.go", Body: domain.SeverityBadgeMarkdown(domain.SeverityBlocker) + "this breaks auth"},
	}
	minor := []domain.ReviewArtifactComment{
		{Path: "a.go", Body: domain.SeverityBadgeMarkdown(domain.SeverityMinor) + "nit"},
	}

	cases := []struct {
		name     string
		posture  string
		identity ghclient.Identity
		event    string
		comments []domain.ReviewArtifactComment
		want     bool
	}{
		{"draft never posts, even under an App", domain.ReviewPostureDraft, ghclient.IdentityApp, "COMMENT", nil, false},
		{"auto always posts, even under a PAT", domain.ReviewPostureAuto, ghclient.IdentityPAT, "REQUEST_CHANGES", blocker, true},
		{"identity posts under an App", domain.ReviewPostureIdentity, ghclient.IdentityApp, "COMMENT", nil, true},
		{"identity stages under a PAT", domain.ReviewPostureIdentity, ghclient.IdentityPAT, "COMMENT", nil, false},
		{"identity stages when unknown", domain.ReviewPostureIdentity, ghclient.IdentityUnknown, "COMMENT", nil, false},
		{"auto_unless_blocking posts an ordinary review", domain.ReviewPostureAutoUnlessBlocking, ghclient.IdentityUnknown, "COMMENT", minor, true},
		{"auto_unless_blocking posts an approve", domain.ReviewPostureAutoUnlessBlocking, ghclient.IdentityUnknown, "APPROVE", nil, true},
		{"auto_unless_blocking stages request-changes", domain.ReviewPostureAutoUnlessBlocking, ghclient.IdentityApp, "REQUEST_CHANGES", nil, false},
		{"auto_unless_blocking stages a blocker comment", domain.ReviewPostureAutoUnlessBlocking, ghclient.IdentityApp, "COMMENT", blocker, false},
		{"an empty posture stages", "", ghclient.IdentityApp, "COMMENT", nil, false},
		{"an unknown posture stages", "post-it-somewhere", ghclient.IdentityApp, "COMMENT", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PostureSubmits(tc.posture, tc.identity, tc.event, tc.comments); got != tc.want {
				t.Errorf("PostureSubmits(%q, %v, %q) = %v, want %v", tc.posture, tc.identity, tc.event, got, tc.want)
			}
		})
	}
}
