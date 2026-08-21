package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The watched-vs-armed split, from the write side. A project row is the team's
// commitment to WATCH the project; ARMED — all three rules complete — is the
// later state reached by mapping the workflow's statuses. These pin what the
// PUT accepts in between, and that the poller is only re-dued when the armed
// configuration actually moved.

// storedJiraProject reads one project's rules back off the team settings GET,
// which is the shape a client re-sends.
func storedJiraProject(t *testing.T, s *Server, key string) jiraProjectSettings {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, teamSettingsPath("default"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET team settings = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		JiraProjects []jiraProjectSettings `json:"jira_projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode team settings: %v", err)
	}
	for _, p := range out.JiraProjects {
		if p.Key == key {
			return p
		}
	}
	t.Fatalf("project %s is not in the stored set %+v", key, out.JiraProjects)
	return jiraProjectSettings{}
}

// TestTeamJiraProjectsPut_WatchedWithoutRules is the whole point of the split:
// watching is one click in the picker, and the mapping is the step after it.
func TestTeamJiraProjectsPut_WatchedWithoutRules(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	body := map[string]any{"jira_projects": []map[string]any{{"key": "SKY"}}}
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body); rec.Code != http.StatusOK {
		t.Fatalf("watching an unmapped project = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := storedJiraProject(t, s, "SKY")
	if len(got.Pickup.Members) != 0 || len(got.InProgress.Members) != 0 || len(got.Done.Members) != 0 {
		t.Errorf("stored rules = %+v, want all three empty", got)
	}
	if got.InProgress.Canonical != nil || got.Done.Canonical != nil {
		t.Errorf("stored canonicals = %+v / %+v, want both null", got.InProgress.Canonical, got.Done.Canonical)
	}
}

// TestTeamJiraProjectsPut_PartiallyArmedStored: each rule is independently
// complete-or-empty, so a half-finished mapping persists rather than forcing
// the whole workflow to be mapped in one gesture.
func TestTeamJiraProjectsPut_PartiallyArmedStored(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	body := map[string]any{"jira_projects": []map[string]any{
		{"key": "SKY", "pickup": validPickup()},
	}}
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body); rec.Code != http.StatusOK {
		t.Fatalf("pickup-only project = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := storedJiraProject(t, s, "SKY")
	if len(got.Pickup.Members) != 1 || got.Pickup.Members[0].ID != statusToDoID {
		t.Errorf("stored pickup = %+v, want the mapped rule preserved", got.Pickup)
	}
	// The name came from Jira, not from the caller — the PUT accepts ids only.
	if got.Pickup.Members[0].Name != "To Do" {
		t.Errorf("stored pickup name = %q, want the server-resolved \"To Do\"", got.Pickup.Members[0].Name)
	}
	if len(got.InProgress.Members) != 0 || len(got.Done.Members) != 0 {
		t.Errorf("stored rules = %+v, want the unmapped two empty", got)
	}
}

// TestTeamJiraProjectsPut_CanonicalWithoutMembersRejected is the other half of
// complete-or-empty: a canonical that is not among the members it was chosen
// from is not a half-finished mapping, it is a broken one.
func TestTeamJiraProjectsPut_CanonicalWithoutMembersRejected(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	body := teamProjectsBodyWith("SKY",
		validPickup(),
		map[string]any{"member_ids": []string{}, "canonical_id": statusInProgressID},
		map[string]any{"member_ids": []string{}},
	)
	rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("canonical with no members = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if msg := firstErrorMessage(t, rec); !strings.Contains(msg, "in_progress members are required") {
		t.Errorf("error should name the rule missing its members, got: %q", msg)
	}
}

// TestTeamJiraProjectsPut_CompleteRulesStillStored — the existing complete
// shape is untouched by the relaxation.
func TestTeamJiraProjectsPut_CompleteRulesStillStored(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	body := teamProjectsBodyWith("SKY", validPickup(), validInProgress(), validDone())
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body); rec.Code != http.StatusOK {
		t.Fatalf("fully armed project = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := storedJiraProject(t, s, "SKY")
	if got.InProgress.Canonical == nil || got.InProgress.Canonical.ID != statusInProgressID {
		t.Errorf("stored in-progress canonical = %+v, want the mapped status", got.InProgress.Canonical)
	}
	if got.Done.Canonical == nil || got.Done.Canonical.Name != "Done" {
		t.Errorf("stored done canonical = %+v, want the mapped status", got.Done.Canonical)
	}
}

// jiraKicks reports whether the PUT re-dued Jira polling, waiting briefly
// because the callback fires on its own goroutine.
func jiraKicks(t *testing.T, s *Server) func() bool {
	t.Helper()
	ch := make(chan struct{}, 8)
	s.SetOnJiraChanged(func(string) { ch <- struct{}{} })
	return func() bool {
		select {
		case <-ch:
			return true
		case <-time.After(300 * time.Millisecond):
			return false
		}
	}
}

// TestTeamJiraProjectsPut_UnarmedChangeDoesNotRestartPolling: an unarmed
// project contributes nothing to the discovery JQL, so watching one asks the
// poller no new question and must not re-due a cycle.
func TestTeamJiraProjectsPut_UnarmedChangeDoesNotRestartPolling(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY", "OPS")
	kicked := jiraKicks(t, s)

	body := map[string]any{"jira_projects": []map[string]any{{"key": "SKY"}}}
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body); rec.Code != http.StatusOK {
		t.Fatalf("watch PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	if kicked() {
		t.Error("watching an unmapped project re-dued Jira polling")
	}

	// A second unarmed project is still no change the poller can act on.
	two := map[string]any{"jira_projects": []map[string]any{{"key": "SKY"}, {"key": "OPS"}}}
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), two); rec.Code != http.StatusOK {
		t.Fatalf("second watch PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	if kicked() {
		t.Error("watching a second unmapped project re-dued Jira polling")
	}
}

// TestTeamJiraProjectsPut_ArmingRestartsPolling is the other side: mapping the
// statuses is the change the poller has been waiting for.
func TestTeamJiraProjectsPut_ArmingRestartsPolling(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")
	kicked := jiraKicks(t, s)

	watch := map[string]any{"jira_projects": []map[string]any{{"key": "SKY"}}}
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), watch); rec.Code != http.StatusOK {
		t.Fatalf("watch PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	if kicked() {
		t.Fatal("watching an unmapped project re-dued Jira polling")
	}

	armed := teamProjectsBodyWith("SKY", validPickup(), validInProgress(), validDone())
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), armed); rec.Code != http.StatusOK {
		t.Fatalf("arming PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !kicked() {
		t.Error("arming a project did not re-due Jira polling")
	}
}
