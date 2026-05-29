package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TeamGitHubGroupsStore owns the team_github_groups table — the GitHub
// twin of jira_project_status_rules. Each row maps a fully-qualified
// GitHub team (org login + team slug) to a TF team, so a human review
// request against that GitHub team can be routed to the team's board.
// Dumb string labels for routing only — no membership resolution, no
// nested-team traversal. Separate from TeamsStore because the table is
// multi-row per team with replace-set save semantics.
//
// # Pool split (Postgres)
//
//   - ListForTeam, SetForTeam, TeamsForGroup run on the app pool. The
//     team_github_groups_select / _insert / _delete RLS policies gate
//     reads by team membership and writes by team admin; the request-
//     handler caller has set the JWT claims via the TxRunner.
//   - ListForTeamSystem, TeamsForGroupSystem, PruneMissingSystem run on
//     the admin pool. The router/poller resolve routing + reconcile
//     deleted GitHub teams without a JWT-claims context.
//
// SQLite collapses the pool split to one connection; the `...System`
// variant delegates to its non-System counterpart.
type TeamGitHubGroupsStore interface {
	// ListForTeam returns the team's GitHub-team mappings ordered by
	// (github_org_login, github_team_slug). Empty slice with nil error
	// when the team has no mappings. Postgres routes through the app
	// pool (team_github_groups_select gates by team membership).
	ListForTeam(ctx context.Context, teamID string) ([]domain.TeamGitHubGroup, error)

	// ListForTeamSystem mirrors ListForTeam but routes through the admin
	// pool in Postgres for callers without a JWT-claims context.
	ListForTeamSystem(ctx context.Context, teamID string) ([]domain.TeamGitHubGroup, error)

	// SetForTeam replaces the team's entire mapping set with groups.
	// Because every column is part of the primary key the rows are pure
	// identity tuples, so the replace runs as a delete-all + insert
	// inside a single transaction — there is nothing to update in place.
	// Passing an empty slice clears every mapping for the team. Org
	// logins + team slugs are lowercase-normalized so routing lookups
	// match regardless of how the admin typed them. Postgres routes
	// through the app pool (team_github_groups_insert / _delete gate
	// writes by team admin).
	SetForTeam(ctx context.Context, teamID string, groups []domain.TeamGitHubGroup) error

	// TeamsForGroup returns the TF team IDs mapped to the given GitHub
	// team within the org, ordered by team_id. This is the routing
	// lookup: a github-team review request resolves to every TF team
	// that funneled it in (M:N). orgLogin + teamSlug are matched
	// case-insensitively. Postgres routes through the app pool, so the
	// caller only sees teams it is a member of — routing consumers
	// should use TeamsForGroupSystem.
	TeamsForGroup(ctx context.Context, orgID, orgLogin, teamSlug string) ([]string, error)

	// TeamsForGroupSystem mirrors TeamsForGroup but routes through the
	// admin pool in Postgres so the router/poller can resolve routing
	// without a JWT-claims context (and without RLS filtering the
	// result to the caller's own teams).
	TeamsForGroupSystem(ctx context.Context, orgID, orgLogin, teamSlug string) ([]string, error)

	// PruneMissingSystem removes, across every team in the org, the
	// mapping rows for github_org_login whose github_team_slug is not in
	// presentSlugs — the GitHub-team-deletion reconcile floor. Callers
	// pass the live set of slugs from GET /orgs/{org}/teams; a deleted
	// GitHub team is absent from that set and its mapping rows are dropped
	// (the TF team itself is never touched).
	//
	// presentSlugs is taken as the authoritative team set for the org
	// login. That holds because the only caller fetches it via the
	// org-level credential (App installation token or org PAT) — a single
	// deterministic identity, not a per-user token — and the editor only
	// ever creates mappings for teams that identity can see, so a present
	// team can't be wrongly pruned. The caller must skip the empty case
	// (a zero-length fetch is ambiguous) and never call this on a fetch
	// error; this method itself treats an empty presentSlugs as "the org
	// has no teams" and drops every mapping for the login. The team:deleted
	// webhook is the optional real-time layer on top of this floor.
	//
	// Returns the number of rows removed. Admin pool in Postgres: the
	// prune spans teams the triggering caller may not belong to, so it
	// runs claims-free.
	PruneMissingSystem(ctx context.Context, orgID, orgLogin string, presentSlugs []string) (int, error)
}
