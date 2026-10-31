package kinds

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
)

// LinearMirror is one team's rule for one Linear team, the rule whose
// in-progress canonical the board mirror moves a claimed or delegated task's
// issue into.
type LinearMirror struct {
	TeamID        string `json:"team_id"`
	LinearTeamID  string `json:"linear_team_id"`
	LinearTeamKey string `json:"linear_team_key"` // display only
	State         string `json:"state"`           // display only
	// Identifier is the issue the last failed move was for; display only.
	Identifier string `json:"identifier"`
}

// LinearMirrorFor is rule's occurrence, raised by a move of identifier.
func LinearMirrorFor(rule domain.LinearTeamRules, identifier string) LinearMirror {
	return LinearMirror{
		TeamID:        rule.TeamID,
		LinearTeamID:  rule.LinearTeamID,
		LinearTeamKey: rule.LinearTeamKey,
		State:         rule.InProgressCanonical.Name,
		Identifier:    identifier,
	}
}

// LinearMirrorFailing is open while Linear answers a move into a rule's
// in-progress canonical that the state does not exist, so claimed and
// delegated issues stay where they are in Linear. It is raised only by that
// answer, never by a transient failure, and resolved by the next successful
// move under the rule or a save of the team's rules that changes or drops it.
var LinearMirrorFailing = notify.Condition(notify.ConditionSpec[LinearMirror]{
	Name:        "linear.mirror_failing",
	Source:      "linear",
	Priority:    notify.Always(notify.High),
	Dismissible: true,
	Key:         func(m LinearMirror) string { return m.TeamID + ":" + m.LinearTeamID },
	Audience:    notify.TeamAdmins(func(m LinearMirror) string { return m.TeamID }),
	Render: func(m LinearMirror, _ notify.Recipient) notify.Content {
		issues, workflow, state, issue := "issues", "the Linear team's workflow", "its in-progress state", ""
		if m.LinearTeamKey != "" {
			issues = m.LinearTeamKey + " issues"
			workflow = "the " + m.LinearTeamKey + " workflow"
		}
		if m.State != "" {
			state = `"` + m.State + `"`
		}
		if m.Identifier != "" {
			issue = " (last tried on " + m.Identifier + ")"
		}
		return notify.Content{
			Title: "Linear can't move " + issues + " into " + state,
			Body: "Triage Factory moves an issue into " + state + " when its task is claimed or delegated, " +
				"and Linear says " + workflow + " no longer has that state" + issue + ". Until this is fixed, " +
				"Linear doesn't show which issues the team is working on. Choose an in-progress state in the " +
				"team's Linear rules.",
			Actions: []notify.Action{notify.Link("Open Linear rules", notify.Routes.TeamLinearRules(m.TeamID))},
		}
	},
	Example: LinearMirror{
		TeamID:        "00000000-0000-0000-0000-000000000000",
		LinearTeamID:  "00000000-0000-0000-0000-000000000001",
		LinearTeamKey: "ENG",
		State:         "In Progress",
		Identifier:    "ENG-12",
	},
})

// LinearTeam is an armed Linear team the org's Linear credential cannot read.
type LinearTeam struct {
	LinearTeamID string `json:"linear_team_id"`
	TeamKey      string `json:"team_key"` // display only
}

// LinearTeamUnreadable is open while an armed Linear team is one the org's
// credential cannot read, so its tracked issues are held unrefreshed and
// nothing new is discovered in it. Fixing the credential is the org admins';
// stopping tracking is the tracking teams' admins', so a team row carries the
// action that does it. Dismissible, because Linear is a source an org may stop
// caring about.
var LinearTeamUnreadable = notify.Condition(notify.ConditionSpec[LinearTeam]{
	Name:        "linear.team_unreadable",
	Source:      "linear",
	Priority:    notify.Always(notify.High),
	Dismissible: true,
	Key:         func(t LinearTeam) string { return t.LinearTeamID },
	Audience: notify.Audiences(
		notify.OrgAdmins(),
		notify.TeamsTrackingLinearTeam(func(t LinearTeam) string { return t.LinearTeamID }, notify.Admins),
	),
	Render: func(t LinearTeam, to notify.Recipient) notify.Content {
		key := t.TeamKey
		if key == "" {
			key = t.LinearTeamID
		}
		c := notify.Content{Title: "Linear team " + key + " can't be read"}
		settings := notify.Link("Open Linear settings", notify.Routes.OrgSettings())
		if to.TeamID == "" {
			c.Body = "The organization's Linear connection can't read this team, so Triage Factory isn't seeing " +
				"changes to its issues or finding new ones. Share the team with the Linear app on its page in " +
				"Linear, or connect an API key whose owner is a member. If the team was deleted in Linear, the " +
				"teams tracking it can stop tracking it; its issues then retire."
			c.Actions = []notify.Action{settings}
			return c
		}
		// Untracking is what a team admin can do here, so it is the primary
		// action; the settings page is an org admin's.
		c.Body = "The organization's Linear connection can't read this team, so Triage Factory isn't seeing " +
			"changes to its issues or finding new ones for this team. An org admin can fix the connection. " +
			"If the team was deleted in Linear, stop tracking it; its issues then retire."
		c.Actions = []notify.Action{
			notify.Invoke("untrack", "Stop tracking "+key, notify.RequiresTeamAdmin, notify.Confirm(
				"This team stops tracking the Linear team "+key+", and its state mappings for it are deleted. "+
					"Unless another team has "+key+"'s states mapped, Triage Factory stops holding its issues, "+
					"so the ones it can't read can retire and their tasks close.")),
			settings,
		}
		return c
	},
	Example: LinearTeam{LinearTeamID: "00000000-0000-0000-0000-000000000000", TeamKey: "ENG"},
})
