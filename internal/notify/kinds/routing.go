package kinds

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
)

// Conflict is one task whose event more than one of the person's teams
// automates: the winner's trigger ran, and each loser's matched and did not,
// because only one team's automation runs on a task.
type Conflict struct {
	TaskID    string `json:"task_id"`
	EventType string `json:"event_type"`
	// Subject is the task's entity key, e.g. owner/repo#18. Display only.
	Subject string          `json:"subject"`
	Winner  ConflictTeam    `json:"winner"`
	Losers  []ConflictLoser `json:"losers"`
}

// ConflictTeam is the team whose automation ran.
type ConflictTeam struct {
	TeamID string `json:"team_id"`
	Name   string `json:"name"` // display only
}

// ConflictLoser is a team whose trigger matched and did not run, and the
// trigger its admins are linked to.
type ConflictLoser struct {
	TeamID    string `json:"team_id"`
	TriggerID string `json:"trigger_id"`
}

// AutomationConflict tells each losing team's admins that another team's
// automation handled a task theirs also matched. Their rule is the one to
// narrow, so they are told and the person the task is about is not: the
// winning team's automation ran as configured. The key is the task, so a
// repeat on the same task coalesces.
var AutomationConflict = notify.Notice(notify.NoticeSpec[Conflict]{
	Name: "routing.automation_conflict",
	TTL:  notify.DefaultNoticeTTL,
	Key:  func(c Conflict) string { return c.TaskID },
	Audience: notify.Teams(func(c Conflict) []string {
		ids := make([]string, len(c.Losers))
		for i, l := range c.Losers {
			ids[i] = l.TeamID
		}
		return ids
	}, notify.Admins),
	Render: func(c Conflict, to notify.Recipient) notify.Content {
		// Local mode's one user stands in for every team; it has one team,
		// so the first loser is the only one.
		var loser ConflictLoser
		if len(c.Losers) > 0 {
			loser = c.Losers[0]
		}
		for _, l := range c.Losers {
			if l.TeamID == to.TeamID {
				loser = l
			}
		}
		winner, theirs := "Another team", "its admins"
		if c.Winner.Name != "" {
			winner, theirs = c.Winner.Name, c.Winner.Name+"'s admins"
		}
		subject := "this task"
		if c.Subject != "" {
			subject = c.Subject
		}
		// The task is about a person or a requested GitHub team, and either
		// can map to several teams, so the copy names neither.
		return notify.Content{
			Title: winner + "'s automation handled " + subject,
			Body: "This team's trigger matched " + subject + " too. " + winner + " and this team both automate " +
				eventLabel(c.EventType) + " for whoever it is about, and only one team's automation runs on a task. " +
				"To choose which one runs, scope this team's trigger more narrowly, or ask " + theirs + " to scope theirs.",
			Actions: []notify.Action{notify.Link("Open this team's trigger", notify.Routes.Trigger(loser.TriggerID))},
		}
	},
	Example: Conflict{
		TaskID:    "00000000-0000-0000-0000-000000000001",
		EventType: domain.EventGitHubPRCICheckFailed,
		Subject:   "acme/widgets#18",
		Winner:    ConflictTeam{TeamID: "00000000-0000-0000-0000-000000000002", Name: "Platform"},
		Losers: []ConflictLoser{
			{TeamID: "00000000-0000-0000-0000-000000000003", TriggerID: "00000000-0000-0000-0000-000000000004"},
		},
	},
})

// eventLabel is an event type's catalog label, quoted, for copy.
func eventLabel(eventType string) string {
	for _, et := range domain.AllEventTypes() {
		if et.ID == eventType {
			return `"` + et.Label + `"`
		}
	}
	return "this event"
}
