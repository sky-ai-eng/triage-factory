package domain

import "testing"

func reviewArtifactWith(t *testing.T, d ReviewArtifactDetails) Artifact {
	t.Helper()
	return Artifact{Kind: ArtifactKindReview, DetailsJSON: MarshalReviewArtifactDetails(d)}
}

// TestReviewAnchoredAtHead_StartHead: a head matching the start-review anchor
// (details.HeadSHA) reads as current.
func TestReviewAnchoredAtHead_StartHead(t *testing.T) {
	a := reviewArtifactWith(t, ReviewArtifactDetails{HeadSHA: "headA"})
	if !ReviewAnchoredAtHead(a, "headA") {
		t.Error("want anchored at the start head")
	}
	if ReviewAnchoredAtHead(a, "headB") {
		t.Error("want NOT anchored at a different head")
	}
}

// TestReviewAnchoredAtHead_ReconciledCommentHead: after finalize-review moves the
// comments to the PR's current head (details.HeadSHA stays the start head), a head
// matching a comment's CommitSHA reads as current — the post-finalize case the
// start-head-only compare would falsely flag as behind.
func TestReviewAnchoredAtHead_ReconciledCommentHead(t *testing.T) {
	a := reviewArtifactWith(t, ReviewArtifactDetails{
		HeadSHA: "startHead",
		StagedComments: []ReviewArtifactComment{
			{Path: "a.go", Body: "x", CommitSHA: "reconciledHead"},
		},
	})
	if !ReviewAnchoredAtHead(a, "reconciledHead") {
		t.Error("want anchored at the reconciled comment head")
	}
	if !ReviewAnchoredAtHead(a, "startHead") {
		t.Error("want still anchored at the start head too")
	}
	if ReviewAnchoredAtHead(a, "brandNewHead") {
		t.Error("want behind a head neither the start nor a comment anchor")
	}
}

// TestReviewAnchoredAtHead_SafeDefaults: a non-review artifact, an empty head, or
// unparseable details default to NOT anchored (the caller fires the note).
func TestReviewAnchoredAtHead_SafeDefaults(t *testing.T) {
	if ReviewAnchoredAtHead(Artifact{Kind: ArtifactKindPullRequest, DetailsJSON: ""}, "h") {
		t.Error("non-review artifact must read as not anchored")
	}
	if ReviewAnchoredAtHead(reviewArtifactWith(t, ReviewArtifactDetails{HeadSHA: "h"}), "") {
		t.Error("empty head must read as not anchored")
	}
	if ReviewAnchoredAtHead(Artifact{Kind: ArtifactKindReview, DetailsJSON: "{not json"}, "h") {
		t.Error("unparseable details must read as not anchored")
	}
}
