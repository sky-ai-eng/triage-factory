package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TeamGitHubReposStore owns the team_github_repos table — the per-team
// GitHub repo selection (one row per (team_id, owner, repo)) and the
// source of truth for which repos a team tracks. The GitHub
// tracking-scope twin of jira_project_status_rules.
//
// # repo_profiles is derived from this table
//
// repo_profiles stays org-shared and stays the polled set, but it is now
// a *derived cache*: the org-wide UNION of every team's rows here. It is
// never user-written directly anymore — ReplaceForTeam reconciles it on
// every mutation (creating skeleton rows for newly-tracked repos,
// GC'ing rows no team tracks anymore). The poller keeps reading
// repo_profiles via RepoStore.ListConfiguredNamesSystem, unchanged.
//
// # Pool split (Postgres)
//
//   - ListForTeam, ReplaceForTeam run on the app pool. The
//     team_github_repos_select / _insert / _update / _delete RLS
//     policies gate reads by team membership and writes by team admin;
//     the request-handler caller has set the JWT claims via the
//     TxRunner. ReplaceForTeam's repo_profiles reconcile runs on the
//     admin pool (it must see every team's rows org-wide to compute the
//     union — RLS would hide sibling teams), committing autonomously
//     from the surrounding tx, the same shape EventStore.RecordSystem
//     and the other admin-pool halves use inside WithTx.
//   - ListForTeamSystem, ListForOrgSystem, TracksRepoSystem run on the
//     admin pool. The router gate + any reconcile/poll caller resolve
//     tracking without a JWT-claims context.
//
// SQLite collapses the pool split to one connection; the `...System`
// variant delegates to its non-System counterpart and the reconcile
// runs in the same transaction as the team-row write.
type TeamGitHubReposStore interface {
	// ListForTeam returns the team's tracked repos ordered by
	// (owner, repo). Empty slice with nil error when the team tracks
	// nothing. Postgres routes through the app pool
	// (team_github_repos_select gates by team membership).
	ListForTeam(ctx context.Context, teamID string) ([]domain.TeamGitHubRepo, error)

	// ListForTeamSystem mirrors ListForTeam but routes through the admin
	// pool in Postgres for callers without a JWT-claims context.
	ListForTeamSystem(ctx context.Context, teamID string) ([]domain.TeamGitHubRepo, error)

	// ListForOrgSystem returns the DISTINCT (owner, repo) union across
	// every team in the org, ordered by (owner, repo) — the polled set
	// without going through repo_profiles. Admin pool in Postgres: the
	// union spans teams the caller may not belong to.
	ListForOrgSystem(ctx context.Context, orgID string) ([]domain.TeamGitHubRepo, error)

	// ReplaceForTeam upserts one row per entry in repos and deletes rows
	// whose (owner, repo) is no longer in the input — bulk-replace
	// semantics mirroring JiraStatusRulesStore.ReplaceForTeam. Passing an
	// empty slice clears every row for the team. The whole team-row
	// replace runs in a single transaction so the table never observes a
	// partial mid-sync state.
	//
	// After mutating, it reconciles repo_profiles to the org-wide union
	// of all teams' tracked repos: newly-tracked repos get a skeleton
	// row, and a repo no team tracks anymore (the last team dropped it)
	// is GC'd. A repo still tracked by another team survives with its
	// cached profile intact. Postgres routes the team-row write through
	// the app pool (RLS gates by team admin) and the union read +
	// repo_profiles reconcile through the admin pool.
	ReplaceForTeam(ctx context.Context, teamID string, repos []domain.TeamGitHubRepo) error

	// TracksRepoSystem reports whether the team tracks (owner, repo).
	// This is the router gate lookup — owner is matched
	// case-insensitively (GitHub logins are case-insensitive) and repo
	// exactly. Admin pool in Postgres: the router goroutine has no JWT
	// claims.
	TracksRepoSystem(ctx context.Context, teamID, owner, repo string) (bool, error)
}
