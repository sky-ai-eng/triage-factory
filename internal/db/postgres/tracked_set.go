package postgres

import (
	"fmt"
	"strings"
)

// --- Tracked-set membership ------------------------------------------
//
// In multi mode an entity belongs to a team's flow iff it sits in that
// team's tracked set: a GitHub entity whose repository is one of the team's
// tracked repos (team_github_repos), a Jira entity whose project is attached
// to the team (jira_project_status_rules), a Linear entity whose Linear team
// the team tracks (linear_team_rules), or a Slack thread in a channel the
// team watches (team_slack_channels). Whether a task was ever minted on the
// entity does not enter it. The entity and event lists, the team activity
// node, and the team pull request list all read it. SQLite/local is N=1 and
// stays unscoped.
//
// The team scoping is free under the app pool (tf_app, RLS active): the
// team_github_repos_select, jira_rules_select, linear_rules_select and
// team_slack_channels_select policies already constrain visible rows to the
// viewer's team memberships, so each EXISTS auto-scopes to the viewer's
// teams with no explicit team_id — the same RLS-does-the-scoping pattern
// ListActiveJiraTeamScoped uses for the Jira discovery deck. The org bind
// (via e.org_id) is defense-in-depth so the filter still holds on the admin
// pool where RLS is bypassed: team_github_repos, jira_project_status_rules
// and team_slack_channels bind it on their own org_id, linear_team_rules
// through the teams join.

// githubRepoTrackedExists scopes a github entities row (alias e) to the
// tracked repos of the viewer's teams.
//
// The entity and the tracking row both reference the repository's registry
// row, so they meet on its id. A rename moves only the registry row's name,
// and a registry row lives on one GitHub host, so a pull request matches
// tracking only on its own host without the host being compared.
const githubRepoTrackedExists = `EXISTS (
	SELECT 1 FROM team_github_repos g
	WHERE g.org_id = e.org_id
	  AND g.repository_id = e.repository_id
)`

// jiraProjectTrackedExists scopes a jira entities row (alias e) to the
// projects attached to the viewer's teams. Mirrors
// jiraTeamProjectMembershipExists (the Jira deck's semi-join): the entity and
// the rule both reference the project's registry row, so they meet on its id,
// which a project key rename does not move, and the column outlives the
// snapshot, so a row stays in its teams' tracked set while its source is
// turned off.
const jiraProjectTrackedExists = `EXISTS (
	SELECT 1 FROM jira_project_status_rules jr
	WHERE jr.org_id = e.org_id
	  AND jr.jira_project_id = e.jira_project_id
)`

// linearTeamTrackedExists scopes a linear entities row (alias e) to the Linear
// teams attached to the viewer's teams. The entity's Linear team is
// entities.linear_team_id, the UUID linear_team_rules is keyed by — not the
// identifier's key prefix, which a team key rename in Linear changes. The
// column outlives the snapshot, so a row stays in its teams' tracked set while
// its source is turned off. Only rules saved under the org's current Linear
// workspace count, as everywhere else rules are read: a rule from a workspace
// the org has moved off names a team no current issue belongs to.
const linearTeamTrackedExists = `EXISTS (
	SELECT 1 FROM linear_team_rules lr
	JOIN teams tm ON tm.id = lr.team_id
	WHERE tm.org_id = e.org_id
	  AND lr.linear_team_id = e.linear_team_id
	  AND lr.linear_workspace_id = (SELECT os.linear_workspace_id FROM org_settings os WHERE os.org_id = e.org_id)
)`

// slackChannelTrackedExists scopes a slack entities row (alias e) to the
// channels the viewer's teams watch. A thread entity references its channel's
// registry row (entities.slack_channel_id), the row a team_slack_channels row
// binds a team to, so the two meet on its id, which a channel id change in
// Slack does not move. Every team watching the channel matches, primary or
// not: the primary flag decides whose task a mention becomes, not whose flow
// the channel's traffic is.
const slackChannelTrackedExists = `EXISTS (
	SELECT 1 FROM team_slack_channels sc
	WHERE sc.org_id = e.org_id
	  AND sc.slack_channel_id = e.slack_channel_id
)`

// githubRepoTrackedForTeams / jiraProjectTrackedForTeams /
// linearTeamTrackedForTeams / slackChannelTrackedForTeams are the
// team-narrowed variants: the correlated tracked-set row must additionally
// belong to one of the teams in placeholders (the comma-joined "$3, $4" list
// bound to team ids). Each branch stays RLS-scoped to the viewer under tf_app
// (a forged team id the caller isn't on is filtered by the SELECT policy) and
// org-bound for the admin pool the way its unnarrowed twin is — the same
// shape as jiraTeamProjectMembershipForTeam, widened to an IN list.
func githubRepoTrackedForTeams(placeholders string) string {
	return `EXISTS (
		SELECT 1 FROM team_github_repos g
		WHERE g.org_id = e.org_id
		  AND g.team_id IN (` + placeholders + `)
		  AND g.repository_id = e.repository_id
	)`
}

func jiraProjectTrackedForTeams(placeholders string) string {
	return `EXISTS (
		SELECT 1 FROM jira_project_status_rules jr
		WHERE jr.org_id = e.org_id
		  AND jr.team_id IN (` + placeholders + `)
		  AND jr.jira_project_id = e.jira_project_id
	)`
}

func linearTeamTrackedForTeams(placeholders string) string {
	return `EXISTS (
		SELECT 1 FROM linear_team_rules lr
		JOIN teams tm ON tm.id = lr.team_id
		WHERE tm.org_id = e.org_id
		  AND lr.team_id IN (` + placeholders + `)
		  AND lr.linear_team_id = e.linear_team_id
		  AND lr.linear_workspace_id = (SELECT os.linear_workspace_id FROM org_settings os WHERE os.org_id = e.org_id)
	)`
}

func slackChannelTrackedForTeams(placeholders string) string {
	return `EXISTS (
		SELECT 1 FROM team_slack_channels sc
		WHERE sc.org_id = e.org_id
		  AND sc.team_id IN (` + placeholders + `)
		  AND sc.slack_channel_id = e.slack_channel_id
	)`
}

// trackedArm is one source's arm of the tracked set. Every read scoped by the
// tracked set builds its SQL from trackedArms, so a source gains a tracked set
// by gaining a row there.
type trackedArm struct {
	source   string
	exists   string
	forTeams func(placeholders string) string
	// unitKey is the entities column naming the row's unit, the column the
	// source's tracking rows reference.
	unitKey string
	// units selects the unit ids of the teams in placeholders, with $1 bound
	// to the org, under the same filters as exists, so a unit listed is
	// exactly a unit whose entities exists admits.
	units func(placeholders string) string
}

var trackedArms = []trackedArm{
	{
		source:   "github",
		exists:   githubRepoTrackedExists,
		forTeams: githubRepoTrackedForTeams,
		unitKey:  `e.repository_id::text`,
		units: func(ph string) string {
			return `g.repository_id::text FROM team_github_repos g WHERE g.org_id = $1 AND g.team_id IN (` + ph + `)`
		},
	},
	{
		source:   "jira",
		exists:   jiraProjectTrackedExists,
		forTeams: jiraProjectTrackedForTeams,
		unitKey:  `e.jira_project_id::text`,
		units: func(ph string) string {
			return `jr.jira_project_id::text FROM jira_project_status_rules jr WHERE jr.org_id = $1 AND jr.team_id IN (` + ph + `)`
		},
	},
	{
		source:   "linear",
		exists:   linearTeamTrackedExists,
		forTeams: linearTeamTrackedForTeams,
		unitKey:  `e.linear_team_id`,
		units: func(ph string) string {
			return `lr.linear_team_id FROM linear_team_rules lr
	JOIN teams tm ON tm.id = lr.team_id
	WHERE tm.org_id = $1 AND lr.team_id IN (` + ph + `)
	  AND lr.linear_workspace_id = (SELECT os.linear_workspace_id FROM org_settings os WHERE os.org_id = $1)`
		},
	},
	{
		source:   "slack",
		exists:   slackChannelTrackedExists,
		forTeams: slackChannelTrackedForTeams,
		unitKey:  `e.slack_channel_id::text`,
		units: func(ph string) string {
			return `sc.slack_channel_id::text FROM team_slack_channels sc WHERE sc.org_id = $1 AND sc.team_id IN (` + ph + `)`
		},
	},
}

// trackedSQL ORs one SQL fragment per arm, each behind its source. An entity
// of a source with no arm never matches.
func trackedSQL(fragment func(trackedArm) string) string {
	arms := make([]string, len(trackedArms))
	for i, a := range trackedArms {
		arms[i] = "(e.source = '" + a.source + "' AND " + fragment(a) + ")"
	}
	return "(" + strings.Join(arms, " OR ") + ")"
}

// entityTrackedExists matches an entities row (alias e) that any of the
// viewer's teams tracks.
var entityTrackedExists = trackedSQL(func(a trackedArm) string { return a.exists })

// entityTrackedForTeams matches an entities row (alias e) that one of the
// teams in placeholders tracks.
func entityTrackedForTeams(placeholders string) string {
	return trackedSQL(func(a trackedArm) string { return a.forTeams(placeholders) })
}

// entityTracked is the tracked-set predicate an entity read takes for its
// optional team narrowing: every viewer team's when teamIDs is empty, else
// those teams', numbering the team placeholders after args. It returns the
// predicate and args with the team ids appended.
func entityTracked(teamIDs []string, args []any) (string, []any) {
	if len(teamIDs) == 0 {
		return entityTrackedExists, args
	}
	ph, ids := teamPlaceholders(len(args)+1, teamIDs)
	return entityTrackedForTeams(ph), append(args, ids...)
}

// trackedSources names the sources that have a tracked set.
func trackedSources() []string {
	out := make([]string, len(trackedArms))
	for i, a := range trackedArms {
		out[i] = a.source
	}
	return out
}

// teamPlaceholders numbers one bind placeholder per team id from start
// ("$3, $4, …") and returns them with the ids as bind arguments.
func teamPlaceholders(start int, teamIDs []string) (string, []any) {
	ph := make([]string, len(teamIDs))
	args := make([]any, len(teamIDs))
	for i, id := range teamIDs {
		ph[i] = fmt.Sprintf("$%d", start+i)
		args[i] = id
	}
	return strings.Join(ph, ", "), args
}
