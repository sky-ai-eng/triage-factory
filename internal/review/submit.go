package review

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// Submitter is the single GitHub call publishing a staged review needs: the
// atomic create+submit (one POST carrying commit_id + event + body + the
// comments). *github.Client satisfies it; so does the agenthost's per-repo
// resolved client and any test double.
type Submitter interface {
	SubmitReview(ctx context.Context, owner, repo string, number int, commitSHA, event, body string, comments []ghclient.SubmitReviewComment) (int, string, error)
}

// UnanchoredCommentError reports a staged comment that carries no line anchor
// and therefore cannot be submitted. It is raised BEFORE the GitHub call, so
// nothing has been published when a caller sees it — the human path answers 422
// and lets the user edit or delete the comment; the auto-post path leaves the
// draft staged for exactly that.
type UnanchoredCommentError struct{ Path string }

func (e *UnanchoredCommentError) Error() string {
	return "a staged review comment on " + e.Path + " has no line anchor and can't be submitted"
}

// SubmitInput is the staged draft to publish plus the two pieces of context the
// artifact row can't supply: the agentmeta footer to append to the body, and
// the org's GitHub web host for the submitted review's deep link.
type SubmitInput struct {
	Owner  string
	Repo   string
	Number int
	// Details is the review draft as stored on the artifact — its ReviewBody,
	// ReviewEvent (the ready sentinel), and StagedComments are what get posted.
	Details domain.ReviewArtifactDetails
	// Footer is appended verbatim to the review body (the agentmeta run
	// attribution). Empty appends nothing.
	Footer string
	// WebBase resolves the org's user-facing GitHub host (github.com, a GHES
	// host, a GHEC data-residency host) for the review's deep link. Called ONLY
	// after the submit lands — the human path resolves it on a detached context
	// so a client disconnect mid-submit can't silently downgrade the stamped
	// link to github.com. Nil (or a "" return) yields the public host.
	WebBase func() string
}

// SubmitResult is what actually reached GitHub: the review's numeric id, the
// event submitted, and the deep link composed against the org's own host.
type SubmitResult struct {
	ReviewID int
	Event    string
	URL      string
}

// SubmitStaged publishes a finalized review draft to GitHub. It is the ONE
// implementation of "publish a staged review", shared by the human-approval
// handler and the auto-post posture — two copies would drift, and
// the freshness/anchoring rules (ReconcileToHead, and the commit pin derived
// here) have to hold identically whichever path publishes.
//
// The commit pin: GitHub reads each comment's line in the frame of a commit and
// the atomic submit carries ONE commit_id for the whole review. finalize-review
// already reconciled every comment to the PR's single current head — auto-
// remapping pure shifts, hard-failing on anything outdated — so a submittable
// draft's comments all share one CommitSHA and the pin simply follows it. A
// comment-less review (approve / body-only) has no inline anchor, so it falls
// back to the start-review head (Details.HeadSHA).
//
// Callers keep their own persistence: this makes the GitHub write and reports
// what happened, and never touches the artifact row (the two callers claim,
// stamp, and audit differently — one is a human approval, the other is the
// drafting run's own action).
func SubmitStaged(ctx context.Context, gh Submitter, in SubmitInput) (SubmitResult, error) {
	comments := make([]ghclient.SubmitReviewComment, 0, len(in.Details.StagedComments))
	commitID := in.Details.HeadSHA
	for _, c := range in.Details.StagedComments {
		if c.Line == nil {
			return SubmitResult{}, &UnanchoredCommentError{Path: c.Path}
		}
		if c.CommitSHA != "" {
			commitID = c.CommitSHA
		}
		comments = append(comments, ghclient.SubmitReviewComment{
			Path:      c.Path,
			Line:      *c.Line,
			StartLine: c.StartLine,
			Body:      c.Body,
		})
	}

	// SubmitReview returns the event it submitted (it doesn't parse an
	// authoritative event back from GitHub's response) — pass it back so the
	// persisted artifact, any verdict diff, and the caller's response all
	// reflect the same value that was requested.
	reviewID, submittedEvent, err := gh.SubmitReview(ctx, in.Owner, in.Repo, in.Number,
		commitID, in.Details.ReviewEvent, in.Details.ReviewBody+in.Footer, comments)
	if err != nil {
		return SubmitResult{}, err
	}

	webBase := ""
	if in.WebBase != nil {
		webBase = in.WebBase()
	}
	return SubmitResult{
		ReviewID: reviewID,
		Event:    submittedEvent,
		URL: fmt.Sprintf("%s#pullrequestreview-%d",
			domain.GitHubPullURLBase(webBase, in.Owner+"/"+in.Repo, in.Number), reviewID),
	}, nil
}
