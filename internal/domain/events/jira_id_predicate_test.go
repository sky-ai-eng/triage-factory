package events

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestJiraPredicates_StatesPrioritiesAndTypesByID pins that no Jira predicate
// offers a status, priority or issue type by name, which a rename in Jira
// would leave matching nothing, and that each event whose metadata names one
// offers its id list instead.
func TestJiraPredicates_StatesPrioritiesAndTypesByID(t *testing.T) {
	names := []string{"status", "new_status", "old_status", "priority", "new_priority", "old_priority", "issue_type"}
	for eventType, s := range All() {
		if !strings.HasPrefix(eventType, "jira:") {
			continue
		}
		for _, f := range s.Fields {
			for _, name := range names {
				if f.Name == name {
					t.Errorf("%s still offers %s", eventType, name)
				}
			}
		}
	}
	want := map[string][]string{
		domain.EventJiraIssueAssigned:        {"status_id_in", "priority_id_in", "issue_type_id_in"},
		domain.EventJiraIssueAvailable:       {"status_id_in", "priority_id_in", "issue_type_id_in"},
		domain.EventJiraIssueBecameAtomic:    {"status_id_in", "priority_id_in", "issue_type_id_in"},
		domain.EventJiraIssueStatusChanged:   {"new_status_id_in", "old_status_id_in", "issue_type_id_in"},
		domain.EventJiraIssuePriorityChanged: {"new_priority_id_in", "old_priority_id_in"},
		domain.EventJiraIssueCompleted:       {"issue_type_id_in"},
		domain.EventJiraIssueUnreachable:     {"issue_type_id_in"},
		domain.EventJiraIssueBodyUpdated:     {"issue_type_id_in"},
	}
	for eventType, fields := range want {
		s, _ := Get(eventType)
		got := map[string]string{}
		for _, f := range s.Fields {
			got[f.Name] = f.Type
		}
		for _, name := range fields {
			if got[name] != "string_list" {
				t.Errorf("%s has no %s list: fields %v", eventType, name, got)
			}
		}
	}
}

// TestJiraMatcher_StatusPriorityAndTypeIDs runs the type-erased matcher: each
// id list matches the event's id exactly, the name a caller might reach for
// matches nothing, and a name field is refused rather than read as match-all.
func TestJiraMatcher_StatusPriorityAndTypeIDs(t *testing.T) {
	assigned, _ := Get(domain.EventJiraIssueAssigned)
	assignedJSON, _ := json.Marshal(JiraIssueAssignedMetadata{
		IssueKey: "SKY-1", JiraProjectID: "jp-sky",
		IssueType: "Bug", IssueTypeID: "10004",
		Priority: "High", PriorityID: "2",
		Status: "In Progress", StatusID: "10001",
	})
	changed, _ := Get(domain.EventJiraIssueStatusChanged)
	changedJSON, _ := json.Marshal(JiraIssueStatusChangedMetadata{
		IssueKey: "SKY-1", JiraProjectID: "jp-sky",
		OldStatus: "To Do", OldStatusID: "10000", NewStatus: "In Review", NewStatusID: "10003",
	})
	reprioritized, _ := Get(domain.EventJiraIssuePriorityChanged)
	reprioritizedJSON, _ := json.Marshal(JiraIssuePriorityChangedMetadata{
		IssueKey: "SKY-1", JiraProjectID: "jp-sky",
		OldPriority: "Low", OldPriorityID: "4", NewPriority: "High", NewPriorityID: "2",
	})
	cases := []struct {
		name      string
		schema    EventSchema
		meta      []byte
		predicate string
		want      bool
	}{
		{"status id", assigned, assignedJSON, `{"status_id_in":["10000","10001"]}`, true},
		{"another status id", assigned, assignedJSON, `{"status_id_in":["10000"]}`, false},
		{"a status name matches nothing", assigned, assignedJSON, `{"status_id_in":["In Progress"]}`, false},
		{"priority id", assigned, assignedJSON, `{"priority_id_in":["2"]}`, true},
		{"a priority name matches nothing", assigned, assignedJSON, `{"priority_id_in":["High"]}`, false},
		{"issue type id", assigned, assignedJSON, `{"issue_type_id_in":["10004"]}`, true},
		{"an issue type name matches nothing", assigned, assignedJSON, `{"issue_type_id_in":["Bug"]}`, false},
		{"into a status", changed, changedJSON, `{"new_status_id_in":["10003"]}`, true},
		{"the status it left, as the new one", changed, changedJSON, `{"new_status_id_in":["10000"]}`, false},
		{"out of a status", changed, changedJSON, `{"old_status_id_in":["10000"]}`, true},
		{"into one status and out of another", changed, changedJSON, `{"new_status_id_in":["10003"],"old_status_id_in":["10001"]}`, false},
		{"to a priority", reprioritized, reprioritizedJSON, `{"new_priority_id_in":["2"]}`, true},
		{"from a priority", reprioritized, reprioritizedJSON, `{"old_priority_id_in":["4"]}`, true},
		{"the priority it left, as the new one", reprioritized, reprioritizedJSON, `{"new_priority_id_in":["4"]}`, false},
	}
	for _, tc := range cases {
		got, err := tc.schema.Match(tc.predicate, string(tc.meta))
		if err != nil {
			t.Fatalf("%s: Match error: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: Match = %v, want %v", tc.name, got, tc.want)
		}
	}

	for eventType, predicate := range map[string]string{
		domain.EventJiraIssueAssigned:        `{"status":"In Progress"}`,
		domain.EventJiraIssueAvailable:       `{"issue_type":"Bug"}`,
		domain.EventJiraIssueStatusChanged:   `{"new_status":"In Review"}`,
		domain.EventJiraIssuePriorityChanged: `{"new_priority":"High"}`,
	} {
		if _, err := ValidatePredicateJSON(eventType, predicate); err == nil {
			t.Errorf("ValidatePredicateJSON(%s) accepted %s", eventType, predicate)
		}
	}
}
