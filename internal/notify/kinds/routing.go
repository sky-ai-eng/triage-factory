package kinds

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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

// TriggerRefusal is one firing of a trigger that the spawner's plan refused,
// so no run started. Its cause is a standing condition, and the firing was
// dropped rather than retried.
type TriggerRefusal struct {
	TriggerID string `json:"trigger_id"`
	// TeamID is the trigger's team.
	TeamID string `json:"team_id"`
	// EventType is the event the trigger fires on. Display only.
	EventType string `json:"event_type"`
	// Subject is the task's entity key, e.g. owner/repo#18. Display only.
	Subject string `json:"subject"`
	// Cause is one of the domain.DelegationRefused* causes.
	Cause string `json:"cause"`
	// TeamCap is whether the spend cap that refused the run is a team's
	// rather than the organization's. The spend_cap cause only.
	TeamCap bool `json:"team_cap,omitempty"`
	// CapTeamName is the capped team's name, for a team's cap. Display only.
	CapTeamName string `json:"cap_team_name,omitempty"`
	// Detail is the refusal's own message, for a model or a provider cause,
	// which names the model, the provider, and the setting to change. The
	// copy shows it for those causes alone: a spend cap's message carries the
	// day's figures, which are an org admin's read.
	Detail string `json:"detail,omitempty"`
}

// TriggerRefused tells a trigger's team admins that a firing of it started no
// run: the spawner's plan refused it for a spend cap, the team's models, a
// provider the org has not connected, or a missing blueprint or prompt. The
// firing is dropped, so it is something that happened once, a notice; the key
// is the trigger and the cause, so the refusals of every later firing coalesce
// into it while the cause stands. Admins, because each cause is a setting.
var TriggerRefused = notify.Notice(notify.NoticeSpec[TriggerRefusal]{
	Name:     "routing.trigger_refused",
	Priority: notify.Always(notify.High),
	TTL:      notify.DefaultNoticeTTL,
	Key:      func(r TriggerRefusal) string { return r.TriggerID + ":" + r.Cause },
	Audience: notify.TeamAdmins(func(r TriggerRefusal) string { return r.TeamID }),
	Render:   renderTriggerRefused,
	Example: TriggerRefusal{
		TriggerID: "00000000-0000-0000-0000-000000000004",
		TeamID:    "00000000-0000-0000-0000-000000000003",
		EventType: domain.EventGitHubPRCICheckFailed,
		Subject:   "acme/widgets#18",
		Cause:     domain.DelegationRefusedProviderNotConnected,
		Detail:    "cannot dispatch: model's provider is not connected: claude-sonnet-5 is served by Anthropic, which this organization has not connected — connect it in Settings → Claude credentials",
	},
})

// renderTriggerRefused is TriggerRefused's copy, the same for every admin.
func renderTriggerRefused(r TriggerRefusal, _ notify.Recipient) notify.Content {
	subject := "this task"
	if r.Subject != "" {
		subject = r.Subject
	}
	return notify.Content{
		Title: "Automation could not start on " + subject,
		Body: "This team's trigger for " + eventLabel(r.EventType) + " matched " + subject + ", and no run started. " +
			r.reason() + " The run is not retried. Once this is fixed, the trigger runs on its next event, or the task can be delegated by hand.",
		Actions: []notify.Action{notify.Link("Open the trigger", notify.Routes.Trigger(r.TriggerID))},
	}
}

// reason is the sentence that says why the plan refused the run.
func (r TriggerRefusal) reason() string {
	switch r.Cause {
	case domain.DelegationRefusedSpendCap:
		capped := "The organization's daily spend cap"
		switch {
		case r.TeamCap && r.CapTeamName != "":
			capped = "Team " + r.CapTeamName + "'s daily spend cap"
		case r.TeamCap:
			capped = "The team's daily spend cap"
		}
		return capped + " is reached. An org admin can raise it in Settings."
	case domain.DelegationRefusedNoDefaultModel, domain.DelegationRefusedProviderNotConnected, domain.DelegationRefusedModelNotEnabled:
		if r.Detail != "" {
			return sentence(r.Detail)
		}
		return "The team's model settings refused it. Check its models in Settings."
	case domain.DelegationRefusedBlueprintUnusable:
		return "The trigger's blueprint, or one of its prompts, is missing."
	}
	return "Its settings refused it."
}

// sentence renders an error message as a sentence: capitalized, and ending in
// a full stop.
func sentence(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return msg
	}
	first, size := utf8.DecodeRuneInString(msg)
	msg = string(unicode.ToUpper(first)) + msg[size:]
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}

// Breaker is one trigger's breaker, tripped on one entity: the runs the
// trigger fired there kept failing, so it fires there no more.
type Breaker struct {
	TriggerID string `json:"trigger_id"`
	EntityID  string `json:"entity_id"`
	// TeamID is the trigger's team.
	TeamID string `json:"team_id"`
	// EventType is the event the trigger fires on. Display only.
	EventType string `json:"event_type"`
	// Subject is the entity's key, e.g. owner/repo#18. Display only.
	Subject string `json:"subject"`
	// Failures is how many runs in a row failed when it was last raised.
	// Display only.
	Failures int `json:"failures"`
	// ConversationID is a conversation of the run that failed last, the one
	// its link opens; empty when none was read.
	ConversationID string `json:"conversation_id"`
}

// BreakerTripped is open while a trigger's breaker holds on an entity: its
// runs there failed breaker_threshold times in a row, so its events there fire
// nothing. It opens where the gate refuses a firing, and it is for the
// trigger's team's members, any of whom can start the run that resets it. It
// resolves when a run a person started on the entity completes, the entity
// closes, or the trigger is turned off, deleted or given a threshold above the
// count. One failed run is not a notification: the board shows it.
var BreakerTripped = notify.Condition(notify.ConditionSpec[Breaker]{
	Name:        "routing.breaker_tripped",
	Priority:    notify.Always(notify.High),
	Dismissible: true,
	Key:         func(b Breaker) string { return b.TriggerID + ":" + b.EntityID },
	Audience:    notify.TeamMembers(func(b Breaker) string { return b.TeamID }),
	Render: func(b Breaker, _ notify.Recipient) notify.Content {
		subject := b.subject()
		failed := "kept failing"
		switch {
		case b.Failures == 1:
			failed = "failed"
		case b.Failures > 1:
			failed = "failed " + strconv.Itoa(b.Failures) + " times in a row"
		}
		c := notify.Content{
			Title: "Automation stopped on " + subject,
			Body: "This team's trigger for " + eventLabel(b.EventType) + " " + failed + " on " + subject +
				", so Triage Factory no longer runs it there. Once a run someone starts on " + subject +
				" completes, the trigger runs there again.",
		}
		if b.ConversationID != "" {
			c.Actions = append(c.Actions, notify.Link("Open the last failed run", notify.Routes.RunStation(b.ConversationID)))
		}
		c.Actions = append(c.Actions, notify.Link("Open the trigger", notify.Routes.Trigger(b.TriggerID)))
		return c
	},
	Resolved: func(b Breaker, r notify.Resolution) string {
		if r.Reason == domain.NotificationResolvedAudienceRemoved {
			return noLongerApplies(r.Recipient)
		}
		return "The trigger runs on " + b.subject() + " again: a run someone started there completed, it closed, " +
			"or the trigger was turned off, deleted, or given a higher breaker threshold."
	},
	Example: Breaker{
		TriggerID:      "00000000-0000-0000-0000-000000000004",
		EntityID:       "00000000-0000-0000-0000-000000000005",
		TeamID:         "00000000-0000-0000-0000-000000000003",
		EventType:      domain.EventGitHubPRCICheckFailed,
		Subject:        "acme/widgets#18",
		Failures:       4,
		ConversationID: "00000000-0000-0000-0000-000000000006",
	},
})

// subject is how copy names the entity.
func (b Breaker) subject() string {
	if b.Subject != "" {
		return b.Subject
	}
	return "this task"
}

// eventLabel is an event type's catalog label, quoted, for copy.
func eventLabel(eventType string) string {
	if label, ok := catalogLabel(eventType); ok {
		return `"` + label + `"`
	}
	return "this event"
}
