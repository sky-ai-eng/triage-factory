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
//   - admin: ListForTeamSystem, ListForOrgSystem, TracksRepoSystem, and
//     the repo_profiles reconcile inside ReplaceForTeam. The union read
//     must see every team's rows org-wide, which RLS on the app pool
//     would hide; the reconcile write commits autonomously from the
//     surrounding tx (same shape EventStore.RecordSystem uses).
//   - app: ListForTeam + the team-row write inside ReplaceForTeam.
//     Request-handler reads/writes gated by team_github_repos_select /
//     _insert / _update / _delete (team membership / team admin).
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
	return scanRepoRows(s.admin.QueryContext(ctx, `
		SELECT DISTINCT g.owner, g.repo
		FROM team_github_repos g
		JOIN teams t ON t.id = g.team_id
		WHERE t.org_id = $1
		ORDER BY g.owner ASC, g.repo ASC
	`, orgID))
}

// listSiblingTeamRepos returns the DISTINCT (owner, repo) union across
// the org's teams *excluding* excludeTeamID. Used by the reconcile to
// read the committed state of sibling teams and merge the mutating
// team's new desired set in memory (the mutating team's rows are still
// in the uncommitted outer tx, invisible to this admin-pool read). org
// scope rides the teams join — team_github_repos carries no org_id.
func listSiblingTeamRepos(ctx context.Context, q queryer, orgID, excludeTeamID string) ([]domain.TeamGitHubRepo, error) {
	return scanRepoRows(q.QueryContext(ctx, `
		SELECT DISTINCT g.owner, g.repo
		FROM team_github_repos g
		JOIN teams t ON t.id = g.team_id
		WHERE t.org_id = $1
		  AND g.team_id <> $2
		ORDER BY g.owner ASC, g.repo ASC
	`, orgID, excludeTeamID))
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

func (s *teamGitHubReposStore) ReplaceForTeam(ctx context.Context, teamID string, repos []domain.TeamGitHubRepo) error {
	norm, err := domain.NormalizeTeamGitHubRepos(repos)
	if err != nil {
		return err
	}

	// Phase 1: replace the team's rows on the app pool (RLS gates by
	// team admin), composed in the surrounding claims tx.
	if err := inTx(ctx, s.app, func(tx queryer) error {
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
			return nil
		}
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
		return nil
	}); err != nil {
		return err
	}

	// Phase 2: reconcile repo_profiles to the org-wide union on the admin
	// pool (must see sibling teams' rows; commits autonomously from the
	// outer tx). The mutating team's new rows are still uncommitted, so
	// compute the post-state union as committed-sibling-teams ∪ norm.
	var orgID string
	if err := s.admin.QueryRowContext(ctx,
		`SELECT org_id FROM teams WHERE id = $1`, teamID,
	).Scan(&orgID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reconcile repo_profiles: team %s not found", teamID)
		}
		return fmt.Errorf("reconcile repo_profiles: resolve org for team %s: %w", teamID, err)
	}

	siblings, err := listSiblingTeamRepos(ctx, s.admin, orgID, teamID)
	if err != nil {
		return err
	}
	union := unionRepos(siblings, norm)
	return reconcileRepoProfilesFromUnion(ctx, s.admin, orgID, union)
}

// reconcileRepoProfilesFromUnion makes repo_profiles (for orgID) match
// the union of tracked repos: deletes rows for repos no team tracks
// anymore and upserts skeleton rows for newly-tracked repos (preserving
// any cached profile on surviving rows). Mirrors RepoStore.SetConfigured's
// delete-removed / upsert-skeleton logic — repo_profiles is now a derived
// cache of team_github_repos.
func reconcileRepoProfilesFromUnion(ctx context.Context, q queryer, orgID string, union []domain.TeamGitHubRepo) error {
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
		WHERE team_id = $1 AND lower(owner) = lower($2) AND repo = $3
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

// unionRepos merges two repo slices, de-duplicating on (owner, repo)
// with first-seen order. Used to compose the post-mutation org union
// from committed sibling-team rows plus the mutating team's new set.
func unionRepos(a, b []domain.TeamGitHubRepo) []domain.TeamGitHubRepo {
	seen := map[domain.TeamGitHubRepo]bool{}
	out := make([]domain.TeamGitHubRepo, 0, len(a)+len(b))
	for _, r := range append(append([]domain.TeamGitHubRepo{}, a...), b...) {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}
