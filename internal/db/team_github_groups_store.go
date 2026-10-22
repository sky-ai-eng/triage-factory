package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TeamGitHubGroupsStore owns the team_github_groups table — the GitHub
// twin of jira_project_status_rules. Each row maps a GitHub team to a TF
// team, so a human review request against that GitHub team can be routed to
// the team's board. No membership resolution, no nested-team traversal.
// Separate from TeamsStore because the table is multi-row per team with
// replace-set save semantics.
//
// A row is identified by GitHub's id for the team (github_team_id), and
// records GitHub's id for its organization (github_org_id) beside it; the
// org login and team slug are display values the deletion reconcile keeps
// current. See domain.TeamGitHubGroup.
//
// # Pool split (Postgres)
//
//   - ListForTeam, SetForTeam run on the app pool. The
//     team_github_groups_select / _insert / _delete RLS policies gate
//     reads by team membership and writes by team admin; the request-
//     handler caller has set the JWT claims via the TxRunner.
//   - TeamsForGitHubTeamSystem, ReconcileOrgTeamsSystem run on the admin
//     pool. The router/poller resolve routing + reconcile GitHub's team
//     listing without a JWT-claims context.
//
// SQLite collapses the pool split to one connection.
//
// # Scoped by GitHub host
//
// GitHub's ids are unique only on the host that issued them, so a row
// records the host its GitHub team lives on and every method takes the host
// it asks about — the org's current one. After the org moves to another
// host its mappings read empty there and route nothing, the deletion
// reconcile never compares them with the new host's teams, and they apply
// again if the org moves back.
//
// # Rows without ids (local mode)
//
// Postgres requires both ids on every row. A SQLite row stored before ids
// were recorded has neither, and holds only its org login and team slug.
// ReconcileOrgTeamsSystem identifies such a row from GitHub's listing, and
// until it does, TeamsForGitHubTeamSystem matches the row by its login and
// slug. Nothing writes a new row without its ids.
type TeamGitHubGroupsStore interface {
	// ListForTeam returns the team's GitHub-team mappings on host, ordered
	// by org login and team slug (case-insensitively), then team id. Empty
	// slice with nil error when the team has no mappings. Postgres routes
	// through the app pool (team_github_groups_select gates by team
	// membership).
	ListForTeam(ctx context.Context, teamID, host string) ([]domain.TeamGitHubGroup, error)

	// SetForTeam replaces the team's entire mapping set on host with groups;
	// the team's mappings on any other host are left as they stand. Each
	// group must carry GitHub's team and organization ids
	// (domain.NormalizeTeamGitHubGroups, which also drops a repeated team);
	// the login and slug are stored as given, for display. The replace runs
	// as a delete-all + insert inside a single transaction. Passing an empty
	// slice clears every mapping for the team on host. Postgres routes
	// through the app pool (team_github_groups_insert / _delete gate writes
	// by team admin).
	//
	// Exempt from the returned-row rule: it reconciles a set, so there is no
	// single row a return value could name.
	SetForTeam(ctx context.Context, teamID, host string, groups []domain.TeamGitHubGroup) error

	// TeamsForGitHubTeamSystem returns the TF team IDs in the org mapped to
	// the GitHub team an event names, ordered by team_id: the routing
	// lookup, where a github-team review request resolves to every TF team
	// that funneled it in (M:N).
	//
	// githubTeamID is GitHub's id for the team when the event carries one (a
	// review request read after ids were recorded); a mapping matches on it
	// and on nothing else, so a renamed team or organization still routes.
	// githubTeamID is "" for a team named only by its handle — a mention,
	// which body text spells as @org/slug, or an event recorded before ids
	// were — and then a mapping matches when its org login and team slug
	// equal orgLogin and teamSlug, compared case-insensitively. A local row
	// with no ids (see the type doc) is matched by its handle in both cases.
	//
	// Admin pool in Postgres: the router/poller resolve routing without a
	// JWT-claims context, and the answer must span every team rather than
	// the teams a caller belongs to.
	TeamsForGitHubTeamSystem(ctx context.Context, orgID, host, githubTeamID, orgLogin, teamSlug string) ([]string, error)

	// ReconcileOrgTeamsSystem brings the org's mappings on host for one
	// GitHub organization in line with that organization's team listing,
	// across every team in the org, in one transaction:
	//
	//   - a local row with no ids whose org login and team slug match the
	//     listing (case-insensitively) takes the team's and the
	//     organization's ids — the one place a mapping is identified by its
	//     handle;
	//   - every row with the organization's id takes its current login, and
	//     the current slug of the team its id names, so a renamed team or
	//     organization keeps its mappings and shows its new name;
	//   - a row with the organization's id whose team id is absent from the
	//     listing is deleted: the team no longer exists. A row with no ids is
	//     never deleted.
	//
	// The listing is authoritative for the organization because the only
	// caller reads it with the org-level credential (App installation token
	// or org PAT) — a single deterministic identity, not a per-user token —
	// and the editor only offers teams that identity can see, so a present
	// team cannot be wrongly pruned. The caller must skip an empty listing
	// (a zero-length read is ambiguous) and never call this on a read
	// error; this method itself takes an empty Teams as "the organization
	// has no teams" and deletes every identified mapping for it. The
	// team:deleted webhook is the optional real-time layer on top.
	//
	// Returns the number of rows deleted. Admin pool in Postgres: the
	// reconcile spans teams the triggering caller may not belong to, so it
	// runs claims-free.
	//
	// Exempt from the returned-row rule: a bulk reconcile, like SetForTeam.
	ReconcileOrgTeamsSystem(ctx context.Context, orgID, host string, listing domain.GitHubOrgTeams) (int, error)
}
