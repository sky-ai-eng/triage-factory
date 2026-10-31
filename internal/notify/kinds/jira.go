package kinds

import (
	"strconv"
	"strings"

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

// JiraMirrorFailing is open while a rule's in-progress canonical is no longer
// one of its project's statuses, so claimed and delegated tickets stay where
// they are in Jira. It is raised by a move Jira offered no transition for into
// a canonical the project no longer has, never by a ticket that cannot reach a
// status the project still has, and resolved by the next successful move under
// the rule or a save of the team's rules that changes or drops it.
var JiraMirrorFailing = notify.Condition(notify.ConditionSpec[JiraMirror]{
	Name:        "jira.mirror_failing",
	Source:      "jira",
	Priority:    notify.Always(notify.High),
	Dismissible: true,
	Key:         func(m JiraMirror) string { return m.TeamID + ":" + m.JiraProjectID },
	Audience:    notify.TeamAdmins(func(m JiraMirror) string { return m.TeamID }),
	Render: func(m JiraMirror, _ notify.Recipient) notify.Content {
		tickets, workflow, status, issue := "tickets", "the project's workflow", "its in-progress status", ""
		if m.ProjectKey != "" {
			tickets = m.ProjectKey + " tickets"
			workflow = "the " + m.ProjectKey + " workflow"
		}
		if m.Status != "" {
			status = `"` + m.Status + `"`
		}
		if m.IssueKey != "" {
			issue = " (last tried on " + m.IssueKey + ")"
		}
		return notify.Content{
			Title: "Jira can't move " + tickets + " into " + status,
			Body: "Triage Factory moves a ticket into " + status + " when its task is claimed or delegated, " +
				"but that status is no longer in " + workflow + issue + ". Until this is fixed, Jira doesn't " +
				"show which tickets the team is working on. Choose an in-progress status in the team's Jira " +
				"status rules.",
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
	Render: func(p JiraProject, to notify.Recipient) notify.Content {
		project := "a project"
		if p.ProjectKey != "" {
			project = "project " + p.ProjectKey
		}
		return notify.Content{
			Title: "Jira rejects the search for " + project,
			Body: "Jira refuses a query Triage Factory finds this project's new issues with, so new work from " +
				"it is being missed. Usually a status named in a team's rules for the project no longer exists " +
				"in Jira, or the Jira credential can no longer see the project.",
			// Each row is for one of the arming teams, so it links to that
			// team's rules.
			Actions: []notify.Action{notify.Link("Open Jira status rules", notify.Routes.TeamJiraRules(to.TeamID))},
		}
	},
	Example: JiraProject{JiraProjectID: "00000000-0000-0000-0000-000000000000", ProjectKey: "SKY"},
})

// JiraStatusRule is one team's status rules for one project, with the statuses
// they name that the project's workflow no longer has.
type JiraStatusRule struct {
	TeamID        string `json:"team_id"`
	JiraProjectID string `json:"jira_project_id"`
	ProjectKey    string `json:"project_key"` // display only
	// Statuses are the missing statuses as the rules recorded them: each one's
	// name, or its id where the rules hold no name. Display only.
	Statuses []string `json:"statuses"`
	// Stopped is whether a rule Triage Factory acts on is left with none of its
	// statuses, or without the status it moves tickets into.
	Stopped bool `json:"stopped"`
}

// JiraStatusDrift is open while a team's status rules for a project name a
// status the project's workflow no longer has: one deleted in Jira, or retired
// from the workflow. It is high while that leaves a rule with nothing to match
// or move tickets into, and low while every rule still has statuses that work.
// The drift pass raises and resolves it from the workflow it reads, and a
// rules save that reads the workflow resolves it at once. The key is the
// rule's row, so a project key rename is the same occurrence.
var JiraStatusDrift = notify.Condition(notify.ConditionSpec[JiraStatusRule]{
	Name:   "jira.status_drift",
	Source: "jira",
	Priority: notify.PriorityOf(func(d JiraStatusRule) notify.Priority {
		if d.Stopped {
			return notify.High
		}
		return notify.Low
	}),
	Dismissible: true,
	Key:         func(d JiraStatusRule) string { return d.TeamID + ":" + d.JiraProjectID },
	Audience:    notify.TeamAdmins(func(d JiraStatusRule) string { return d.TeamID }),
	Render: func(d JiraStatusRule, _ notify.Recipient) notify.Content {
		project, workflow := "a project", "the project's workflow"
		if d.ProjectKey != "" {
			project, workflow = d.ProjectKey, "the "+d.ProjectKey+" workflow"
		}
		one := len(d.Statuses) == 1
		noun, these, verb, them := "statuses", "These statuses", "are", "them"
		if one {
			noun, these, verb, them = "a status", "This status", "is", "it"
		}
		if len(d.Statuses) > 0 {
			these = quotedList(d.Statuses)
		}
		effect := "Every rule still has statuses the workflow has, so nothing has stopped, but if the " +
			"workflow replaced " + them + ", the replacement isn't mapped."
		if d.Stopped {
			effect = "That leaves a rule Triage Factory uses to find, track or move this project's tickets " +
				"with nothing to match or move them into, so that part of it has stopped working."
		}
		return notify.Content{
			Title: "Jira status rules for " + project + " name " + noun + " the project no longer has",
			Body: these + " " + verb + " no longer in " + workflow + ". " + effect + " Remove " + them +
				" from the team's Jira status rules, where the project shows " + them + " as missing, and map " +
				"the status that replaced " + them + " if there is one.",
			Actions: []notify.Action{notify.Link("Open Jira status rules", notify.Routes.TeamJiraRules(d.TeamID))},
		}
	},
	Example: JiraStatusRule{
		TeamID:        "00000000-0000-0000-0000-000000000000",
		JiraProjectID: "00000000-0000-0000-0000-000000000001",
		ProjectKey:    "SKY",
		Statuses:      []string{"Ready", "QA"},
		Stopped:       true,
	},
})

// JiraHandlerFilter is one Jira task rule's, trigger's or notify rule's filter,
// with the ids it names that are gone.
type JiraHandlerFilter struct {
	HandlerID string `json:"handler_id"`
	TeamID    string `json:"team_id"`
	// HandlerKind is domain.EventHandlerKindRule, EventHandlerKindTrigger or
	// EventHandlerKindNotify.
	HandlerKind string `json:"handler_kind"`
	RuleName    string `json:"rule_name"`  // display only; a trigger has none
	EventType   string `json:"event_type"` // display only
	// The ids the filter names that are gone, by catalog. A status or an issue
	// type is gone when no project the team tracks has it, a priority when the
	// Jira instance has no such priority.
	Statuses   []string `json:"statuses"`
	IssueTypes []string `json:"issue_types"`
	Priorities []string `json:"priorities"`
	// Stopped is whether a field of the filter names nothing that is not gone,
	// so the handler matches no event at all.
	Stopped bool `json:"stopped"`
}

// JiraPredicateDrift is open while an enabled Jira task rule, trigger or
// notify rule filters on a status, issue type or priority that is gone:
// deleted in Jira, dropped from a workflow, or left behind on a project the
// team stopped tracking.
// Nothing about the handler changes, so it silently stops matching what the
// id stood for. It is high while a field names nothing else, so the handler
// matches no event, and low while it still matches the field's other values.
// The key is the handler, so one occurrence lists every gone id it names.
var JiraPredicateDrift = notify.Condition(notify.ConditionSpec[JiraHandlerFilter]{
	Name:   "jira.predicate_drift",
	Source: "jira",
	Priority: notify.PriorityOf(func(d JiraHandlerFilter) notify.Priority {
		if d.Stopped {
			return notify.High
		}
		return notify.Low
	}),
	Dismissible: true,
	Key:         func(d JiraHandlerFilter) string { return d.HandlerID },
	Audience:    notify.TeamAdmins(func(d JiraHandlerFilter) string { return d.TeamID }),
	Render: func(d JiraHandlerFilter, _ notify.Recipient) notify.Content {
		subject := "The trigger on " + eventLabel(d.EventType)
		stopped, working := "It can't fire until the filter is changed.", "It still fires on the filter's other values."
		link := notify.Link("Open trigger", notify.Routes.Trigger(d.HandlerID))
		switch d.HandlerKind {
		case domain.EventHandlerKindRule:
			subject = "A task rule"
			if d.RuleName != "" {
				subject = `Task rule "` + d.RuleName + `"`
			}
			stopped, working = "It matches no event until the filter is changed.", "It still matches the filter's other values."
			link = notify.Link("Open task rule", notify.Routes.TaskRule(d.HandlerID))
		case domain.EventHandlerKindNotify:
			subject = "A notify rule"
			if d.RuleName != "" {
				subject = `Notify rule "` + d.RuleName + `"`
			}
			stopped, working = "It tells no one until the filter is changed.", "It still matches the filter's other values."
			link = notify.Link("Open notify rule", notify.Routes.NotifyRule(d.HandlerID))
		}
		var gone []string
		if part := goneIDs(d.Statuses, "a status", "statuses", "no Jira project this team tracks has"); part != "" {
			gone = append(gone, part)
		}
		if part := goneIDs(d.IssueTypes, "an issue type", "issue types", "no Jira project this team tracks has"); part != "" {
			gone = append(gone, part)
		}
		if part := goneIDs(d.Priorities, "a priority", "priorities", "Jira no longer has"); part != "" {
			gone = append(gone, part)
		}
		body := "Its filter names values that are gone."
		if len(gone) > 0 {
			body = "Its filter names " + joinList(gone) + "."
		}
		if d.Stopped {
			body += " " + stopped
		} else {
			body += " " + working
		}
		body += " The editor shows them as unknown: remove them, and choose what replaced them if Jira has it."
		return notify.Content{
			Title:   subject + " filters on Jira values that are gone",
			Body:    body,
			Actions: []notify.Action{link},
		}
	},
	Example: JiraHandlerFilter{
		HandlerID:   "00000000-0000-0000-0000-000000000000",
		TeamID:      "00000000-0000-0000-0000-000000000001",
		HandlerKind: domain.EventHandlerKindRule,
		RuleName:    "Ready for pickup",
		EventType:   domain.EventJiraIssueStatusChanged,
		Statuses:    []string{"10010", "10011"},
		Priorities:  []string{"4"},
		Stopped:     true,
	},
})

// goneIDs is one catalog's gone ids for a predicate drift's copy, or "" when
// there are none: "a status no Jira project this team tracks has (id 10010)".
func goneIDs(ids []string, one, many, where string) string {
	switch len(ids) {
	case 0:
		return ""
	case 1:
		return one + " " + where + " (id " + ids[0] + ")"
	}
	return strconv.Itoa(len(ids)) + " " + many + " " + where + " (ids " + strings.Join(ids, ", ") + ")"
}

// quotedList is names quoted and joined for copy: `"Ready" and "QA"`.
func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = `"` + n + `"`
	}
	return joinList(quoted)
}

// joinList joins parts as prose: "a", "a and b", "a, b and c".
func joinList(parts []string) string {
	if len(parts) <= 1 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}
