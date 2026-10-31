package kinds

import "github.com/sky-ai-eng/triage-factory/internal/notify"

// JiraProject is a project TF polls, by its jira_projects registry row.
type JiraProject struct {
	JiraProjectID string `json:"jira_project_id"`
	ProjectKey    string `json:"project_key"` // display only
}

// JiraDiscoveryFailing is open while Jira rejects the query TF finds a
// project's new issues with, so the project produces no new work. It is for
// the admins of the teams whose rules arm the project, since a rule is what
// the query is built from, and is dismissible because a team may be done with
// the project. The key is the registry row, so a project key rename is the
// same occurrence.
var JiraDiscoveryFailing = notify.Condition(notify.ConditionSpec[JiraProject]{
	Name:        "jira.discovery_failing",
	Source:      "jira",
	Priority:    notify.Always(notify.High),
	Dismissible: true,
	Key:         func(p JiraProject) string { return p.JiraProjectID },
	Audience:    notify.TeamsTrackingJiraProject(func(p JiraProject) string { return p.JiraProjectID }, notify.Admins),
	Render: func(p JiraProject, _ notify.Recipient) notify.Content {
		project := "a project"
		if p.ProjectKey != "" {
			project = "project " + p.ProjectKey
		}
		return notify.Content{
			Title: "Jira rejects the search for " + project,
			Body: "Triage Factory finds no new issues in the project while Jira refuses its query. Usually a " +
				"status in the project's status rules no longer exists in Jira, or the Jira credential can no " +
				"longer see the project.",
			Actions: []notify.Action{notify.Link("Open Jira status rules", notify.Routes.TeamJira())},
		}
	},
	Example: JiraProject{JiraProjectID: "00000000-0000-0000-0000-000000000000", ProjectKey: "SKY"},
})
