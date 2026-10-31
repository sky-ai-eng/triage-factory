package kinds

import "github.com/sky-ai-eng/triage-factory/internal/notify"

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
