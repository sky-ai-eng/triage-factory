package domain

import "testing"

func TestNewReviewArtifact_Shape(t *testing.T) {
	a := NewReviewArtifact("octo/repo", 42, "PR_node42", "PRR_review7")

	if a.Provider != ArtifactProviderGitHub {
		t.Errorf("Provider = %q, want %q", a.Provider, ArtifactProviderGitHub)
	}
	if a.Kind != ArtifactKindReview {
		t.Errorf("Kind = %q, want %q", a.Kind, ArtifactKindReview)
	}
	if a.Target != "octo/repo#42" {
		t.Errorf("Target = %q, want octo/repo#42", a.Target)
	}
	if a.ExternalID != "PRR_review7" {
		t.Errorf("ExternalID = %q, want PRR_review7 (the review node id)", a.ExternalID)
	}
	if a.State != ArtifactStateReviewPending {
		t.Errorf("State = %q, want %q", a.State, ArtifactStateReviewPending)
	}
	if a.DedupKey != "github:review:octo/repo#42" {
		t.Errorf("DedupKey = %q, want github:review:octo/repo#42", a.DedupKey)
	}

	d, err := ParseReviewArtifactDetails(a.DetailsJSON)
	if err != nil {
		t.Fatalf("ParseReviewArtifactDetails: %v", err)
	}
	if d.NodeID != "PR_node42" {
		t.Errorf("details NodeID = %q, want PR_node42", d.NodeID)
	}
	if d.Number != 42 {
		t.Errorf("details Number = %d, want 42", d.Number)
	}
	// Fresh artifact: no ready sentinel and no proposed snapshot yet.
	if d.ReviewEvent != "" {
		t.Errorf("fresh review details ReviewEvent = %q, want empty (ready sentinel unset)", d.ReviewEvent)
	}
	if len(d.Proposed.Comments) != 0 || d.Proposed.Body != "" || d.Proposed.Event != "" {
		t.Errorf("fresh review details Proposed should be zero, got %+v", d.Proposed)
	}
}

func TestReviewArtifactDetails_RoundTrip(t *testing.T) {
	line := 12
	in := ReviewArtifactDetails{
		NodeID:      "PR_node1",
		Number:      7,
		ReviewBody:  "## Looks good\nminor nits",
		ReviewEvent: "COMMENT",
		Proposed: ReviewArtifactProposed{
			Body:  "## Looks good\nminor nits",
			Event: "COMMENT",
			Comments: []ReviewArtifactComment{
				{ID: "PRRC_1", Path: "a.go", Line: &line, Body: "nit: rename"},
			},
		},
	}
	out, err := ParseReviewArtifactDetails(MarshalReviewArtifactDetails(in))
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if out.ReviewEvent != "COMMENT" || out.ReviewBody != in.ReviewBody {
		t.Errorf("staged body/event did not round-trip: %+v", out)
	}
	if len(out.Proposed.Comments) != 1 || out.Proposed.Comments[0].ID != "PRRC_1" {
		t.Errorf("proposed comments did not round-trip: %+v", out.Proposed.Comments)
	}
	if out.Proposed.Comments[0].Line == nil || *out.Proposed.Comments[0].Line != 12 {
		t.Errorf("proposed comment line did not round-trip: %+v", out.Proposed.Comments[0])
	}
}

func TestParseReviewArtifactDetails_Empty(t *testing.T) {
	d, err := ParseReviewArtifactDetails("")
	if err != nil {
		t.Fatalf("empty input should not error: %v", err)
	}
	if d.NodeID != "" || d.Number != 0 || d.ReviewEvent != "" || d.ReviewBody != "" || len(d.Proposed.Comments) != 0 {
		t.Errorf("empty input should yield zero value, got %+v", d)
	}
}

func TestFirstReadyReview(t *testing.T) {
	pending := NewReviewArtifact("o/r", 1, "n1", "PRR_1") // ReviewEvent unset
	ready := NewReviewArtifact("o/r", 2, "n2", "PRR_2")
	rd, _ := ParseReviewArtifactDetails(ready.DetailsJSON)
	rd.ReviewEvent = "APPROVE"
	ready.DetailsJSON = MarshalReviewArtifactDetails(rd)

	// Only the finalized one (ready sentinel set) is "ready".
	if got := FirstReadyReview([]Artifact{pending}); got != nil {
		t.Errorf("a started-but-not-submitted review must not be ready, got %+v", got)
	}
	got := FirstReadyReview([]Artifact{pending, ready})
	if got == nil || got.ExternalID != "PRR_2" {
		t.Errorf("FirstReadyReview should pick the finalized review, got %+v", got)
	}

	// Both predicates ignore submitted/dismissed artifacts.
	submitted := ready
	submitted.State = ArtifactStateReviewSubmitted
	if got := FirstPendingReviewArtifact([]Artifact{submitted}); got != nil {
		t.Errorf("a submitted review is not pending, got %+v", got)
	}
}

func TestFirstPendingReviewArtifact(t *testing.T) {
	pending := NewReviewArtifact("o/r", 1, "n1", "PRR_1")
	if got := FirstPendingReviewArtifact([]Artifact{pending}); got == nil || got.ExternalID != "PRR_1" {
		t.Errorf("FirstPendingReviewArtifact should find the pending review, got %+v", got)
	}
	if got := FirstPendingReviewArtifact(nil); got != nil {
		t.Errorf("FirstPendingReviewArtifact(nil) = %+v, want nil", got)
	}
}
