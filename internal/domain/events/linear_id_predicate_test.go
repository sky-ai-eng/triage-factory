package events

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestLinearPredicates_StatesAndLabelsByID pins that no Linear predicate
// offers a workflow state, a label or a label group by name, which a rename in
// Linear would leave matching nothing.
func TestLinearPredicates_StatesAndLabelsByID(t *testing.T) {
	names := []string{"status", "new_status", "old_status", "has_label", "label_name", "label_group"}
	for eventType, s := range All() {
		if !strings.HasPrefix(eventType, "linear:") {
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
}

// TestLinearMatcher_StateAndLabelIDs runs the type-erased matcher: the state
// and label lists match ids exactly, has_label_id_in matches when the issue
// bears any one of its labels, and a name matches nothing.
func TestLinearMatcher_StateAndLabelIDs(t *testing.T) {
	identity := LinearIssueIdentity{IssueIdentifier: "ENG-1", LinearTeamID: "team-eng"}
	assigned, _ := Get(domain.EventLinearIssueAssigned)
	assignedJSON, _ := json.Marshal(LinearIssueAssignedMetadata{
		LinearIssueIdentity: identity,
		Status:              "In Progress", StatusID: "state-started",
		Labels: []string{"bug", "tf-agent"}, LabelIDs: []string{"label-bug", "label-agent"},
	})
	changed, _ := Get(domain.EventLinearIssueStatusChanged)
	changedJSON, _ := json.Marshal(LinearIssueStatusChangedMetadata{
		LinearIssueIdentity: identity,
		OldStatus:           "Todo", OldStatusID: "state-todo", NewStatus: "In Review", NewStatusID: "state-review",
	})
	labeled, _ := Get(domain.EventLinearIssueLabelAdded)
	labeledJSON, _ := json.Marshal(LinearIssueLabelMetadata{
		LinearIssueIdentity: identity,
		LabelID:             "label-medium", LabelName: "Medium", LabelGroupID: "group-difficulty", LabelGroup: "Difficulty",
		Labels: []string{"Medium"}, LabelIDs: []string{"label-medium"},
	})
	cases := []struct {
		name      string
		schema    EventSchema
		meta      []byte
		predicate string
		want      bool
	}{
		{"state id", assigned, assignedJSON, `{"status_id_in":["state-started"]}`, true},
		{"a state name matches nothing", assigned, assignedJSON, `{"status_id_in":["In Progress"]}`, false},
		{"one of the issue's labels", assigned, assignedJSON, `{"has_label_id_in":["label-other","label-agent"]}`, true},
		{"none of the issue's labels", assigned, assignedJSON, `{"has_label_id_in":["label-other"]}`, false},
		{"a label name matches nothing", assigned, assignedJSON, `{"has_label_id_in":["tf-agent"]}`, false},
		{"into a state", changed, changedJSON, `{"new_status_id_in":["state-review"]}`, true},
		{"out of a state", changed, changedJSON, `{"old_status_id_in":["state-todo"]}`, true},
		{"the state it left, as the new one", changed, changedJSON, `{"new_status_id_in":["state-todo"]}`, false},
		{"the label", labeled, labeledJSON, `{"label_id_in":["label-medium"]}`, true},
		{"another label", labeled, labeledJSON, `{"label_id_in":["label-easy"]}`, false},
		{"the label's group", labeled, labeledJSON, `{"label_group_id_in":["group-difficulty"]}`, true},
		{"the group's name matches nothing", labeled, labeledJSON, `{"label_group_id_in":["Difficulty"]}`, false},
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
		domain.EventLinearIssueAssigned:      `{"status":"Todo"}`,
		domain.EventLinearIssueStatusChanged: `{"new_status":"In Review"}`,
		domain.EventLinearIssueLabelAdded:    `{"label_name":"Medium"}`,
		domain.EventLinearIssueBodyUpdated:   `{"has_label":"bug"}`,
	} {
		if _, err := ValidatePredicateJSON(eventType, predicate); err == nil {
			t.Errorf("ValidatePredicateJSON(%s) accepted %s", eventType, predicate)
		}
	}
}
