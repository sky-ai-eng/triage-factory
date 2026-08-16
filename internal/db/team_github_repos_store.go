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
// # repositories is derived from this table
//
// repositories stays org-shared and stays the polled set, but it is now
// a *derived cache*: the org-wide UNION of every team's rows here. It is
// never user-written directly anymore — ReplaceForTeam mutates the team's
// rows AND reconciles repositories to the new union atomically, in one
// transaction. The poller keeps reading repositories via
// RepositoryStore.ListConfiguredNamesSystem, unchanged.
//
// # Pool split (Postgres)
//
//   - ListForTeam, ReplaceForTeam run on the app pool. The
//     team_github_repos_select / _insert / _update / _delete RLS
//     policies gate reads by team membership and writes by team admin;
//     the request-handler caller has set the JWT claims via the TxRunner.
//     ReplaceForTeam's union read crosses team boundaries (which the
//     team-membership SELECT policy hides), so it reads through the
//     tf.org_tracked_repos() SECURITY DEFINER helper — scoped to the
//     caller's org via tf.current_org_id(), never to a trusted argument,
//     so the org boundary is preserved at the DB layer while only the
//     non-boundary team scope is bypassed. The whole team-write +
//     reconcile runs in the caller's tx (atomic), serialized per org by a
//     transaction advisory lock so concurrent same-org saves can't race
//     repositories into an inconsistent state.
//   - ListForTeamSystem, ListForOrgSystem, TracksRepoSystem run on the
//     admin pool. The router gate + any poll caller resolve tracking
//     without a JWT-claims context.
//
// SQLite collapses the pool split to one connection; the `...System`
// variant delegates to its non-System counterpart, the union read needs
// no SECURITY DEFINER (no RLS), and the reconcile runs in the same
// (single-writer, lock-free) transaction as the team-row write.
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
	// without going through repositories. Admin pool in Postgres: the
	// union spans teams the caller may not belong to.
	ListForOrgSystem(ctx context.Context, orgID string) ([]domain.TeamGitHubRepo, error)

	// ListOrgReposWithTeamsSystem returns each tracked (owner, repo) in the
	// org together with the display names of the teams tracking it, ordered by
	// (owner, repo) then team name. It is the tracked-set source for the
	// GitHub-access switch reachability preflights (TFAC-328): a repo that goes
	// dark under the new credential is reported with the teams that own it.
	// Admin pool in Postgres — the org admin running a preflight must see every
	// team's tracking, including teams they don't belong to, so this is a
	// claims-free system read scoped by org_id in the query.
	ListOrgReposWithTeamsSystem(ctx context.Context, orgID string) ([]domain.TrackedRepoTeams, error)

	// ReplaceForTeam upserts one row per entry in repos and deletes rows
	// whose (owner, repo) is no longer in the input — bulk-replace
	// semantics mirroring JiraStatusRulesStore.ReplaceForTeam. Passing an
	// empty slice clears every row for the team.
	//
	// In the same transaction it reconciles the org-shared repositories
	// cache to the new org-wide union: newly-tracked repos get a skeleton
	// row, a repo no team tracks anymore is GC'd, and a repo still tracked
	// by another team survives with its cached profile intact. Atomic — if
	// the tx rolls back, neither team_github_repos nor repositories moves.
	// Postgres serializes concurrent same-org calls with a per-org
	// transaction advisory lock so the union recompute can't race; the
	// cross-team union read goes through the tf.org_tracked_repos(org)
	// SECURITY DEFINER helper, which enforces org == the caller's claims
	// when present (so the org boundary holds at the DB layer). Composed
	// in the caller's WithTx so RLS gates the team-row write by team admin.
	// orgID must be the team's org (the request's authorized org).
	ReplaceForTeam(ctx context.Context, orgID, teamID string, repos []domain.TeamGitHubRepo) error

	// TracksRepoSystem reports whether the team tracks (owner, repo),
	// matched case-insensitively on both fields (GitHub identifiers are
	// case-insensitive). This is the router gate lookup. Admin pool in
	// Postgres: the router goroutine has no JWT claims.
	TracksRepoSystem(ctx context.Context, teamID, owner, repo string) (bool, error)

	// RepoUpdateRecipientsSystem returns the distinct, sorted ids of every
	// user who may receive a repository_updated websocket event for
	// (owner, repo) in orgID: the org's admins and owners, plus every
	// member of a non-archived team that tracks the repo, matched
	// case-insensitively on both fields. This deliberately mirrors the
	// REST read's visibility (org admins get the org-wide union, members
	// their teams' tracked sets — see handleRepositories), because the
	// websocket hub scopes connections on (org, user) only and the
	// repoevent.Notifier fans one event per returned id onto that axis.
	// Admin pool in Postgres: the emitters are claims-free background
	// jobs. Called fresh per emission so membership changes take effect
	// immediately; local mode never calls it (N=1 broadcasts org-wide).
	RepoUpdateRecipientsSystem(ctx context.Context, orgID, owner, repo string) ([]string, error)

	// TracksRepoViewerScoped reports whether ANY team the calling user
	// belongs to tracks (owner, repo), matched case-insensitively
	// (TFAC-559). The app-pool, viewer-scoped sibling of TracksRepoSystem
	// (which checks one named team via the admin pool): under RLS,
	// team_github_repos_select auto-scopes the EXISTS to the caller's own
	// memberships, so this is the per-repo gate for "does any of my teams
	// track this repo" — used by the repo-settings base-branch PATCH and
	// branches GET so a non-admin member can't touch a repo outside their
	// team's tracked set. Org admins bypass this check entirely at the
	// handler layer instead of calling it. Local mode (N=1) always
	// reports true — no team boundary to enforce.
	TracksRepoViewerScoped(ctx context.Context, orgID, owner, repo string) (bool, error)

	// TracksRepoViewerAdminScoped narrows TracksRepoViewerScoped to teams
	// the calling user *administers*: it reports whether ANY team the
	// caller is a team admin of tracks (owner, repo), matched
	// case-insensitively. Same app pool, same RLS-does-the-scoping trick,
	// plus an explicit team-admin predicate on the matched row.
	//
	// This is the mutation gate for org-wide repo configuration
	// (repositories carries no team_id, so a write by a member of one
	// tracking team lands on every tracking team's runs). Membership alone
	// is the *read* gate — TracksRepoViewerScoped — and the two are
	// deliberately separate: a caller who sees a repo in their list but
	// administers none of the teams tracking it gets 403, not 404. Org
	// admins bypass this at the handler layer instead of calling it. Local
	// mode (N=1) always reports true — no team boundary to enforce.
	TracksRepoViewerAdminScoped(ctx context.Context, orgID, owner, repo string) (bool, error)
}
