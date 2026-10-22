package events

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// No predicate offers by name what its source identifies by id, which a
// rename would leave matching nothing, and each Jira event whose metadata
// names a status, priority or issue type offers its id list instead.
func TestPredicates_NameFieldsReplacedByIDs(t *testing.T) {
	names := map[string][]string{
		"jira:":   {"status", "new_status", "old_status", "priority", "new_priority", "old_priority", "issue_type"},
		"linear:": {"status", "new_status", "old_status", "has_label", "label_name", "label_group"},
	}
	for eventType, s := range All() {
		for prefix, banned := range names {
			if !strings.HasPrefix(eventType, prefix) {
				continue
			}
			for _, f := range s.Fields {
				if slices.Contains(banned, f.Name) {
					t.Errorf("%s still offers %s", eventType, f.Name)
				}
			}
		}
	}
	for eventType, fields := range map[string][]string{
		domain.EventJiraIssueAssigned:        {"status_id_in", "priority_id_in", "issue_type_id_in"},
		domain.EventJiraIssueAvailable:       {"status_id_in", "priority_id_in", "issue_type_id_in"},
		domain.EventJiraIssueBecameAtomic:    {"status_id_in", "priority_id_in", "issue_type_id_in"},
		domain.EventJiraIssueStatusChanged:   {"new_status_id_in", "old_status_id_in", "issue_type_id_in"},
		domain.EventJiraIssuePriorityChanged: {"new_priority_id_in", "old_priority_id_in"},
		domain.EventJiraIssueCompleted:       {"issue_type_id_in"},
		domain.EventJiraIssueUnreachable:     {"issue_type_id_in"},
		domain.EventJiraIssueBodyUpdated:     {"issue_type_id_in"},
	} {
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

// The type-erased matcher compares each id field to the event's id exactly:
// the name a caller might reach for matches nothing, and so does an event
// recorded without the id.
func TestMatcher_IDFields(t *testing.T) {
	type event struct {
		eventType string
		meta      any
	}
	jiraAssigned := event{domain.EventJiraIssueAssigned, JiraIssueAssignedMetadata{
		IssueKey: "SKY-1", JiraProjectID: "jp-sky", IssueType: "Bug", IssueTypeID: "10004",
		Priority: "High", PriorityID: "2", Status: "In Progress", StatusID: "10001",
	}}
	jiraStatus := event{domain.EventJiraIssueStatusChanged, JiraIssueStatusChangedMetadata{
		IssueKey: "SKY-1", JiraProjectID: "jp-sky", OldStatus: "To Do", OldStatusID: "10000", NewStatus: "In Review", NewStatusID: "10003",
	}}
	jiraPriority := event{domain.EventJiraIssuePriorityChanged, JiraIssuePriorityChangedMetadata{
		IssueKey: "SKY-1", JiraProjectID: "jp-sky", OldPriority: "Low", OldPriorityID: "4", NewPriority: "High", NewPriorityID: "2",
	}}
	identity := LinearIssueIdentity{IssueIdentifier: "ENG-1", LinearTeamID: "team-eng"}
	linearAssigned := event{domain.EventLinearIssueAssigned, LinearIssueAssignedMetadata{
		LinearIssueIdentity: identity, Status: "In Progress", StatusID: "state-started",
		Labels: []string{"bug", "tf-agent"}, LabelIDs: []string{"label-bug", "label-agent"},
	}}
	linearStatus := event{domain.EventLinearIssueStatusChanged, LinearIssueStatusChangedMetadata{
		LinearIssueIdentity: identity, OldStatus: "Todo", OldStatusID: "state-todo", NewStatus: "In Review", NewStatusID: "state-review",
	}}
	linearLabel := event{domain.EventLinearIssueLabelAdded, LinearIssueLabelMetadata{
		LinearIssueIdentity: identity, LabelID: "label-medium", LabelName: "Medium", LabelGroupID: "group-difficulty", LabelGroup: "Difficulty",
		Labels: []string{"Medium"}, LabelIDs: []string{"label-medium"},
	}}
	linearParent := event{domain.EventLinearIssueParentChanged, LinearIssueParentChangedMetadata{
		LinearIssueIdentity: identity, OldParentID: "uuid-old", NewParentID: "uuid-new", OldParent: "ENG-9", NewParent: "OPS-4",
	}}
	approved := event{domain.EventGitHubPRReviewApproved, GitHubPRReviewApprovedMetadata{Author: "aidan", AuthorID: "1", Reviewer: "alice", ReviewerID: "5"}}
	changesNoID := event{domain.EventGitHubPRReviewChangesRequested, GitHubPRReviewChangesRequestedMetadata{Author: "aidan", AuthorID: "1", Reviewer: "alice"}}

	for _, tc := range []struct {
		name  string
		event event
		pred  string
		want  bool
	}{
		{"jira status id", jiraAssigned, `{"status_id_in":["10000","10001"]}`, true},
		{"a jira status name", jiraAssigned, `{"status_id_in":["In Progress"]}`, false},
		{"jira priority id", jiraAssigned, `{"priority_id_in":["2"]}`, true},
		{"a jira priority name", jiraAssigned, `{"priority_id_in":["High"]}`, false},
		{"jira issue type id", jiraAssigned, `{"issue_type_id_in":["10004"]}`, true},
		{"a jira issue type name", jiraAssigned, `{"issue_type_id_in":["Bug"]}`, false},
		{"into a jira status", jiraStatus, `{"new_status_id_in":["10003"]}`, true},
		{"the jira status it left, as the new one", jiraStatus, `{"new_status_id_in":["10000"]}`, false},
		{"out of a jira status", jiraStatus, `{"old_status_id_in":["10000"]}`, true},
		{"into one jira status and out of another", jiraStatus, `{"new_status_id_in":["10003"],"old_status_id_in":["10001"]}`, false},
		{"to a jira priority", jiraPriority, `{"new_priority_id_in":["2"]}`, true},
		{"from a jira priority", jiraPriority, `{"old_priority_id_in":["4"]}`, true},
		{"the jira priority it left, as the new one", jiraPriority, `{"new_priority_id_in":["4"]}`, false},
		{"linear state id", linearAssigned, `{"status_id_in":["state-started"]}`, true},
		{"a linear state name", linearAssigned, `{"status_id_in":["In Progress"]}`, false},
		{"one of the issue's linear labels", linearAssigned, `{"has_label_id_in":["label-other","label-agent"]}`, true},
		{"a linear label name", linearAssigned, `{"has_label_id_in":["tf-agent"]}`, false},
		{"into a linear state", linearStatus, `{"new_status_id_in":["state-review"]}`, true},
		{"out of a linear state", linearStatus, `{"old_status_id_in":["state-todo"]}`, true},
		{"the linear state it left, as the new one", linearStatus, `{"new_status_id_in":["state-todo"]}`, false},
		{"the linear label", linearLabel, `{"label_id_in":["label-medium"]}`, true},
		{"another linear label", linearLabel, `{"label_id_in":["label-easy"]}`, false},
		{"the linear label's group", linearLabel, `{"label_group_id_in":["group-difficulty"]}`, true},
		{"the linear group's name", linearLabel, `{"label_group_id_in":["Difficulty"]}`, false},
		{"the new linear parent's id", linearParent, `{"new_parent_id":"uuid-new"}`, true},
		{"the old linear parent's id", linearParent, `{"new_parent_id":"uuid-old"}`, false},
		{"the new linear parent's identifier", linearParent, `{"new_parent_id":"OPS-4"}`, false},
		{"a linear parent id in another case", linearParent, `{"new_parent_id":"UUID-NEW"}`, false},
		{"the reviewer's github id", approved, `{"reviewer_id_in":["5"]}`, true},
		{"the reviewer's github login", approved, `{"reviewer_id_in":["alice"]}`, false},
		{"the author's github id as reviewer", approved, `{"reviewer_id_in":["1"]}`, false},
		{"a review recorded without the reviewer's id", changesNoID, `{"reviewer_id_in":["5"]}`, false},
		{"the author's github id", changesNoID, `{"author_id_in":["1"]}`, true},
	} {
		s, _ := Get(tc.event.eventType)
		meta, _ := json.Marshal(tc.event.meta)
		got, err := s.Match(tc.pred, string(meta))
		if err != nil {
			t.Fatalf("%s: Match error: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: Match = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A name-valued field is an unknown field: refused at validation, and by the
// matcher for a stored predicate that carries one, rather than read as
// match-all.
func TestPredicates_NameFieldsRefused(t *testing.T) {
	for _, tc := range []struct{ eventType, pred string }{
		{domain.EventGitHubPRNewCommits, `{"author_in":["aidan"]}`},
		{domain.EventGitHubPRNewCommits, `{"author":"aidan"}`},
		{domain.EventGitHubPRReviewApproved, `{"reviewer_in":["alice"]}`},
		{domain.EventGitHubPRReviewApproved, `{"reviewer":"alice"}`},
		{domain.EventGitHubPRReviewRequested, `{"requested_login":"alice"}`},
		{domain.EventGitHubPRReviewRequested, `{"requested_team":"acme/backend"}`},
		{domain.EventGitHubPRReviewRequestRemoved, `{"requested_login":"alice"}`},
		{domain.EventGitHubPRCommented, `{"commenter_in":["alice"]}`},
		{domain.EventGitHubPRMentioned, `{"commenter_in":["alice"]}`},
		{domain.EventGitHubPRMentioned, `{"commenter":"alice"}`},
		{domain.EventGitHubPRBodyUpdated, `{"author_in":["aidan"]}`},
		{domain.EventJiraIssueAssigned, `{"status":"In Progress"}`},
		{domain.EventJiraIssueAvailable, `{"issue_type":"Bug"}`},
		{domain.EventJiraIssueStatusChanged, `{"new_status":"In Review"}`},
		{domain.EventJiraIssuePriorityChanged, `{"new_priority":"High"}`},
		{domain.EventLinearIssueAssigned, `{"status":"Todo"}`},
		{domain.EventLinearIssueStatusChanged, `{"new_status":"In Review"}`},
		{domain.EventLinearIssueLabelAdded, `{"label_name":"Medium"}`},
		{domain.EventLinearIssueBodyUpdated, `{"has_label":"bug"}`},
		{domain.EventLinearIssueParentChanged, `{"new_parent":"OPS-4"}`},
	} {
		if _, err := ValidatePredicateJSON(tc.eventType, tc.pred); err == nil {
			t.Errorf("ValidatePredicateJSON(%s) accepted %s", tc.eventType, tc.pred)
		}
		if s, _ := Get(tc.eventType); s.Match == nil {
			t.Errorf("%s has no schema", tc.eventType)
		} else if _, err := s.Match(tc.pred, `{}`); err == nil {
			t.Errorf("%s matched %s without error", tc.eventType, tc.pred)
		}
	}
	if _, err := ValidatePredicateJSON(domain.EventGitHubPRMentioned, `{"mentioned_login":"bob","commenter_id_in":["5"]}`); err != nil {
		t.Errorf("mentioned_login beside commenter_id_in refused: %v", err)
	}
}
