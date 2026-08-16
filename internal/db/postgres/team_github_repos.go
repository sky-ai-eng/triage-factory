package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamGitHubReposStore is the Postgres impl of db.TeamGitHubReposStore.
// Holds both pools — see the TeamGitHubReposStore interface comment for
// the pool-split rationale.
//
//   - app: ListForTeam + the whole of ReplaceForTeam (team-row write +
//     repositories reconcile, atomic in the caller's claims tx, org-
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

func (s *teamGitHubReposStore) ListOrgReposWithTeamsSystem(ctx context.Context, orgID string) ([]domain.TrackedRepoTeams, error) {
	// Admin pool: the preflight's org admin must see every team's tracking,
	// including teams they don't belong to (the app-pool SELECT policy is
	// team-membership-scoped). Org scope rides the teams join + the org_id
	// filter, mirroring listTeamGitHubReposForOrg.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT g.owner, g.repo, t.name
		FROM team_github_repos g
		JOIN teams t ON t.id = g.team_id
		WHERE t.org_id = $1
		ORDER BY g.owner ASC, g.repo ASC, t.name ASC
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
	// write by team admin) so the team-row mutation and the repositories
	// reconcile are atomic. A per-org transaction advisory lock serializes
	// concurrent same-org saves so the union recompute can't race a
	// sibling team's write into an inconsistent repositories table. The key
	// is hashtextextended (64-bit) so distinct orgs don't collide onto the
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
		return reconcileRepositoriesFromUnion(ctx, tx, orgID, union)
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

// reconcileRepositoriesFromUnion makes repositories (for orgID) match
// the union of tracked repos: deletes rows for repos no team tracks
// anymore and inserts skeleton rows for newly-tracked repos. Mirrors
// RepositoryStore.SetConfigured's delete-removed / insert-skeleton logic —
// the repositories table is now a derived cache of team_github_repos. The
// union is case-folded to one canonical row per logical repo
// (NormalizeTeamGitHubRepos).
//
// Matching is case-INSENSITIVE on both axes (GitHub identifiers are): a
// repo still tracked under different casing keeps its existing row rather
// than being deleted + reinserted, so the cached profile_text /
// base_branch / clone status / etag survive a mere casing change. Casing
// is therefore sticky — repositories keeps the first casing it was
// created with (case-equivalent to whatever team_github_repos now holds).
func reconcileRepositoriesFromUnion(ctx context.Context, q queryer, orgID string, rawUnion []domain.TeamGitHubRepo) error {
	desired, err := domain.NormalizeTeamGitHubRepos(rawUnion)
	if err != nil {
		return fmt.Errorf("normalize repo union: %w", err)
	}
	// Case-folded keys for the GC match. An empty union deletes every
	// repositories row for the org (unnest of empty arrays is empty, so
	// NOT IN (empty) keeps nothing).
	lowOwners := make([]string, len(desired))
	lowNames := make([]string, len(desired))
	for i, r := range desired {
		lowOwners[i] = strings.ToLower(r.Owner)
		lowNames[i] = strings.ToLower(r.Repo)
	}
	if _, err := q.ExecContext(ctx, `
		DELETE FROM repositories
		WHERE org_id = $1
		  AND (lower(owner), lower(repo)) NOT IN (
		      SELECT * FROM unnest($2::text[], $3::text[])
		  )
	`, orgID, lowOwners, lowNames); err != nil {
		return fmt.Errorf("gc repositories: %w", err)
	}
	// Create a row only for repos not already present case-insensitively;
	// existing rows keep their casing + cached columns (sticky casing). This
	// is the tracking half of the get-or-create invariant — a repository gets
	// its row here, when it is first tracked, not when profiling first
	// succeeds — and it goes through the same insert the store method does so
	// the identity columns cannot differ by which path created the row. The
	// tracked set is GitHub's by construction, so source is the default.
	for _, r := range desired {
		if err := insertRepositoryRow(ctx, q, orgID, domain.RepoRef{Owner: r.Owner, Repo: r.Repo}); err != nil {
			return err
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

// RepoUpdateRecipientsSystem — identical query to the SQLite impl so both
// backends pin to one result. Admin pool: the emitters (profiler) are
// claims-free background jobs, and the union deliberately crosses team
// boundaries the membership RLS policies would hide.
func (s *teamGitHubReposStore) RepoUpdateRecipientsSystem(ctx context.Context, orgID, owner, repo string) ([]string, error) {
	// UNION (not UNION ALL) dedups a user who is both an org admin and a
	// tracking-team member. The tracking arm mirrors
	// repoProfileTrackedByViewerTeams (the REST read's scoping) join for
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
			JOIN memberships m ON m.team_id = g.team_id
			WHERE t.org_id = $1
			  AND lower(g.owner) = lower($2) AND lower(g.repo) = lower($3)
		) u
		ORDER BY user_id ASC
	`, orgID, owner, repo)
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
			WHERE tm.org_id = $1
			  AND lower(g.owner) = lower($2)
			  AND lower(g.repo) = lower($3)
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
			WHERE tm.org_id = $1
			  AND lower(g.owner) = lower($2)
			  AND lower(g.repo) = lower($3)
			  AND tf.user_is_team_admin(g.team_id)
		)
	`, orgID, owner, repo).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("tracks repo viewer admin scoped: %w", err)
	}
	return ok, nil
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
