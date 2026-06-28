package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ReviewArtifactComment is a snapshot of one inline comment on the agent's
// drafted review, captured from the live GitHub pending review at submit-review
// time. Body is the GitHub-native form — the severity badge is baked in (parsed
// back out for display via ParseSeverityBadge) — so the snapshot matches exactly
// what the bot drafted on GitHub. ID is the comment's GraphQL node id, the key
// the approve-time human-feedback diff joins the proposed set to the live set on.
//
// Line/StartLine are pointers because GitHub returns them null for a comment with
// no anchor on the current diff (an outdated/unpositioned comment) — nil means
// "no current-diff line", distinct from a real line, never silently collapsed to 0.
type ReviewArtifactComment struct {
	ID        string `json:"id,omitempty"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	StartLine *int   `json:"start_line,omitempty"`
	Body      string `json:"body"`
}

// ReviewArtifactProposed is the agent's first-draft review, snapshotted once at
// submit-review (the moment the agent hands the review off for human approval).
// Write-once: the approve-time human-feedback diff compares it against the
// human-edited final, so it must not change as the human edits.
type ReviewArtifactProposed struct {
	Body     string                  `json:"body"`
	Event    string                  `json:"event"`
	Comments []ReviewArtifactComment `json:"comments"`
}

// ReviewArtifactDetails is the kind-specific payload in a review artifact's
// DetailsJSON:
//
//   - NodeID / Number: the backing PR's durable GraphQL node id + per-repo
//     number (a review anchors to a PR; reconciliation keys on these).
//   - ReviewBody / ReviewEvent: the *staged* review summary + verdict —
//     initialized to the agent's draft at submit-review, then mutated 1:1 by the
//     human via PATCH, and applied to GitHub at approval (SubmitExistingReview).
//     ReviewEvent doubles as the **ready sentinel**: empty until submit-review
//     finalizes the draft, so the park check distinguishes a
//     started-but-not-submitted review (don't park) from a finalized one (park,
//     awaiting approval). The review body + event are NOT live-editable on a
//     GitHub pending review (the event *is* the submit), so they stage here and
//     apply atomically at approval — unlike per-comment edits, which are live.
//   - Proposed: the agent's first-draft {body, event, comments}. Write-once —
//     snapshotted at submit-review and never modified — the baseline the
//     approve-time human-feedback diff renders against.
type ReviewArtifactDetails struct {
	NodeID      string                 `json:"node_id,omitempty"`
	Number      int                    `json:"number,omitempty"`
	ReviewBody  string                 `json:"review_body"`
	ReviewEvent string                 `json:"review_event"`
	Proposed    ReviewArtifactProposed `json:"proposed"`
}

// ReviewTarget is the artifact Target for a review: owner/repo#<number> — the PR
// the review is attached to. A pending review has no stable resource key of its
// own (its node id moves to ExternalID), so it anchors to its PR. Parse it back
// with ParsePRTarget, which is shape-generic over owner/repo#<number>.
func ReviewTarget(repoPath string, number int) string {
	return fmt.Sprintf("%s#%d", repoPath, number)
}

// ReviewDedupKey is the stable key every writer that touches a given PR's review
// upserts on: github:review:owner/repo#<number>. GitHub allows one pending review
// per identity per PR, so the PR coordinate is already unique.
func ReviewDedupKey(repoPath string, number int) string {
	return ArtifactDedupKey(ArtifactProviderGitHub, ArtifactKindReview, ReviewTarget(repoPath, number), "")
}

// NewReviewArtifact builds the durable review artifact for a GitHub pending
// review the agent just created. repoPath is "owner/repo"; number is the PR
// number; nodeID is the PR's GraphQL node id (best-effort context for
// reconciliation); reviewID is the pending review's node id — the handle every
// later op (add-comment, submit, delete) speaks, stored as ExternalID.
//
// The artifact starts state=pending with an empty ReviewEvent: the review exists
// on GitHub (private to the bot) but isn't finalized for approval until
// submit-review sets the ready sentinel. RunID/OrgID/TeamID are stamped by the
// recording client, not here.
func NewReviewArtifact(repoPath string, number int, nodeID, reviewID string) Artifact {
	details, err := json.Marshal(ReviewArtifactDetails{
		NodeID: nodeID,
		Number: number,
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
		ExternalID:  reviewID,
		State:       ArtifactStateReviewPending,
		DedupKey:    ReviewDedupKey(repoPath, number),
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
// dismissed — a live GitHub pending review still backs it. Used by the abandon
// path (which deletes that GitHub pending review) and shared by FirstReadyReview.
func FirstPendingReviewArtifact(arts []Artifact) *Artifact {
	for i := range arts {
		if arts[i].Kind == ArtifactKindReview && arts[i].State == ArtifactStateReviewPending {
			return &arts[i]
		}
	}
	return nil
}

// FirstReadyReview returns the first *finalized* pending review artifact in arts
// — Kind==review, State==pending, AND details.ReviewEvent != "" (the ready
// sentinel set by submit-review). Only a finalized review is an unresolved
// artifact: a run that called start-review but never submit-review has a pending
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
