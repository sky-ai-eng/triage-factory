package kinds

import "github.com/sky-ai-eng/triage-factory/internal/notify"

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
					"Unless another team tracks "+key+", Triage Factory stops following its issues and closes their tasks.")),
			settings,
		}
		return c
	},
	Example: LinearTeam{LinearTeamID: "00000000-0000-0000-0000-000000000000", TeamKey: "ENG"},
})
