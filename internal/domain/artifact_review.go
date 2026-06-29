package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ReviewArtifactComment is one inline comment on the agent's drafted review,
// staged entirely TF-side (TFAC-494) — the review never occupies GitHub's
// per-identity pending-review slot during the draft window. Body is the
// GitHub-native form — the severity badge is baked in (parsed back out for
// display via ParseSeverityBadge) — so it submits to GitHub verbatim at approval.
// ID is a TF-local, non-numeric id minted at add-review-comment time and stable
// across the draft; it's the key comment edit/delete address the staged set on,
// and the key the approve-time human-feedback diff joins the proposed set to the
// final set on (both sides carry the same local ids).
//
// Line/StartLine are pointers so an unpositioned comment is distinguishable from
// a real line: nil means "no anchor", never silently collapsed to 0. A staged
// comment with a nil Line can't be submitted to GitHub — approval surfaces that
// rather than dropping it.
//
// CommitSHA is the PR head the comment's (path, line) was validated and anchored
// against at add-review-comment time — GitHub interprets a comment's line in the
// frame of a commit, so the atomic submit must pin commit_id to the same commit
// the line was validated against. GitHub anchors each comment independently (a
// review can carry comments against different commits); the atomic submit can't —
// it takes one commit_id for the whole review — so all of a draft's comments must
// share this SHA at approval, else the PR moved mid-review and the draft can't be
// submitted coherently (approval surfaces that rather than mis-anchoring). Empty
// only on rows staged before per-comment anchoring landed.
type ReviewArtifactComment struct {
	ID        string `json:"id,omitempty"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	StartLine *int   `json:"start_line,omitempty"`
	Body      string `json:"body"`
	CommitSHA string `json:"commit_sha,omitempty"`
}

// ReviewArtifactProposed is the agent's first-draft review, snapshotted once at
// finalize-review (the moment the agent hands the review off for human approval).
// Write-once: the approve-time human-feedback diff compares it against the
// human-edited final, so it must not change as the human edits.
type ReviewArtifactProposed struct {
	Body     string                  `json:"body"`
	Event    string                  `json:"event"`
	Comments []ReviewArtifactComment `json:"comments"`
}

// ReviewArtifactDetails is the kind-specific payload in a review artifact's
// DetailsJSON. The whole review is staged here (TFAC-494) — start-review through
// approval makes no GitHub write until the atomic submit at approval — so it
// holds the full draft, not just a pointer to a live GitHub pending review:
//
//   - Number: the backing PR's per-repo number (a review anchors to a PR).
//   - HeadSHA: the PR head at start-review. It is the FALLBACK commit_id for a
//     comment-less review (an approve / body-only request-changes, which has no
//     inline anchor); a review WITH comments pins commit_id to the commit those
//     comments were validated against (each comment's CommitSHA), not this — the
//     PR head can advance between start-review and the comments.
//   - FinalizedHeadSHA: the PR head at finalize-review — the commit every staged
//     comment was reconciled to (TFAC-499) and the baseline the human-facing
//     freshness check (TFAC-500) measures drift from: reviewGet compares it to
//     the live PR head for the "N commits since this review was written" count.
//     Empty on a never-finalized draft and on rows finalized before TFAC-500
//     (those fall back to HeadSHA for the count).
//   - FinalizedBaseSHA: the PR base branch tip at finalize-review. Paired with
//     FinalizedHeadSHA it reproduces the finalize-time PR diff deterministically
//     (the review overlay renders FinalizedBaseSHA...FinalizedHeadSHA so every
//     staged comment anchors in the frame it was written against, TFAC-500); the
//     three-dot compare recomputes the merge-base from the two fixed commits, so
//     a later base advance can't shift it. Empty on a comment-less review (which
//     makes no GitHub call at finalize) and on pre-TFAC-500 rows — the overlay
//     falls back to the live PR diff in that case.
//   - StagedComments: the mutable inline-comment set — appended at
//     add-review-comment (each anchored to the PR head it was validated against,
//     ReviewArtifactComment.CommitSHA), edited/deleted live by the human (local
//     writes to this slice), and submitted as the review's comments[] at approval.
//     Comment ids are TF-local and stable so edit/delete address them.
//   - ReviewBody / ReviewEvent: the *staged* review summary + verdict —
//     finalized at finalize-review, then mutated 1:1 by the human via PATCH, and
//     applied to GitHub at approval. ReviewEvent doubles as the **ready
//     sentinel**: empty until finalize-review finalizes the draft, so the
//     unresolved-artifact check distinguishes a started-but-not-finalized review
//     (not yet awaiting approval) from a finalized one (awaiting approval).
//   - Proposed: the agent's first-draft {body, event, comments}. Write-once —
//     snapshotted at finalize-review and never modified — the baseline the
//     approve-time human-feedback diff renders against (proposed vs. the final
//     StagedComments).
//
// NodeID is retained for backward compatibility with rows written before the
// local-staging rework; it is no longer populated (review reconciliation is moot
// for a never-published draft, TFAC-494 §8).
type ReviewArtifactDetails struct {
	NodeID           string                  `json:"node_id,omitempty"`
	Number           int                     `json:"number,omitempty"`
	HeadSHA          string                  `json:"head_sha,omitempty"`
	FinalizedHeadSHA string                  `json:"finalized_head_sha,omitempty"`
	FinalizedBaseSHA string                  `json:"finalized_base_sha,omitempty"`
	ReviewBody       string                  `json:"review_body"`
	ReviewEvent      string                  `json:"review_event"`
	StagedComments   []ReviewArtifactComment `json:"staged_comments,omitempty"`
	Proposed         ReviewArtifactProposed  `json:"proposed"`
}

// ReviewTarget is the artifact Target for a review: owner/repo#<number> — the PR
// the review is attached to. A pending review has no stable resource key of its
// own (its node id moves to ExternalID), so it anchors to its PR. Parse it back
// with ParsePRTarget, which is shape-generic over owner/repo#<number>.
func ReviewTarget(repoPath string, number int) string {
	return fmt.Sprintf("%s#%d", repoPath, number)
}

// ReviewDedupKey is the stable key a run's review-draft upserts on:
// github:review:owner/repo#<number>:<run_id>. It is **run-scoped** (TFAC-494):
// the review is staged entirely TF-side, so two runs (different teams, a
// re-delegate, interactive steering) reviewing the same PR each own an
// independent draft row instead of colliding on one org+PR-scoped row. A run
// reviews a PR at most once, so (PR, run) is unique.
func ReviewDedupKey(repoPath string, number int, runID string) string {
	return ArtifactDedupKey(ArtifactProviderGitHub, ArtifactKindReview, ReviewTarget(repoPath, number), runID)
}

// NewReviewArtifact builds the durable review-draft artifact start-review records
// — a fully TF-side draft, with zero GitHub writes (TFAC-494). repoPath is
// "owner/repo"; number is the PR number; headSHA is the reviewed commit (pinned
// into details and passed as commit_id at the atomic submit on approval); runID
// scopes the dedup key so concurrent runs on one PR never collide.
//
// The artifact starts state=pending with an empty ReviewEvent and an empty
// ExternalID: nothing exists on GitHub yet (the submitted review's id + URL are
// stamped on at approval), and ReviewEvent stays empty until finalize-review sets
// the ready sentinel. RunID/OrgID/TeamID are stamped by the recording client.
func NewReviewArtifact(repoPath string, number int, headSHA, runID string) Artifact {
	details, err := json.Marshal(ReviewArtifactDetails{
		Number:  number,
		HeadSHA: headSHA,
	})
	if err != nil {
		// This fixed shape can't realistically fail to marshal; degrade to empty
		// details rather than dropping the artifact entirely.
		details = nil
	}
	return Artifact{
		Provider:    ArtifactProviderGitHub,
		Kind:        ArtifactKindReview,
		Target:      ReviewTarget(repoPath, number),
		State:       ArtifactStateReviewPending,
		DedupKey:    ReviewDedupKey(repoPath, number, runID),
		DetailsJSON: string(details),
	}
}

// MarshalReviewArtifactDetails serializes details to a DetailsJSON string.
// Returns "" on a marshal error — callers treat "" as empty details (→ SQL NULL).
func MarshalReviewArtifactDetails(d ReviewArtifactDetails) string {
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseReviewArtifactDetails unmarshals a review artifact's DetailsJSON. Returns
// the zero value (no error) on empty input so callers can treat a detail-less row
// as "no snapshot" without special-casing.
func ParseReviewArtifactDetails(detailsJSON string) (ReviewArtifactDetails, error) {
	var d ReviewArtifactDetails
	if strings.TrimSpace(detailsJSON) == "" {
		return d, nil
	}
	if err := json.Unmarshal([]byte(detailsJSON), &d); err != nil {
		return ReviewArtifactDetails{}, err
	}
	return d, nil
}

// FirstPendingReviewArtifact returns the first pending review artifact in arts
// (Kind==review, State==pending), or nil. "Pending" means not yet submitted or
// dismissed — the TF-side draft is still live. Used by the abandon path (which
// flips it dismissed) and shared by FirstReadyReview.
func FirstPendingReviewArtifact(arts []Artifact) *Artifact {
	for i := range arts {
		if arts[i].Kind == ArtifactKindReview && arts[i].State == ArtifactStateReviewPending {
			return &arts[i]
		}
	}
	return nil
}

// PendingReviewArtifactByID returns the pending review artifact in arts whose id
// matches handle (Kind==review, State==pending), or nil. A single run can hold
// several review drafts at once — dedup is run-scoped per PR (TFAC-494), so a run
// reviewing multiple PRs gets one draft row each — so add-review-comment /
// finalize-review must locate the specific draft the agent named by its handle, not
// just the first pending review (which could be a different PR's draft).
func PendingReviewArtifactByID(arts []Artifact, handle string) *Artifact {
	for i := range arts {
		if arts[i].ID == handle && arts[i].Kind == ArtifactKindReview && arts[i].State == ArtifactStateReviewPending {
			return &arts[i]
		}
	}
	return nil
}

// FirstReadyReview returns the first *finalized* pending review artifact in arts
// — Kind==review, State==pending, AND details.ReviewEvent != "" (the ready
// sentinel set by finalize-review). Only a finalized review is an unresolved
// artifact: a run that called start-review but never finalize-review has a pending
// artifact with an empty ReviewEvent and must NOT count (it would strand on an
// approval card with nothing to approve). Mirrors FirstDraftPullRequest for the
// review kind; shared by HasUnresolvedArtifacts so consumers agree on "the run
// has a review awaiting approval".
func FirstReadyReview(arts []Artifact) *Artifact {
	for i := range arts {
		if isReadyReview(arts[i]) {
			return &arts[i]
		}
	}
	return nil
}

// AllPendingReviewArtifacts returns every pending review artifact in arts (the
// plural of FirstPendingReviewArtifact), in slice order — Kind==review,
// State==pending, regardless of the ready sentinel. The task-level resolve-all
// teardown uses this to dismiss every abandoned review (finalized or not), the
// same "abandon whether or not it reached the ready sentinel" rule the
// single-artifact teardown path applied via FirstPendingReviewArtifact.
func AllPendingReviewArtifacts(arts []Artifact) []Artifact {
	var out []Artifact
	for i := range arts {
		if arts[i].Kind == ArtifactKindReview && arts[i].State == ArtifactStateReviewPending {
			out = append(out, arts[i])
		}
	}
	return out
}

// AllReadyReviews returns every finalized pending review artifact in arts (the
// plural of FirstReadyReview), in slice order — the ones a human can actually
// approve (the ready sentinel is set). The projection (pending_artifact_ids)
// uses this so only finalized reviews count as approvable, mirroring
// AllDraftPullRequests for the review kind. Shares isReadyReview with the
// single-artifact predicate.
func AllReadyReviews(arts []Artifact) []Artifact {
	var out []Artifact
	for i := range arts {
		if isReadyReview(arts[i]) {
			out = append(out, arts[i])
		}
	}
	return out
}

// isReadyReview is the single-artifact predicate for "a finalized pending review
// awaiting approval" — Kind==review, State==pending, and the ready sentinel
// (details.ReviewEvent) set. FirstReadyReview and UnresolvedArtifactCounts both
// go through it so the "ready review" definition lives in exactly one place.
func isReadyReview(a Artifact) bool {
	if a.Kind != ArtifactKindReview || a.State != ArtifactStateReviewPending {
		return false
	}
	d, err := ParseReviewArtifactDetails(a.DetailsJSON)
	return err == nil && d.ReviewEvent != ""
}
