package server

import (
	"net/http"
	"strings"
	"testing"
)

// The optional in-review rule, from the write side. It arms no polling and
// classifies nothing, so what these pin is that it validates exactly like its
// write-target siblings, that it round-trips, and that mapping it alone asks
// the poller nothing.

func validInReview() map[string]any {
	return map[string]any{"member_ids": []string{statusInReviewID}, "canonical_id": statusInReviewID}
}

// withInReview adds an in_review rule to a single-project body from
// teamProjectsBodyWith, whose four-rule form would otherwise mean a fifth
// parameter on every existing call site.
func withInReview(body map[string]any, rule map[string]any) map[string]any {
	body["jira_projects"].([]map[string]any)[0]["in_review"] = rule
	return body
}

func armedBodyWithReview(key string) map[string]any {
	return withInReview(teamProjectsBodyWith(key, validPickup(), validInProgress(), validDone()), validInReview())
}

// A mapped rule round-trips, and its display name is the one the server
// resolved from Jira rather than anything the caller could have supplied.
func TestTeamJiraProjectsPut_InReviewStoredAndEchoed(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), armedBodyWithReview("SKY"))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with an in-review rule = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := storedJiraProject(t, s, "SKY")
	if got.InReview.Canonical == nil || got.InReview.Canonical.ID != statusInReviewID {
		t.Fatalf("stored in-review canonical = %+v, want the mapped status", got.InReview.Canonical)
	}
	if got.InReview.Canonical.Name != "Code Review" {
		t.Errorf("in-review canonical name = %q, want the server-resolved \"Code Review\"", got.InReview.Canonical.Name)
	}
	if len(got.InReview.Members) != 1 || got.InReview.Members[0].ID != statusInReviewID {
		t.Errorf("stored in-review members = %+v, want the mapped status", got.InReview.Members)
	}
}

// Leaving it unmapped is a complete configuration, so the rule must be
// storable empty and echo as null.
func TestTeamJiraProjectsPut_InReviewOptional(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	body := teamProjectsBodyWith("SKY", validPickup(), validInProgress(), validDone())
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body); rec.Code != http.StatusOK {
		t.Fatalf("PUT without an in-review rule = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := storedJiraProject(t, s, "SKY")
	if len(got.InReview.Members) != 0 || got.InReview.Canonical != nil {
		t.Errorf("stored in-review rule = %+v, want an unmapped rule", got.InReview)
	}
}

// Optional does not mean unchecked: members with no write target is half a
// rule, so it is refused exactly as in_progress is.
func TestTeamJiraProjectsPut_InReviewMembersWithoutCanonicalRejected(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	body := withInReview(
		teamProjectsBodyWith("SKY", validPickup(), validInProgress(), validDone()),
		map[string]any{"member_ids": []string{statusInReviewID}, "canonical_id": ""},
	)
	rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("in-review members with no canonical = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	items := decodeErrorItems(t, rec)
	if len(items) != 1 || items[0].Field != "jira_projects[0]" {
		t.Fatalf("errors = %+v, want one naming jira_projects[0]", items)
	}
	if !strings.Contains(items[0].Message, "in_review canonical is required") {
		t.Errorf("message %q does not name the missing write target", items[0].Message)
	}
}

// A write target that is not one of the members it was chosen from would fail
// at transition time — the check a CHECK constraint cannot express.
func TestTeamJiraProjectsPut_InReviewCanonicalNotInMembersRejected(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	body := withInReview(
		teamProjectsBodyWith("SKY", validPickup(), validInProgress(), validDone()),
		map[string]any{"member_ids": []string{statusInReviewID}, "canonical_id": statusDoneID},
	)
	rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("in-review canonical outside its members = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if msg := firstErrorMessage(t, rec); !strings.Contains(msg, "in_review canonical") {
		t.Errorf("error should name the in-review rule, got: %q", msg)
	}
}

// A status the project's workflow does not have is the caller's mistake, and
// the fault names the exact element it arrived in — one per bad value.
func TestTeamJiraProjectsPut_InReviewUnknownStatusRejected(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	body := withInReview(
		teamProjectsBodyWith("SKY", validPickup(), validInProgress(), validDone()),
		map[string]any{"member_ids": []string{statusUnknownID}, "canonical_id": statusUnknownID},
	)
	rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown in-review status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	fields := map[string]bool{}
	for _, item := range decodeErrorItems(t, rec) {
		fields[item.Field] = true
	}
	for _, want := range []string{"jira_projects[0].in_review.member_ids", "jira_projects[0].in_review.canonical_id"} {
		if !fields[want] {
			t.Errorf("no fault named %s; got %v", want, fields)
		}
	}
}

// Jira not answering establishes nothing, so a changed in-review rule that
// could not be verified is an upstream error and nothing is stored — the same
// contract every other rule on this route has.
func TestTeamJiraProjectsPut_InReviewUpstreamFailureStoresNothing(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")

	armed := teamProjectsBodyWith("SKY", validPickup(), validInProgress(), validDone())
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), armed); rec.Code != http.StatusOK {
		t.Fatalf("seeding PUT = %d, body=%s", rec.Code, rec.Body.String())
	}

	fake.SetFailing(true)
	rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), armedBodyWithReview("SKY"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("PUT with Jira down = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}

	fake.SetFailing(false)
	if got := storedJiraProject(t, s, "SKY"); len(got.InReview.Members) != 0 || got.InReview.Canonical != nil {
		t.Errorf("stored in-review rule = %+v, want nothing stored by the failed write", got.InReview)
	}
}

// Re-sending exactly what is stored — in-review rule included — is carried,
// not resolved, so the door stays open while Jira is unreachable.
func TestTeamJiraProjectsPut_ResendingAStoredInReviewRuleAsksJiraNothing(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")

	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), armedBodyWithReview("SKY")); rec.Code != http.StatusOK {
		t.Fatalf("seeding PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	before := fake.Calls()

	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), armedBodyWithReview("SKY")); rec.Code != http.StatusOK {
		t.Fatalf("re-PUT of the stored set = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if after := fake.Calls(); after != before {
		t.Errorf("Jira calls went %d → %d, want none — an unchanged set asks Jira nothing", before, after)
	}
}

// The in-review rule reaches neither the discovery JQL nor any classification,
// so mapping it is not a question the poller can act on. Changing pickup still
// is, which is what keeps this from being a blanket "never restart".
func TestTeamJiraProjectsPut_InReviewOnlyChangeDoesNotRestartPolling(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")
	kicked := jiraKicks(t, s)

	armed := teamProjectsBodyWith("SKY", validPickup(), validInProgress(), validDone())
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), armed); rec.Code != http.StatusOK {
		t.Fatalf("arming PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !kicked() {
		t.Fatal("arming a project did not re-due Jira polling")
	}

	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), armedBodyWithReview("SKY")); rec.Code != http.StatusOK {
		t.Fatalf("in-review PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	if kicked() {
		t.Error("mapping the in-review rule re-dued Jira polling")
	}

	widened := withInReview(
		teamProjectsBodyWith("SKY",
			map[string]any{"member_ids": []string{statusToDoID, statusInReviewID}},
			validInProgress(), validDone()),
		validInReview(),
	)
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), widened); rec.Code != http.StatusOK {
		t.Fatalf("pickup PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !kicked() {
		t.Error("widening pickup did not re-due Jira polling")
	}
}
