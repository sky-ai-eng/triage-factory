package domain

import "testing"

// readyReview builds a finalized (ready-sentinel set) pending review artifact
// with the given PR number + review node id, for the set-helper tests.
func readyReview(number int, reviewID string) Artifact {
	a := NewReviewArtifact("o/r", number, "n", reviewID)
	rd, _ := ParseReviewArtifactDetails(a.DetailsJSON)
	rd.ReviewEvent = "APPROVE"
	a.DetailsJSON = MarshalReviewArtifactDetails(rd)
	return a
}

// draftPR builds a draft pull_request artifact with the given number + id, for
// the set-helper tests.
func draftPR(number int, id string) Artifact {
	a := NewPullRequestArtifact("o/r", number, "n", "head", "main", "url", "t", "b", true)
	a.ID = id
	return a
}

func TestAllDraftPullRequests(t *testing.T) {
	d1 := draftPR(1, "a1")
	d2 := draftPR(2, "a2")
	open := draftPR(3, "a3")
	open.State = ArtifactStatePROpen // resolved — must not count

	got := AllDraftPullRequests([]Artifact{d1, open, d2})
	if len(got) != 2 {
		t.Fatalf("AllDraftPullRequests len = %d, want 2 (only the drafts)", len(got))
	}
	if got[0].ID != "a1" || got[1].ID != "a2" {
		t.Errorf("AllDraftPullRequests order/ids = %q,%q, want a1,a2", got[0].ID, got[1].ID)
	}
	if AllDraftPullRequests(nil) != nil {
		t.Errorf("AllDraftPullRequests(nil) should be nil")
	}
}

func TestAllReadyReviews(t *testing.T) {
	ready1 := readyReview(1, "PRR_1")
	pendingUnfinalized := NewReviewArtifact("o/r", 2, "n", "PRR_2") // no sentinel
	ready2 := readyReview(3, "PRR_3")

	got := AllReadyReviews([]Artifact{ready1, pendingUnfinalized, ready2})
	if len(got) != 2 {
		t.Fatalf("AllReadyReviews len = %d, want 2 (only finalized)", len(got))
	}
	if got[0].ExternalID != "PRR_1" || got[1].ExternalID != "PRR_3" {
		t.Errorf("AllReadyReviews = %q,%q, want PRR_1,PRR_3", got[0].ExternalID, got[1].ExternalID)
	}
}

func TestAllPendingReviewArtifacts(t *testing.T) {
	finalized := readyReview(1, "PRR_1")
	unfinalized := NewReviewArtifact("o/r", 2, "n", "PRR_2")
	submitted := readyReview(3, "PRR_3")
	submitted.State = ArtifactStateReviewSubmitted // resolved

	// All pending reviews count regardless of the ready sentinel (teardown
	// abandons even started-but-unfinalized reviews); submitted/dismissed don't.
	got := AllPendingReviewArtifacts([]Artifact{finalized, unfinalized, submitted})
	if len(got) != 2 {
		t.Fatalf("AllPendingReviewArtifacts len = %d, want 2 (both pending, finalized or not)", len(got))
	}
	if got[0].ExternalID != "PRR_1" || got[1].ExternalID != "PRR_2" {
		t.Errorf("AllPendingReviewArtifacts = %q,%q, want PRR_1,PRR_2", got[0].ExternalID, got[1].ExternalID)
	}
}

func TestUnresolvedArtifactIDs(t *testing.T) {
	d := draftPR(1, "pr1")
	openPR := draftPR(2, "pr2")
	openPR.State = ArtifactStatePROpen
	ready := readyReview(3, "PRR_1")
	ready.ID = "rv1"
	unfinalized := NewReviewArtifact("o/r", 4, "n", "PRR_2") // not ready → not approvable
	unfinalized.ID = "rv2"

	ids := UnresolvedArtifactIDs([]Artifact{d, openPR, ready, unfinalized})
	// Draft PR ids first, then ready-review ids — both approvable; the open PR and
	// the unfinalized review are excluded.
	if len(ids) != 2 || ids[0] != "pr1" || ids[1] != "rv1" {
		t.Errorf("UnresolvedArtifactIDs = %v, want [pr1 rv1]", ids)
	}
	// Always non-nil so the JSON field serializes as [] not null.
	if got := UnresolvedArtifactIDs(nil); got == nil || len(got) != 0 {
		t.Errorf("UnresolvedArtifactIDs(nil) = %v, want non-nil empty slice", got)
	}
}
