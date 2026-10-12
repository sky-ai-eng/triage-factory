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
		issue.Fields.Assignee = &jiraclient.IssueUser{DisplayName: "Aidan Allchin", AccountID: accountID, Key: key, Name: name}
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

// TestIssueToState_Reporter locks the reporter to the assignee's precedence,
// so a reporter_in allowlist built from user_jira_identities matches on Cloud
// and on Data Center alike, and leaves both fields empty when the issue has no
// reporter.
func TestIssueToState_Reporter(t *testing.T) {
	cases := []struct {
		desc     string
		reporter *jiraclient.IssueUser
		wantName string
		wantID   string
	}{
		{"cloud prefers accountId", &jiraclient.IssueUser{DisplayName: "Ada Lovelace", AccountID: "557058:abc-ada", Key: "JIRAUSER10200", Name: "ada@corp.com"}, "Ada Lovelace", "557058:abc-ada"},
		{"server/dc falls back to key, not name", &jiraclient.IssueUser{DisplayName: "Ada Lovelace", Key: "JIRAUSER10200", Name: "ada@corp.com"}, "Ada Lovelace", "JIRAUSER10200"},
		{"name only when no stable id", &jiraclient.IssueUser{DisplayName: "Ada Lovelace", Name: "ada@corp.com"}, "Ada Lovelace", "ada@corp.com"},
		{"no reporter", nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			var issue jiraclient.Issue
			issue.Key = "SKY-1"
			issue.Fields.Reporter = tc.reporter
			got := issueToState(issue, "https://jira.corp.com", nil).Snap
			if got.Reporter != tc.wantName || got.ReporterAccountID != tc.wantID {
				t.Errorf("reporter = (%q, %q), want (%q, %q)", got.Reporter, got.ReporterAccountID, tc.wantName, tc.wantID)
			}
		})
	}
}
