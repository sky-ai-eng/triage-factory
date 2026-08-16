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
//   - app: ListForTeam + the whole of ReplaceForTeam (registry get-or-create +
//     team-row write, atomic in the caller's claims tx, org-serialized by an
//     advisory lock). RLS gates the team-row write by team admin.
//   - admin: ListForTeamSystem, ListForOrgSystem, TracksRepoSystem —
//     claims-free reads for the router gate / poll callers.
//
// A row points at a repository by the registry row's id. The store's own
// surface is still (owner, repo) — that is what a request body carries and
// what the router gate is asked about — so every read joins the registry to
// project the slug and every write resolves the slug to a row first.
type teamGitHubReposStore struct {
	app   queryer
	admin queryer
}

func newTeamGitHubReposStore(app, admin queryer) db.TeamGitHubReposStore {
	return &teamGitHubReposStore{app: app, admin: admin}
}

var _ db.TeamGitHubReposStore = (*teamGitHubReposStore)(nil)

func (s *teamGitHubReposStore) ListForTeam(ctx context.Context, teamID string) ([]domain.TeamGitHubRepo, error) {
	return listTeamGitHubRepos(ctx, s.app, teamID)
}

func (s *teamGitHubReposStore) ListForTeamSystem(ctx context.Context, teamID string) ([]domain.TeamGitHubRepo, error) {
	return listTeamGitHubRepos(ctx, s.admin, teamID)
}

func listTeamGitHubRepos(ctx context.Context, q queryer, teamID string) ([]domain.TeamGitHubRepo, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT r.owner, r.repo
		FROM team_github_repos g
		JOIN repositories r ON r.id = g.repository_id
		WHERE g.team_id = $1
		ORDER BY r.owner ASC, r.repo ASC
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("read team_github_repos: %w", err)
	}
	defer rows.Close()
	out := []domain.TeamGitHubRepo{}
	for rows.Next() {
		var r domain.TeamGitHubRepo
		if err := rows.Scan(&r.Owner, &r.Repo); err != nil {
			return nil, fmt.Errorf("scan team_github_repos: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *teamGitHubReposStore) ListForOrgSystem(ctx context.Context, orgID string) ([]domain.TeamGitHubRepo, error) {
	return listTeamGitHubReposForOrg(ctx, s.admin, orgID)
}

func (s *teamGitHubReposStore) ListOrgReposWithTeamsSystem(ctx context.Context, orgID string) ([]domain.TrackedRepoTeams, error) {
	// Admin pool: the preflight's org admin must see every team's tracking,
	// including teams they don't belong to (the app-pool SELECT policy is
	// team-membership-scoped). Org scope rides the teams join + the org_id
	// filter, mirroring listTeamGitHubReposForOrg.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT r.owner, r.repo, t.name
		FROM team_github_repos g
		JOIN teams t ON t.id = g.team_id
		JOIN repositories r ON r.id = g.repository_id
		WHERE t.org_id = $1
		ORDER BY r.owner ASC, r.repo ASC, t.name ASC
	`, orgID)
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

func scanRepoRows(rows *sql.Rows, err error) ([]domain.TeamGitHubRepo, error) {
	if err != nil {
		return nil, fmt.Errorf("read team_github_repos union: %w", err)
	}
	defer rows.Close()
	out := []domain.TeamGitHubRepo{}
	for rows.Next() {
		var r domain.TeamGitHubRepo
		if err := rows.Scan(&r.Owner, &r.Repo); err != nil {
			return nil, fmt.Errorf("scan team_github_repos union: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *teamGitHubReposStore) ReplaceForTeam(ctx context.Context, orgID, teamID string, repos []domain.TeamGitHubRepo) error {
	norm, err := domain.NormalizeTeamGitHubRepos(repos)
	if err != nil {
		return err
	}

	// Everything runs in the caller's app-pool tx (RLS gates the team-row
	// write by team admin) so the registry creates and the team-row mutation
	// are atomic. A per-org transaction advisory lock serializes concurrent
	// same-org saves: two teams adding the same new repository would otherwise
	// race in the registry, and while the folded identity index makes that
	// resolve to one row, the lock keeps the ordering legible rather than
	// leaving one writer to discover the conflict. The key is hashtextextended
	// (64-bit) so distinct orgs don't collide onto the same lock and serialize
	// against each other.
	return inTx(ctx, s.app, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, orgID); err != nil {
			return fmt.Errorf("lock org for repo tracking write: %w", err)
		}

		// The registry row comes first: a tracking row references it, so it has
		// to exist before the insert that points at it. This is also the whole
		// of what used to be a reconcile — the org-wide union recompute existed
		// to keep a derived cache in step, and the foreign key has taken that
		// job over. With it goes the cross-team union read (and the
		// tf.org_tracked_repos SECURITY DEFINER helper it needed to see past
		// the per-team SELECT policy): this transaction now touches only the
		// caller's own team rows plus the org's registry, both of which the app
		// pool can see for itself.
		ids := make([]string, 0, len(norm))
		for _, r := range norm {
			id, err := getOrCreateRepositoryID(ctx, tx, orgID, domain.RepoRef{Owner: r.Owner, Repo: r.Repo})
			if err != nil {
				return fmt.Errorf("resolve repository %s/%s: %w", r.Owner, r.Repo, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO team_github_repos (team_id, repository_id)
				VALUES ($1, $2)
				ON CONFLICT (team_id, repository_id) DO NOTHING
			`, teamID, id); err != nil {
				return fmt.Errorf("insert team_github_repos[%s/%s]: %w", r.Owner, r.Repo, err)
			}
			ids = append(ids, id)
		}
		// Prune rows no longer desired. NOT IN against an empty list keeps
		// every row, so an empty desired set takes the unconditional clear.
		//
		// Untracking stops here. The registry row survives — a worktree ledger
		// entry, a pinned project or a task may still name the repository, and
		// tracking is forward-only in both directions.
		if len(ids) == 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM team_github_repos WHERE team_id = $1`, teamID,
			); err != nil {
				return fmt.Errorf("clear team_github_repos: %w", err)
			}
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM team_github_repos
			WHERE team_id = $1
			  AND repository_id <> ALL ($2::uuid[])
		`, teamID, pgUUIDArray(ids)); err != nil {
			return fmt.Errorf("prune team_github_repos: %w", err)
		}
		return nil
	})
}

// listTeamGitHubReposForOrg reads the DISTINCT (owner, repo) union across
// the org's teams (committed state on whatever queryer is passed). org
// scope rides the teams join — team_github_repos carries no org_id.
func listTeamGitHubReposForOrg(ctx context.Context, q queryer, orgID string) ([]domain.TeamGitHubRepo, error) {
	return scanRepoRows(q.QueryContext(ctx, `
		SELECT DISTINCT r.owner, r.repo
		FROM team_github_repos g
		JOIN teams t ON t.id = g.team_id
		JOIN repositories r ON r.id = g.repository_id
		WHERE t.org_id = $1
		ORDER BY r.owner ASC, r.repo ASC
	`, orgID))
}

func (s *teamGitHubReposStore) TracksRepoSystem(ctx context.Context, teamID, owner, repo string) (bool, error) {
	var n int
	err := s.admin.QueryRowContext(ctx, `
		SELECT 1 FROM team_github_repos g
		JOIN repositories r ON r.id = g.repository_id
		WHERE g.team_id = $1 AND lower(r.owner) = lower($2) AND lower(r.repo) = lower($3)
		LIMIT 1
	`, teamID, owner, repo).Scan(&n)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("tracks repo: %w", err)
	}
	return true, nil
}

func (s *teamGitHubReposStore) TracksRepoViewerScoped(ctx context.Context, orgID, owner, repo string) (bool, error) {
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
			  AND lower(r.owner) = lower($2)
			  AND lower(r.repo) = lower($3)
		)
	`, orgID, owner, repo).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("tracks repo viewer scoped: %w", err)
	}
	return ok, nil
}

func (s *teamGitHubReposStore) TracksRepoViewerAdminScoped(ctx context.Context, orgID, owner, repo string) (bool, error) {
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
			  AND lower(r.owner) = lower($2)
			  AND lower(r.repo) = lower($3)
			  AND tf.user_is_team_admin(g.team_id)
		)
	`, orgID, owner, repo).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("tracks repo viewer admin scoped: %w", err)
	}
	return ok, nil
}
