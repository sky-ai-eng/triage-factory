package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrTeamNotInOrg reports a write whose orgID and teamID name different
// tenants. It is returned rather than tolerated because the row such a write
// produces is invisible: the tracking row would live under one org's team and
// point at another org's repository, so every org-scoped read filters it out
// and the caller sees a save that succeeded and tracked nothing.
var ErrTeamNotInOrg = errors.New("db: team does not belong to that org")

// MissingRepositoriesError is ReplaceForTeam's refusal of ids that name no
// repository on host: nil when found holds every id, else an error wrapping
// ErrNoSuchRepository that names each missing id in input order. Shared by
// both dialects so the refusal reads the same from either.
func MissingRepositoriesError(ids []string, found map[string]bool, host string) error {
	var missing []string
	for _, id := range ids {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w on %s: %s", ErrNoSuchRepository, host, strings.Join(missing, ", "))
}

// TeamGitHubReposStore owns the team_github_repos table — the per-team
// GitHub repo selection (one row per (team_id, repository_id)) and the
// source of truth for which repos a team tracks. The GitHub
// tracking-scope twin of jira_project_status_rules.
//
// # This table IS the tracked set; repositories is the registry
//
// A row here references a repository by the registry row's id, so a rename
// moves one column on one row in `repositories` and every tracking row still
// resolves without being rewritten. The write takes registry ids and nothing
// else, and the team's read hands each one back beside the display name. A
// caller that holds only a name — an agent's push, a `workspace add` —
// resolves it to a registry row first (RepositoryStore.GetByRefSystem) and
// asks about that row's id.
//
// The two tables have different lifetimes, and the direction matters. A
// registry row exists before anything tracks it — it is registered by GitHub
// id (RepositoryStore.FindOrCreateByExternalID) or minted when TF records an
// entity in the repository — and untracking deletes only the row here. A
// registry row outlives every tracking decision, because a worktree ledger
// entry, a pinned project or a task may still name the repository. So "which
// repositories does TF poll and profile" is a question about tracking, and
// RepositoryStore.ListTrackedNamesSystem — which reads through this table — is
// what answers it.
//
// # Scoped by GitHub host
//
// A tracking row references a repository row, and a repository row lives on
// one GitHub host (RepositoryStore's host scope), so every method here takes
// the host it asks about — the org's current one — and sees only tracking rows
// whose repository is on it. After the org moves to another host its tracked
// set reads empty there, because a same-named repository on the new host is a
// different repository; the old host's rows stay stored and apply again if the
// org moves back. Like untracking, nothing about a move prunes or deletes.
//
// # Pool split (Postgres)
//
//   - ListForTeam, ReplaceForTeam run on the app pool. The
//     team_github_repos_select / _insert / _update / _delete RLS
//     policies gate reads by team membership and writes by team admin;
//     the request-handler caller has set the JWT claims via the TxRunner.
//     ReplaceForTeam runs in the caller's tx, serialized per org by a
//     transaction advisory lock so two concurrent saves of one team's set
//     cannot interleave into a set neither of them asked for.
//   - ListForTeamSystem, TracksRepositorySystem run on the admin pool. The router gate + any poll caller resolve tracking
//     without a JWT-claims context.
//
// SQLite collapses the pool split to one connection; the `...System`
// variant delegates to its non-System counterpart, and its single writer
// makes the lock moot.
type TeamGitHubReposStore interface {
	// ListForTeam returns the team's tracked repos on host ordered by
	// (owner, repo), each with its registry id, its GitHub id when the
	// registry row carries one, and its current name. Empty slice with nil
	// error when the team tracks nothing. Postgres routes through the app
	// pool (team_github_repos_select gates by team membership).
	ListForTeam(ctx context.Context, teamID, host string) ([]domain.TeamGitHubRepo, error)

	// ListForTeamSystem mirrors ListForTeam but routes through the admin
	// pool in Postgres for callers without a JWT-claims context.
	ListForTeamSystem(ctx context.Context, teamID, host string) ([]domain.TeamGitHubRepo, error)

	// ListOrgReposWithTeamsSystem returns each tracked, live registry row on
	// host in the org — its id, GitHub id and current name — together with the
	// display names of the teams tracking it, one entry per row, ordered by
	// (owner, repo, row id) then team name. It is the tracked-set source for
	// the GitHub-access switch reachability preflights: a repo that goes dark
	// under the new credential is reported with the teams that own it. An
	// archived team tracks nothing here, so a repo only it tracks is left out
	// and its name is in no entry's team list.
	// Admin pool in Postgres — the org admin running a preflight must see every
	// team's tracking, including teams they don't belong to, so this is a
	// claims-free system read scoped by org_id in the query.
	ListOrgReposWithTeamsSystem(ctx context.Context, orgID, host string) ([]domain.TrackedRepoTeams, error)

	// ReplaceForTeam makes the team's tracked set on host exactly the
	// registry rows repositoryIDs names: it inserts a tracking row for each
	// id the team does not track yet and deletes the team's rows on host for
	// every repository not in the list — bulk-replace semantics mirroring
	// JiraStatusRulesStore.ReplaceForTeam. Passing an empty slice clears every
	// row for the team on host. The prune reaches only rows whose repository
	// is on host: the team's tracking on another host is left exactly as it
	// stands. A repeated id is one row.
	//
	// Every id must name a registry row of orgID on host; one that does not
	// (unknown, another org's, another host's) fails the whole write with an
	// error wrapping ErrNoSuchRepository that names it, and nothing moves. It
	// creates no registry row: a repository is registered first, by GitHub id
	// through RepositoryStore.FindOrCreateByExternalID, and tracked by the id
	// that returns. Composed in the caller's WithTx so RLS gates the team-row
	// write by team admin. orgID must be the team's org (the request's
	// authorized org), else ErrTeamNotInOrg.
	//
	// Control-plane only: tracking is a team admin's write. The executor's
	// Postgres role holds nothing on team_github_repos beyond SELECT, so it
	// never adds to a tracked set.
	//
	// Untracking is a delete HERE and nowhere else: the registry row for a
	// repository no team tracks anymore survives, with its profile, base
	// branch, clone state and poll cursor intact, because a worktree ledger
	// entry, a pinned project or a task may still name it. It simply stops
	// being in the tracked set, so it stops being polled and profiled.
	//
	// Exempt from the returned-row rule: it reconciles a team's whole tracked
	// set in one transaction, so there is no single row a return value could
	// name.
	ReplaceForTeam(ctx context.Context, orgID, teamID, host string, repositoryIDs []string) error

	// TracksRepositorySystem reports whether the team tracks the repository
	// whose registry row is repositoryID. Every tracking question is asked by
	// id: a GitHub entity holds one (entities.repository_id), a ledger row
	// holds one, and a name a run hands TF — a push through the git proxy, a
	// `workspace add` — is resolved to its row once, where it arrives. No host
	// is asked about: a registry row lives on one host, so the id already
	// names it. A retired row's tracking row is kept, so this answers true for
	// it; a caller that resolved a name never holds one, because no name
	// resolves to a retired row. Admin pool in Postgres: these callers have no
	// JWT claims.
	TracksRepositorySystem(ctx context.Context, teamID, repositoryID string) (bool, error)

	// RepoUpdateRecipientsSystem returns the distinct, sorted ids of every
	// user who may receive a repository_updated websocket event for the
	// repository whose registry row is repositoryID in orgID: the org's admins
	// and owners, plus every member of a team that tracks the row. This
	// deliberately mirrors the REST read's visibility (org admins get the
	// org-wide union, members their teams' tracked sets — the
	// repoTrackedByATeam semi-join behind ListTeamScoped), because the
	// websocket hub scopes connections on (org, user) only and the
	// repoevent.Notifier fans one event per returned id onto that axis.
	// Keyed by the row id the event carries, so a rename between the write
	// and this read changes nothing, and a retired row's tracking teams are
	// left out the way the REST read leaves the row out. An archived team's
	// members are left out the same way: an archived team tracks nothing in
	// repoTrackedByATeam, so the repo is not on their GET /api/repos through
	// it, and a restore brings them back. Admin pool in Postgres: the
	// emitters are claims-free background jobs. Called fresh per emission so
	// membership and archive changes take effect immediately; local mode
	// never calls it (N=1 broadcasts org-wide).
	RepoUpdateRecipientsSystem(ctx context.Context, orgID, repositoryID string) ([]string, error)

	// TracksRepositoryViewerScoped reports whether ANY team the calling user
	// belongs to tracks the repository with this registry id (TFAC-559). The
	// app-pool, viewer-scoped sibling of TracksRepositorySystem (which checks
	// one named team via the admin pool): under RLS, team_github_repos_select
	// auto-scopes the EXISTS to the caller's own memberships, so this is the
	// per-repo gate for "does any of my teams track this repo" — the read gate
	// of the repo routes, so a non-admin member can't read or touch a repo
	// outside their team's tracked set. Keyed by id rather than name because
	// a name does not pick one row: a retired row keeps the name the
	// repository now under it holds. An archived team tracks nothing here, as
	// in ListTeamScoped, so a repo outside this gate is outside the caller's
	// list too. Org admins bypass this check entirely at the handler layer
	// instead of calling it. Local mode (N=1) always reports true — no team
	// boundary to enforce.
	TracksRepositoryViewerScoped(ctx context.Context, orgID, repositoryID string) (bool, error)

	// TracksRepositoryViewerAdminScoped narrows TracksRepositoryViewerScoped to
	// teams the calling user *administers*: it reports whether ANY team the
	// caller is a team admin of tracks the repository with this registry id.
	// Same app pool, same RLS-does-the-scoping trick, plus an explicit
	// team-admin predicate on the matched row. Administering an archived team
	// grants nothing, since that team tracks nothing in the read gate.
	//
	// This is the mutation gate for org-wide repo configuration
	// (repositories carries no team_id, so a write by a member of one
	// tracking team lands on every tracking team's runs). Membership alone
	// is the *read* gate — TracksRepositoryViewerScoped — and the two are
	// deliberately separate: a caller who sees a repo in their list but
	// administers none of the teams tracking it gets 403, not 404. Org
	// admins bypass this at the handler layer instead of calling it. Local
	// mode (N=1) always reports true — no team boundary to enforce.
	TracksRepositoryViewerAdminScoped(ctx context.Context, orgID, repositoryID string) (bool, error)
}
