package tracker

import (
	"testing"

	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// TestIssueToState_AssigneeAccountIDPrecedence locks the stable-id precedence
// the snapshot derives for an issue assignee: accountId (Cloud) → key
// (Server/DC) → name (last resort). This MUST match auth.JiraUser.StableID(),
// because user_jira_identities.account_id is written from StableID() and the
// router's assignee-centric owner ladder joins the event's assignee_account_id
// against it. A Server/DC regression where the snapshot fell back to the
// mutable username (name) while the bound identity held the key silently broke
// that join — events landed but no task was ever created.
func TestIssueToState_AssigneeAccountIDPrecedence(t *testing.T) {
	mk := func(accountID, key, name string) jiraclient.Issue {
		var issue jiraclient.Issue
		issue.Key = "SKY-1"
		issue.Fields.Summary = "test"
		issue.Fields.Assignee = &struct {
			DisplayName string `json:"displayName"`
			AccountID   string `json:"accountId"`
			Key         string `json:"key"`
			Name        string `json:"name"`
		}{DisplayName: "Aidan Allchin", AccountID: accountID, Key: key, Name: name}
		return issue
	}

	cases := []struct {
		desc      string
		accountID string
		key       string
		name      string
		want      string
	}{
		{"cloud prefers accountId", "557058:abc-aidan", "JIRAUSER10100", "aidan.allchin@corp.com", "557058:abc-aidan"},
		{"server/dc falls back to key, not name", "", "JIRAUSER10100", "aidan.allchin@corp.com", "JIRAUSER10100"},
		{"name only when no stable id", "", "", "aidan.allchin@corp.com", "aidan.allchin@corp.com"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := issueToState(mk(tc.accountID, tc.key, tc.name), "https://jira.corp.com", nil)
			if got.Snap.AssigneeAccountID != tc.want {
				t.Errorf("AssigneeAccountID = %q, want %q", got.Snap.AssigneeAccountID, tc.want)
			}
		})
	}
}
