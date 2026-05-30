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
//   - app: ListForTeam + the whole of ReplaceForTeam (team-row write +
//     repo_profiles reconcile, atomic in the caller's claims tx, org-
//     serialized by an advisory lock). RLS gates the team-row write by
//     team admin; the cross-team union read inside it goes through the
//     tf.org_tracked_repos() SECURITY DEFINER helper.
//   - admin: ListForTeamSystem, ListForOrgSystem, TracksRepoSystem —
//     claims-free reads for the router gate / poll callers.
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
		SELECT owner, repo
		FROM team_github_repos
		WHERE team_id = $1
		ORDER BY owner ASC, repo ASC
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
	// write by team admin) so the team-row mutation and the repo_profiles
	// reconcile are atomic. A per-org transaction advisory lock serializes
	// concurrent same-org saves so the union recompute can't race a
	// sibling team's write into an inconsistent repo_profiles. The key is
	// hashtextextended (64-bit) so distinct orgs don't collide onto the
	// same lock and serialize against each other.
	return inTx(ctx, s.app, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, orgID); err != nil {
			return fmt.Errorf("lock org for repo reconcile: %w", err)
		}

		for _, r := range norm {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO team_github_repos (team_id, owner, repo)
				VALUES ($1, $2, $3)
				ON CONFLICT (team_id, owner, repo) DO NOTHING
			`, teamID, r.Owner, r.Repo); err != nil {
				return fmt.Errorf("insert team_github_repos[%s/%s]: %w", r.Owner, r.Repo, err)
			}
		}
		// Prune rows no longer desired. NOT IN against an empty list keeps
		// every row, so an empty desired set takes the unconditional clear.
		if len(norm) == 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM team_github_repos WHERE team_id = $1`, teamID,
			); err != nil {
				return fmt.Errorf("clear team_github_repos: %w", err)
			}
		} else {
			owners, names := splitRepoColumns(norm)
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM team_github_repos
				WHERE team_id = $1
				  AND (owner, repo) NOT IN (
				      SELECT * FROM unnest($2::text[], $3::text[])
				  )
			`, teamID, owners, names); err != nil {
				return fmt.Errorf("prune team_github_repos: %w", err)
			}
		}

		// Read the org-wide union via the SECURITY DEFINER helper so it sees
		// every team's rows (RLS on the app pool would hide siblings) plus
		// this tx's just-written rows. The helper is org-scoped to the
		// caller's claims when present, so the org boundary holds.
		union, err := scanRepoRows(tx.QueryContext(ctx,
			`SELECT owner, repo FROM tf.org_tracked_repos($1)`, orgID))
		if err != nil {
			return fmt.Errorf("read org %s tracked-repo union: %w", orgID, err)
		}
		return reconcileRepoProfilesFromUnion(ctx, tx, orgID, union)
	})
}

// listTeamGitHubReposForOrg reads the DISTINCT (owner, repo) union across
// the org's teams (committed state on whatever queryer is passed). org
// scope rides the teams join — team_github_repos carries no org_id.
func listTeamGitHubReposForOrg(ctx context.Context, q queryer, orgID string) ([]domain.TeamGitHubRepo, error) {
	return scanRepoRows(q.QueryContext(ctx, `
		SELECT DISTINCT g.owner, g.repo
		FROM team_github_repos g
		JOIN teams t ON t.id = g.team_id
		WHERE t.org_id = $1
		ORDER BY g.owner ASC, g.repo ASC
	`, orgID))
}

// reconcileRepoProfilesFromUnion makes repo_profiles (for orgID) match
// the union of tracked repos: deletes rows for repos no team tracks
// anymore and upserts skeleton rows for newly-tracked repos (preserving
// any cached profile on surviving rows). Mirrors RepoStore.SetConfigured's
// delete-removed / upsert-skeleton logic — repo_profiles is now a derived
// cache of team_github_repos. The union is case-folded to one canonical
// row per logical repo (NormalizeTeamGitHubRepos) so two teams tracking
// the same repo with different casing don't yield two repo_profiles rows.
func reconcileRepoProfilesFromUnion(ctx context.Context, q queryer, orgID string, rawUnion []domain.TeamGitHubRepo) error {
	union, err := domain.NormalizeTeamGitHubRepos(rawUnion)
	if err != nil {
		return fmt.Errorf("normalize repo union: %w", err)
	}
	owners, names := splitRepoColumns(union)
	// Delete repos no team tracks anymore. An empty union deletes every
	// repo_profiles row for the org (unnest of empty arrays is empty, so
	// NOT IN (empty) keeps nothing).
	if _, err := q.ExecContext(ctx, `
		DELETE FROM repo_profiles
		WHERE org_id = $1
		  AND (owner, repo) NOT IN (
		      SELECT * FROM unnest($2::text[], $3::text[])
		  )
	`, orgID, owners, names); err != nil {
		return fmt.Errorf("gc repo_profiles: %w", err)
	}
	for _, r := range union {
		if _, err := q.ExecContext(ctx, `
			INSERT INTO repo_profiles (org_id, owner, repo)
			VALUES ($1, $2, $3)
			ON CONFLICT (org_id, owner, repo) DO UPDATE SET updated_at = now()
		`, orgID, r.Owner, r.Repo); err != nil {
			return fmt.Errorf("upsert repo_profiles skeleton[%s]: %w", r.Slug(), err)
		}
	}
	return nil
}

func (s *teamGitHubReposStore) TracksRepoSystem(ctx context.Context, teamID, owner, repo string) (bool, error) {
	var n int
	err := s.admin.QueryRowContext(ctx, `
		SELECT 1 FROM team_github_repos
		WHERE team_id = $1 AND lower(owner) = lower($2) AND lower(repo) = lower($3)
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

// splitRepoColumns transposes a repo slice into parallel owner + repo
// slices for unnest()-based array binding. Returns empty (non-nil)
// slices for an empty input so unnest yields zero rows.
func splitRepoColumns(repos []domain.TeamGitHubRepo) (owners, names []string) {
	owners = make([]string, 0, len(repos))
	names = make([]string, 0, len(repos))
	for _, r := range repos {
		owners = append(owners, r.Owner)
		names = append(names, r.Repo)
	}
	return owners, names
}
