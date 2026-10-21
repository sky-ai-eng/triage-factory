package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamGitHubReposStore is the Postgres impl of db.TeamGitHubReposStore.
// Holds both pools — see the TeamGitHubReposStore interface comment for
// the pool-split rationale.
//
//   - app: ListForTeam + ReplaceForTeam (the team-row write, in the caller's
//     claims tx, org-serialized by an advisory lock). RLS gates the team-row
//     write by team admin.
//   - admin: ListForTeamSystem, TracksRepoSystem, TracksRepositorySystem —
//     claims-free reads for the router gate / poll callers.
//
// A row points at a repository by the registry row's id, and the write takes
// those ids. The (owner, repo) lookups join the registry to match a name.
type teamGitHubReposStore struct {
	app   queryer
	admin queryer
}

func newTeamGitHubReposStore(app, admin queryer) db.TeamGitHubReposStore {
	return &teamGitHubReposStore{app: app, admin: admin}
}

var _ db.TeamGitHubReposStore = (*teamGitHubReposStore)(nil)

func (s *teamGitHubReposStore) ListForTeam(ctx context.Context, teamID, host string) ([]domain.TeamGitHubRepo, error) {
	return listTeamGitHubRepos(ctx, s.app, teamID, host)
}

func (s *teamGitHubReposStore) ListForTeamSystem(ctx context.Context, teamID, host string) ([]domain.TeamGitHubRepo, error) {
	return listTeamGitHubRepos(ctx, s.admin, teamID, host)
}

func listTeamGitHubRepos(ctx context.Context, q queryer, teamID, host string) ([]domain.TeamGitHubRepo, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `
		SELECT r.id, COALESCE(r.external_id, ''), r.owner, r.repo
		FROM team_github_repos g
		JOIN repositories r ON r.id = g.repository_id
		WHERE g.team_id = $1 AND r.host = $2
		ORDER BY r.owner ASC, r.repo ASC, r.id ASC
	`, teamID, host)
	if err != nil {
		return nil, fmt.Errorf("read team_github_repos: %w", err)
	}
	defer rows.Close()
	out := []domain.TeamGitHubRepo{}
	for rows.Next() {
		var r domain.TeamGitHubRepo
		if err := rows.Scan(&r.RepositoryID, &r.ExternalID, &r.Owner, &r.Repo); err != nil {
			return nil, fmt.Errorf("scan team_github_repos: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *teamGitHubReposStore) ListOrgReposWithTeamsSystem(ctx context.Context, orgID, host string) ([]domain.TrackedRepoTeams, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	// Admin pool: the preflight's org admin must see every team's tracking,
	// including teams they don't belong to (the app-pool SELECT policy is
	// team-membership-scoped). Org scope rides the teams join + the org_id
	// filter — team_github_repos carries no org_id of its own to filter on.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT r.owner, r.repo, t.name
		FROM team_github_repos g
		JOIN teams t ON t.id = g.team_id
		JOIN repositories r ON r.id = g.repository_id
		WHERE t.org_id = $1 AND r.host = $2
		ORDER BY r.owner ASC, r.repo ASC, t.name ASC
	`, orgID, host)
	if err != nil {
		return nil, fmt.Errorf("read team_github_repos with teams: %w", err)
	}
	defer rows.Close()
	out := []domain.TrackedRepoTeams{}
	for rows.Next() {
		var owner, repo, team string
		if err := rows.Scan(&owner, &repo, &team); err != nil {
			return nil, fmt.Errorf("scan team_github_repos with teams: %w", err)
		}
		if n := len(out); n > 0 && out[n-1].Owner == owner && out[n-1].Repo == repo {
			out[n-1].Teams = append(out[n-1].Teams, team)
			continue
		}
		out = append(out, domain.TrackedRepoTeams{Owner: owner, Repo: repo, Teams: []string{team}})
	}
	return out, rows.Err()
}

func (s *teamGitHubReposStore) ReplaceForTeam(ctx context.Context, orgID, teamID, host string, repositoryIDs []string) error {
	if err := db.RequireRepoHost(host); err != nil {
		return err
	}
	ids := db.DedupPreserveOrder(repositoryIDs)

	// Everything runs in the caller's app-pool tx, where RLS gates the
	// team-row write by team admin. A per-org transaction advisory lock
	// serializes concurrent same-org saves: without it two saves of one team's
	// set interleave under READ COMMITTED — neither prune sees the other's
	// uncommitted inserts — and the team ends up tracking the union of both
	// bodies, a set nobody submitted. The key is hashtextextended (64-bit) so
	// distinct orgs don't collide onto the same lock and serialize against
	// each other.
	return inTx(ctx, s.app, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, orgID); err != nil {
			return fmt.Errorf("lock org for repo tracking write: %w", err)
		}
		// The two arguments have to describe one tenant. The composite foreign
		// keys on team_github_repos are what enforce that — a mismatched pair
		// has no (team_id, org_id) or (repository_id, org_id) parent row to
		// reference — so this check is not the enforcement. It is here to fail
		// with a name instead of an integrity-violation string.
		if err := assertTeamInOrg(ctx, tx, orgID, teamID); err != nil {
			return err
		}
		// Every id has to be a repository of the org on host. The foreign key
		// would catch an id that names nothing, but not one on another host,
		// which the prune below could then never reach again from this host's
		// saves. A string that is not a uuid names nothing too, and is refused
		// here by name rather than by the uuid[] cast.
		if err := requireRepositoriesOnHost(ctx, tx, orgID, host, ids); err != nil {
			return err
		}

		// ON CONFLICT DO NOTHING preserves the original created_at for repos a
		// team already tracked.
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO team_github_repos (team_id, repository_id, org_id)
				VALUES ($1, $2, $3)
				ON CONFLICT (team_id, repository_id) DO NOTHING
			`, teamID, id, orgID); err != nil {
				return fmt.Errorf("insert team_github_repos[%s]: %w", id, err)
			}
		}
		// Prune rows no longer desired. NOT IN against an empty list keeps
		// every row, so an empty desired set takes the unconditional clear.
		//
		// Untracking stops here. The registry row survives — a worktree ledger
		// entry, a pinned project or a task may still name the repository, and
		// tracking is forward-only in both directions. The prune reaches only
		// repositories on host: the team's tracking on another host is not
		// this save's to change.
		if len(ids) == 0 {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM team_github_repos g
				 USING repositories r
				 WHERE g.team_id = $1 AND r.id = g.repository_id AND r.host = $2
			`, teamID, host); err != nil {
				return fmt.Errorf("clear team_github_repos: %w", err)
			}
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM team_github_repos g
			 USING repositories r
			 WHERE g.team_id = $1 AND r.id = g.repository_id AND r.host = $3
			   AND g.repository_id <> ALL ($2::uuid[])
		`, teamID, pgUUIDArray(ids), host); err != nil {
			return fmt.Errorf("prune team_github_repos: %w", err)
		}
		return nil
	})
}

// requireRepositoriesOnHost refuses ids that do not all name a repository row
// of orgID on host, naming every one that does not.
func requireRepositoriesOnHost(ctx context.Context, q queryer, orgID, host string, ids []string) error {
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if isValidUUID(id) {
			valid = append(valid, id)
		}
	}
	found := make(map[string]bool, len(valid))
	if len(valid) > 0 {
		rows, err := q.QueryContext(ctx, `
			SELECT id::text FROM repositories
			 WHERE org_id = $1 AND host = $2 AND id = ANY ($3::uuid[])
		`, orgID, host, pgUUIDArray(valid))
		if err != nil {
			return fmt.Errorf("resolve tracked repositories: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("scan tracked repository: %w", err)
			}
			found[id] = true
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return db.MissingRepositoriesError(ids, found, host)
}

func (s *teamGitHubReposStore) TracksRepoSystem(ctx context.Context, teamID, host, owner, repo string) (bool, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return false, err
	}
	var n int
	err := s.admin.QueryRowContext(ctx, `
		SELECT 1 FROM team_github_repos g
		JOIN repositories r ON r.id = g.repository_id
		WHERE g.team_id = $1 AND r.host = $4
		  AND lower(r.owner) = lower($2) AND lower(r.repo) = lower($3)
		LIMIT 1
	`, teamID, owner, repo, host).Scan(&n)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("tracks repo: %w", err)
	}
	return true, nil
}

func (s *teamGitHubReposStore) TracksRepositorySystem(ctx context.Context, teamID, repositoryID string) (bool, error) {
	var n int
	err := s.admin.QueryRowContext(ctx, `
		SELECT 1 FROM team_github_repos
		WHERE team_id = $1 AND repository_id = $2
	`, teamID, repositoryID).Scan(&n)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("tracks repository: %w", err)
	}
	return true, nil
}

// RepoUpdateRecipientsSystem — identical query to the SQLite impl so both
// backends pin to one result. Admin pool: the emitters (profiler) are
// claims-free background jobs, and the union deliberately crosses team
// boundaries the membership RLS policies would hide.
func (s *teamGitHubReposStore) RepoUpdateRecipientsSystem(ctx context.Context, orgID, host, owner, repo string) ([]string, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	// UNION (not UNION ALL) dedups a user who is both an org admin and a
	// tracking-team member. The tracking arm mirrors
	// repoTrackedByATeam (the REST read's scoping) join for
	// join — including its deliberate LACK of a teams.deleted_at filter:
	// archiving force-stops a team's work but hides nothing (Archive
	// touches no memberships/tracking rows, and the repo keeps being
	// polled), so its members still see the repo on GET /api/repos and
	// must keep receiving its events. Excluding them here would be
	// routing semantics (TeamIDsForUserInOrgSystem — no NEW work for an
	// archived team) misapplied to a visibility read, leaving their
	// Repos page silently stale.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT user_id::text FROM (
			SELECT om.user_id
			FROM org_memberships om
			WHERE om.org_id = $1 AND om.role IN ('owner', 'admin')
			UNION
			SELECT m.user_id
			FROM team_github_repos g
			JOIN teams t ON t.id = g.team_id
			JOIN repositories r ON r.id = g.repository_id
			JOIN memberships m ON m.team_id = g.team_id
			WHERE t.org_id = $1
			  AND r.host = $4
			  AND lower(r.owner) = lower($2) AND lower(r.repo) = lower($3)
		) u
		ORDER BY user_id ASC
	`, orgID, owner, repo, host)
	if err != nil {
		return nil, fmt.Errorf("repo update recipients: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan recipient: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *teamGitHubReposStore) TracksRepoViewerScoped(ctx context.Context, orgID, host, owner, repo string) (bool, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return false, err
	}
	// Runs on the app pool so team_github_repos_select RLS auto-scopes the
	// EXISTS to the caller's own team memberships — no team_id needed, same
	// RLS-does-the-scoping trick as factoryGitHubRepoTrackedExists /
	// jiraTeamProjectMembershipExists. The teams join binds org as
	// defense-in-depth.
	var ok bool
	err := s.app.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM team_github_repos g
			JOIN teams tm ON tm.id = g.team_id
			JOIN repositories r ON r.id = g.repository_id
			WHERE tm.org_id = $1
			  AND r.host = $4
			  AND lower(r.owner) = lower($2)
			  AND lower(r.repo) = lower($3)
		)
	`, orgID, owner, repo, host).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("tracks repo viewer scoped: %w", err)
	}
	return ok, nil
}

func (s *teamGitHubReposStore) TracksRepoViewerAdminScoped(ctx context.Context, orgID, host, owner, repo string) (bool, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return false, err
	}
	// TracksRepoViewerScoped's query plus tf.user_is_team_admin on the
	// matched row: RLS narrows the EXISTS to the caller's memberships, the
	// predicate narrows it again to the ones they administer. Both are
	// needed — the predicate alone would match a team the caller admins in
	// another org, and RLS alone is plain membership.
	var ok bool
	err := s.app.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM team_github_repos g
			JOIN teams tm ON tm.id = g.team_id
			JOIN repositories r ON r.id = g.repository_id
			WHERE tm.org_id = $1
			  AND r.host = $4
			  AND lower(r.owner) = lower($2)
			  AND lower(r.repo) = lower($3)
			  AND tf.user_is_team_admin(g.team_id)
		)
	`, orgID, owner, repo, host).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("tracks repo viewer admin scoped: %w", err)
	}
	return ok, nil
}

// assertTeamInOrg refuses a write whose orgID and teamID name different
// tenants. Under RLS the teams row is invisible outside the caller's org, so
// the same query answers for both pools: the admin pool is filtered by the
// org_id predicate, the app pool by the policy on top of it.
func assertTeamInOrg(ctx context.Context, q queryer, orgID, teamID string) error {
	var ok bool
	if err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM teams WHERE id = $1 AND org_id = $2)
	`, teamID, orgID).Scan(&ok); err != nil {
		return fmt.Errorf("verify team %s belongs to org %s: %w", teamID, orgID, err)
	}
	if !ok {
		return fmt.Errorf("%w: team %s, org %s", db.ErrTeamNotInOrg, teamID, orgID)
	}
	return nil
}
