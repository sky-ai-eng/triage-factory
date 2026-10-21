package events

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestJiraPredicates_ScopeByProjectID pins that every Jira predicate scopes to
// a project by registry id, so a project key rename cannot leave a filter
// matching nothing, and that none still offers the key.
func TestJiraPredicates_ScopeByProjectID(t *testing.T) {
	n := 0
	for eventType, s := range All() {
		if !strings.HasPrefix(eventType, "jira:") {
			continue
		}
		n++
		fields := map[string]string{}
		for _, f := range s.Fields {
			fields[f.Name] = f.Type
		}
		if fields["jira_project_id_in"] != "string_list" {
			t.Errorf("%s has no jira_project_id_in list: fields %v", eventType, fields)
		}
		for _, gone := range []string{"project", "old_project"} {
			if _, ok := fields[gone]; ok {
				t.Errorf("%s still offers %s", eventType, gone)
			}
		}
	}
	if n != 12 {
		t.Errorf("checked %d jira event types, want 12", n)
	}
}

// TestJiraMatcher_ProjectIDIn runs the type-erased matcher: jira_project_id_in
// matches the event's jira_project_id exactly, and the project key, the value
// a caller might reach for, matches nothing.
func TestJiraMatcher_ProjectIDIn(t *testing.T) {
	s, _ := Get(domain.EventJiraIssueStatusChanged)
	metaJSON, _ := json.Marshal(JiraIssueStatusChangedMetadata{IssueKey: "SKY-1", Project: "SKY", JiraProjectID: "jp-sky", NewStatus: "In Review"})
	cases := []struct {
		name, predicate string
		want            bool
	}{
		{"empty list is no filter", `{"jira_project_id_in":[]}`, true},
		{"the event's project among others", `{"jira_project_id_in":["jp-ops","jp-sky"]}`, true},
		{"another project", `{"jira_project_id_in":["jp-ops"]}`, false},
		{"the key matches nothing", `{"jira_project_id_in":["SKY"]}`, false},
		{"ids are not case-folded", `{"jira_project_id_in":["JP-SKY"]}`, false},
		{"with another filter", `{"jira_project_id_in":["jp-sky"],"new_status":"In Review"}`, true},
	}
	for _, tc := range cases {
		got, err := s.Match(tc.predicate, string(metaJSON))
		if err != nil {
			t.Fatalf("%s: Match error: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: Match = %v, want %v", tc.name, got, tc.want)
		}
	}

	if _, err := s.Match(`{"project":"SKY"}`, string(metaJSON)); err == nil {
		t.Error("a stored project key matched without error; want it refused rather than read as match-all")
	}
	if _, err := ValidatePredicateJSON(domain.EventJiraIssueStatusChanged, `{"project":"SKY"}`); err == nil {
		t.Error("ValidatePredicateJSON accepted project")
	}
}

// TestJiraKeyChangedPredicate pins the two project filters of the one Jira
// event about two projects: jira_project_id_in on the project the issue is in
// now, old_jira_project_id_in on the one it left.
func TestJiraKeyChangedPredicate(t *testing.T) {
	meta := JiraIssueKeyChangedMetadata{
		IssueKey: "OPS-7", Project: "OPS", JiraProjectID: "jp-ops",
		OldIssueKey: "SKY-7", OldProject: "SKY", OldJiraProjectID: "jp-sky",
	}
	cases := []struct {
		name string
		pred JiraIssueKeyChangedPredicate
		want bool
	}{
		{"empty predicate", JiraIssueKeyChangedPredicate{}, true},
		{"the project it is in now", JiraIssueKeyChangedPredicate{JiraProjectIDIn: []string{"jp-ops"}}, true},
		{"the project it left, as the current project", JiraIssueKeyChangedPredicate{JiraProjectIDIn: []string{"jp-sky"}}, false},
		{"the project it left", JiraIssueKeyChangedPredicate{OldJiraProjectIDIn: []string{"jp-sky"}}, true},
		{"the project it is in now, as the project it left", JiraIssueKeyChangedPredicate{OldJiraProjectIDIn: []string{"jp-ops"}}, false},
		{"both", JiraIssueKeyChangedPredicate{JiraProjectIDIn: []string{"jp-ops"}, OldJiraProjectIDIn: []string{"jp-sky"}}, true},
	}
	for _, tc := range cases {
		if got := tc.pred.Matches(meta); got != tc.want {
			t.Errorf("%s: Matches() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
