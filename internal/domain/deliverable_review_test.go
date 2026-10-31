package domain

import (
	"testing"
)

func TestNewReviewDeliverable_Shape(t *testing.T) {
	a := NewReviewDeliverable("octo/repo", 42, "PR_kwDOnode", "headsha42", "conv_7")

	if a.Provider != ProviderGitHub {
		t.Errorf("Provider = %q, want %q", a.Provider, ProviderGitHub)
	}
	if a.Kind != DeliverableKindReview {
		t.Errorf("Kind = %q, want %q", a.Kind, DeliverableKindReview)
	}
	if a.Target != "octo/repo#42" {
		t.Errorf("Target = %q, want octo/repo#42", a.Target)
	}
	// A TF-side draft has no GitHub review id until the atomic submit at approval.
	if a.ExternalID != "" {
		t.Errorf("ExternalID = %q, want empty (no GitHub review until approval)", a.ExternalID)
	}
	if a.State != DeliverableStateReviewPending {
		t.Errorf("State = %q, want %q", a.State, DeliverableStateReviewPending)
	}
	// Keyed on the PR's node id, not its repository's name, and
	// conversation-scoped: two conversations reviewing the same PR get
	// distinct rows.
	if a.DedupKey != "github:review:PR_kwDOnode:conv_7" {
		t.Errorf("DedupKey = %q, want github:review:PR_kwDOnode:conv_7", a.DedupKey)
	}

	d, err := ParseReviewDeliverableDetails(a.DetailsJSON)
	if err != nil {
		t.Fatalf("ParseReviewDeliverableDetails: %v", err)
	}
	if d.HeadSHA != "headsha42" {
		t.Errorf("details HeadSHA = %q, want headsha42", d.HeadSHA)
	}
	if d.Number != 42 {
		t.Errorf("details Number = %d, want 42", d.Number)
	}
	// Fresh deliverable: no ready sentinel, no staged comments, no proposed snapshot.
	if d.ReviewEvent != "" {
		t.Errorf("fresh review details ReviewEvent = %q, want empty (ready sentinel unset)", d.ReviewEvent)
	}
	if len(d.StagedComments) != 0 {
		t.Errorf("fresh review details StagedComments should be empty, got %+v", d.StagedComments)
	}
	if len(d.Proposed.Comments) != 0 || d.Proposed.Body != "" || d.Proposed.Event != "" {
		t.Errorf("fresh review details Proposed should be zero, got %+v", d.Proposed)
	}
}

func TestReviewDedupKey_ConversationScoped(t *testing.T) {
	// The same PR reviewed by two conversations yields two distinct dedup keys, so the
	// upsert never collapses them onto one row.
	a := ReviewDedupKey("PR_kwDO7", "conv_a")
	b := ReviewDedupKey("PR_kwDO7", "conv_b")
	if a == b {
		t.Errorf("two conversations on one PR must have distinct dedup keys, both = %q", a)
	}
	// No node id, no key: a draft keyed on nothing would be every PR's.
	if got := ReviewDedupKey("", "conv_a"); got != "" {
		t.Errorf("ReviewDedupKey with no node id = %q, want empty", got)
	}
}

func TestReviewDeliverableDetails_RoundTrip(t *testing.T) {
	line := 12
	in := ReviewDeliverableDetails{
		Number:      7,
		HeadSHA:     "abc123",
		ReviewBody:  "## Looks good\nminor nits",
		ReviewEvent: "COMMENT",
		StagedComments: []ReviewDeliverableComment{
			{ID: "c_1", Path: "a.go", Line: &line, Body: "nit: rename"},
		},
		Proposed: ReviewDeliverableProposed{
			Body:  "## Looks good\nminor nits",
			Event: "COMMENT",
			Comments: []ReviewDeliverableComment{
				{ID: "c_1", Path: "a.go", Line: &line, Body: "nit: rename"},
			},
		},
	}
	out, err := ParseReviewDeliverableDetails(MarshalReviewDeliverableDetails(in))
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if out.ReviewEvent != "COMMENT" || out.ReviewBody != in.ReviewBody || out.HeadSHA != "abc123" {
		t.Errorf("staged body/event/head_sha did not round-trip: %+v", out)
	}
	if len(out.StagedComments) != 1 || out.StagedComments[0].ID != "c_1" {
		t.Errorf("staged comments did not round-trip: %+v", out.StagedComments)
	}
	if len(out.Proposed.Comments) != 1 || out.Proposed.Comments[0].ID != "c_1" {
		t.Errorf("proposed comments did not round-trip: %+v", out.Proposed.Comments)
	}
	if out.Proposed.Comments[0].Line == nil || *out.Proposed.Comments[0].Line != 12 {
		t.Errorf("proposed comment line did not round-trip: %+v", out.Proposed.Comments[0])
	}
}

func TestParseReviewDeliverableDetails_Empty(t *testing.T) {
	d, err := ParseReviewDeliverableDetails("")
	if err != nil {
		t.Fatalf("empty input should not error: %v", err)
	}
	if d.Number != 0 || d.ReviewEvent != "" || d.ReviewBody != "" || len(d.StagedComments) != 0 || len(d.Proposed.Comments) != 0 {
		t.Errorf("empty input should yield zero value, got %+v", d)
	}
}

func TestFirstReadyReview(t *testing.T) {
	pending := NewReviewDeliverable("o/r", 1, "PR_kwDOnode", "n1", "run_1") // ReviewEvent unset
	ready := NewReviewDeliverable("o/r", 2, "PR_kwDOnode", "n2", "run_2")
	rd, _ := ParseReviewDeliverableDetails(ready.DetailsJSON)
	rd.ReviewEvent = "APPROVE"
	ready.DetailsJSON = MarshalReviewDeliverableDetails(rd)

	// Only the finalized one (ready sentinel set) is "ready".
	if got := FirstReadyReview([]Deliverable{pending}); got != nil {
		t.Errorf("a started-but-not-finalized review must not be ready, got %+v", got)
	}
	got := FirstReadyReview([]Deliverable{pending, ready})
	if got == nil || got.Target != "o/r#2" {
		t.Errorf("FirstReadyReview should pick the finalized review, got %+v", got)
	}

	// Both predicates ignore submitted/dismissed deliverables.
	submitted := ready
	submitted.State = DeliverableStateReviewSubmitted
	if got := FirstPendingReviewDeliverable([]Deliverable{submitted}); got != nil {
		t.Errorf("a submitted review is not pending, got %+v", got)
	}
}

func TestFirstPendingReviewDeliverable(t *testing.T) {
	pending := NewReviewDeliverable("o/r", 1, "PR_kwDOnode", "n1", "run_1")
	if got := FirstPendingReviewDeliverable([]Deliverable{pending}); got == nil || got.Target != "o/r#1" {
		t.Errorf("FirstPendingReviewDeliverable should find the pending review, got %+v", got)
	}
	if got := FirstPendingReviewDeliverable(nil); got != nil {
		t.Errorf("FirstPendingReviewDeliverable(nil) = %+v, want nil", got)
	}
}
