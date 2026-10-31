package kinds

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
)

// JiraMirror is one team's Jira status rule for one project, the rule whose
// in-progress canonical the board mirror moves a claimed or delegated task's
// ticket into.
type JiraMirror struct {
	TeamID        string `json:"team_id"`
	JiraProjectID string `json:"jira_project_id"`
	ProjectKey    string `json:"project_key"` // display only
	Status        string `json:"status"`      // display only
	// IssueKey is the ticket the last failed move was for; display only.
	IssueKey string `json:"issue_key"`
}

// JiraMirrorFor is rule's occurrence, raised by a move of issueKey.
func JiraMirrorFor(rule domain.JiraProjectStatusRules, issueKey string) JiraMirror {
	return JiraMirror{
		TeamID:        rule.TeamID,
		JiraProjectID: rule.JiraProjectID,
		ProjectKey:    rule.ProjectKey,
		Status:        rule.InProgressCanonical.Name,
		IssueKey:      issueKey,
	}
}

// JiraMirrorFailing is open while Jira offers no transition into a rule's
// in-progress canonical, so claimed and delegated tickets stay where they are
// in Jira. It is raised only by that answer, never by a transient failure,
// and resolved by the next successful move under the rule.
var JiraMirrorFailing = notify.Condition(notify.ConditionSpec[JiraMirror]{
	Name:        "jira.mirror_failing",
	Source:      "jira",
	Priority:    notify.Always(notify.High),
	Dismissible: true,
	Key:         func(m JiraMirror) string { return m.TeamID + ":" + m.JiraProjectID },
	Audience:    notify.TeamAdmins(func(m JiraMirror) string { return m.TeamID }),
	Render: func(m JiraMirror, _ notify.Recipient) notify.Content {
		tickets, status, issue := "tickets", "its in-progress status", ""
		if m.ProjectKey != "" {
			tickets = m.ProjectKey + " tickets"
		}
		if m.Status != "" {
			status = `"` + m.Status + `"`
		}
		if m.IssueKey != "" {
			issue = " for " + m.IssueKey
		}
		return notify.Content{
			Title: "Jira won't move " + tickets + " into " + status,
			Body: "Triage Factory moves a ticket into " + status + " when its task is claimed or delegated, " +
				"and Jira offered no transition into it" + issue + ". The status may have been removed from " +
				"the project's workflow, or the workflow doesn't allow that move. Until this is fixed, Jira " +
				"doesn't show which tickets the team is working on. Check the in-progress status in the " +
				"team's Jira status rules.",
			Actions: []notify.Action{notify.Link("Open Jira status rules", notify.Routes.TeamJiraRules(m.TeamID))},
		}
	},
	Example: JiraMirror{
		TeamID:        "00000000-0000-0000-0000-000000000000",
		JiraProjectID: "00000000-0000-0000-0000-000000000001",
		ProjectKey:    "SKY",
		Status:        "In Progress",
		IssueKey:      "SKY-12",
	},
})

// JiraProject is a project TF polls, by its jira_projects registry row.
type JiraProject struct {
	JiraProjectID string `json:"jira_project_id"`
	ProjectKey    string `json:"project_key"` // display only
}

// JiraDiscoveryFailing is open while Jira rejects a query TF finds a project's
// new issues with. It is for the admins of every team whose rules arm the
// project: the queries are built from all of their rules together, so one
// team's dead status stops discovery for each of them. It is dismissible
// because a team may be done with the project. The key is the registry row,
// so a project key rename is the same occurrence.
var JiraDiscoveryFailing = notify.Condition(notify.ConditionSpec[JiraProject]{
	Name:        "jira.discovery_failing",
	Source:      "jira",
	Priority:    notify.Always(notify.High),
	Dismissible: true,
	Key:         func(p JiraProject) string { return p.JiraProjectID },
	Audience:    notify.TeamsArmingJiraProject(func(p JiraProject) string { return p.JiraProjectID }, notify.Admins),
	Render: func(p JiraProject, _ notify.Recipient) notify.Content {
		project := "a project"
		if p.ProjectKey != "" {
			project = "project " + p.ProjectKey
		}
		return notify.Content{
			Title: "Jira rejects the search for " + project,
			Body: "Jira refuses a query Triage Factory finds this project's new issues with, so new work from " +
				"it is being missed. Usually a status named in a team's rules for the project no longer exists " +
				"in Jira, or the Jira credential can no longer see the project.",
			Actions: []notify.Action{notify.Link("Open Jira status rules", notify.Routes.TeamJira())},
		}
	},
	Example: JiraProject{JiraProjectID: "00000000-0000-0000-0000-000000000000", ProjectKey: "SKY"},
})
