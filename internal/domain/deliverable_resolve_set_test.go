package domain

import "testing"

// readyReview builds a finalized (ready-sentinel set) pending review deliverable
// with the given PR number + conversation id, for the set-helper tests. A TF-side draft
// (TFAC-494) has no GitHub review id, so the tests key on Target/ID, not
// ExternalID.
func readyReview(number int, conversationID string) Deliverable {
	a := NewReviewDeliverable("o/r", number, "PR_kwDOnode", "headsha", conversationID)
	rd, _ := ParseReviewDeliverableDetails(a.DetailsJSON)
	rd.ReviewEvent = "APPROVE"
	a.DetailsJSON = MarshalReviewDeliverableDetails(rd)
	return a
}

// draftPR builds a draft pull_request deliverable with the given number + id, for
// the set-helper tests.
func draftPR(number int, id string) Deliverable {
	a := NewPullRequestDeliverable("o/r", number, "n", "head", "main", "url", "t", "b", true)
	a.ID = id
	return a
}

func TestAllDraftPullRequests(t *testing.T) {
	d1 := draftPR(1, "a1")
	d2 := draftPR(2, "a2")
	open := draftPR(3, "a3")
	open.State = DeliverableStatePROpen // resolved — must not count

	got := AllDraftPullRequests([]Deliverable{d1, open, d2})
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
	ready1 := readyReview(1, "run_1")
	pendingUnfinalized := NewReviewDeliverable("o/r", 2, "PR_kwDOnode", "headsha", "run_2") // no sentinel
	ready2 := readyReview(3, "run_3")

	got := AllReadyReviews([]Deliverable{ready1, pendingUnfinalized, ready2})
	if len(got) != 2 {
		t.Fatalf("AllReadyReviews len = %d, want 2 (only finalized)", len(got))
	}
	if got[0].Target != "o/r#1" || got[1].Target != "o/r#3" {
		t.Errorf("AllReadyReviews = %q,%q, want o/r#1,o/r#3", got[0].Target, got[1].Target)
	}
}

func TestAllPendingReviewDeliverables(t *testing.T) {
	finalized := readyReview(1, "run_1")
	unfinalized := NewReviewDeliverable("o/r", 2, "PR_kwDOnode", "headsha", "run_2")
	submitted := readyReview(3, "run_3")
	submitted.State = DeliverableStateReviewSubmitted // resolved

	// All pending reviews count regardless of the ready sentinel (teardown
	// abandons even started-but-unfinalized reviews); submitted/dismissed don't.
	got := AllPendingReviewDeliverables([]Deliverable{finalized, unfinalized, submitted})
	if len(got) != 2 {
		t.Fatalf("AllPendingReviewDeliverables len = %d, want 2 (both pending, finalized or not)", len(got))
	}
	if got[0].Target != "o/r#1" || got[1].Target != "o/r#2" {
		t.Errorf("AllPendingReviewDeliverables = %q,%q, want o/r#1,o/r#2", got[0].Target, got[1].Target)
	}
}

func TestUnresolvedDeliverableIDs(t *testing.T) {
	d := draftPR(1, "pr1")
	openPR := draftPR(2, "pr2")
	openPR.State = DeliverableStatePROpen
	ready := readyReview(3, "PRR_1")
	ready.ID = "rv1"
	unfinalized := NewReviewDeliverable("o/r", 4, "PR_kwDOnode", "n", "PRR_2") // not ready → not approvable
	unfinalized.ID = "rv2"

	ids := UnresolvedDeliverableIDs([]Deliverable{d, openPR, ready, unfinalized})
	// Draft PR ids first, then ready-review ids — both approvable; the open PR and
	// the unfinalized review are excluded.
	if len(ids) != 2 || ids[0] != "pr1" || ids[1] != "rv1" {
		t.Errorf("UnresolvedDeliverableIDs = %v, want [pr1 rv1]", ids)
	}
	// Always non-nil so the JSON field serializes as [] not null.
	if got := UnresolvedDeliverableIDs(nil); got == nil || len(got) != 0 {
		t.Errorf("UnresolvedDeliverableIDs(nil) = %v, want non-nil empty slice", got)
	}
}
