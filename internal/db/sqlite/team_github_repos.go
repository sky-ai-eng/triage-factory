package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamGitHubReposStore is the SQLite impl of db.TeamGitHubReposStore.
// The constructor accepts two queryers for signature parity with the
// Postgres impl; SQLite has one connection so both collapse. The
// `...System` variants delegate to their non-System counterparts, and
// because there is a single connection the registry writes run inside the
// same transaction as the team-row write — no cross-pool visibility
// concern like the Postgres impl has.
//
// A row points at a repository by the registry row's id. The store's own
// surface is still (owner, repo): that is what a request body carries and what
// the router gate is asked about, so every read joins the registry to project
// the slug and every write resolves the slug to a row first.
type teamGitHubReposStore struct{ q queryer }

func newTeamGitHubReposStore(q, _ queryer) db.TeamGitHubReposStore {
	return &teamGitHubReposStore{q: q}
}

var _ db.TeamGitHubReposStore = (*teamGitHubReposStore)(nil)

func (s *teamGitHubReposStore) ListForTeam(ctx context.Context, teamID string) ([]domain.TeamGitHubRepo, error) {
	return listTeamGitHubRepos(ctx, s.q, teamID)
}

func (s *teamGitHubReposStore) ListForTeamSystem(ctx context.Context, teamID string) ([]domain.TeamGitHubRepo, error) {
	return listTeamGitHubRepos(ctx, s.q, teamID)
}

func listTeamGitHubRepos(ctx context.Context, q queryer, teamID string) ([]domain.TeamGitHubRepo, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT r.owner, r.repo
		FROM team_github_repos g
		JOIN repositories r ON r.id = g.repository_id
		WHERE g.team_id = ?
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
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return listTeamGitHubReposUnion(ctx, s.q)
}

// listTeamGitHubReposUnion returns the DISTINCT (owner, repo) across
// every team's rows — the org-wide union. SQLite is single-org so no
// org filter is needed; the union spans all team_github_repos rows.
func listTeamGitHubReposUnion(ctx context.Context, q queryer) ([]domain.TeamGitHubRepo, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT DISTINCT r.owner, r.repo
		FROM team_github_repos g
		JOIN repositories r ON r.id = g.repository_id
		ORDER BY r.owner ASC, r.repo ASC
	`)
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

func (s *teamGitHubReposStore) ListOrgReposWithTeamsSystem(ctx context.Context, orgID string) ([]domain.TrackedRepoTeams, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT r.owner, r.repo, t.name
		FROM team_github_repos g
		JOIN teams t ON t.id = g.team_id
		JOIN repositories r ON r.id = g.repository_id
		ORDER BY r.owner ASC, r.repo ASC, t.name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("read team_github_repos with teams: %w", err)
	}
	defer rows.Close()
	out, err := scanTrackedRepoTeams(rows)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scanTrackedRepoTeams collapses (owner, repo, team_name) rows ordered by
// (owner, repo, team_name) into one TrackedRepoTeams per repo with its team
// list.
func scanTrackedRepoTeams(rows *sql.Rows) ([]domain.TrackedRepoTeams, error) {
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

func (s *teamGitHubReposStore) ReplaceForTeam(ctx context.Context, orgID, teamID string, repos []domain.TeamGitHubRepo) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	norm, err := domain.NormalizeTeamGitHubRepos(repos)
	if err != nil {
		return err
	}
	return inTx(ctx, s.q, func(tx queryer) error {
		// The two arguments have to describe one tenant. The composite foreign
		// keys on team_github_repos are what enforce that; this check is here
		// to fail with a name instead of "FOREIGN KEY constraint failed", and
		// to fail BEFORE the registry mint below, so a refused save leaves no
		// bare repository row behind.
		if err := assertTeamInOrg(ctx, tx, orgID, teamID); err != nil {
			return err
		}

		// The registry row comes first: a tracking row references it, so it has
		// to exist before the insert that points at it. This is also the whole
		// of what used to be a reconcile — the union recompute existed to keep
		// a derived cache in step, and the FK has taken that job over.
		// ON CONFLICT DO NOTHING preserves the original created_at for repos a
		// team already tracked.
		ids := make([]string, 0, len(norm))
		for _, r := range norm {
			id, err := getOrCreateRepositoryID(ctx, tx, domain.RepoRef{Owner: r.Owner, Repo: r.Repo})
			if err != nil {
				return fmt.Errorf("resolve repository %s/%s: %w", r.Owner, r.Repo, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO team_github_repos (team_id, repository_id, org_id)
				VALUES (?, ?, ?)
				ON CONFLICT(team_id, repository_id) DO NOTHING
			`, teamID, id, orgID); err != nil {
				return fmt.Errorf("insert team_github_repos[%s/%s]: %w", r.Owner, r.Repo, err)
			}
			ids = append(ids, id)
		}

		// Prune rows no longer desired. SQLite has no array binding, so
		// build the NOT IN placeholder list dynamically; an empty desired
		// set clears the team unconditionally.
		//
		// Untracking stops here. The registry row survives — a worktree
		// ledger entry, a pinned project or a task may still name the
		// repository, and tracking is forward-only in both directions.
		if len(ids) == 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM team_github_repos WHERE team_id = ?`, teamID,
			); err != nil {
				return fmt.Errorf("clear team_github_repos: %w", err)
			}
			return nil
		}
		placeholders := make([]string, len(ids))
		args := make([]any, 0, len(ids)+1)
		args = append(args, teamID)
		for i, id := range ids {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query := fmt.Sprintf(
			`DELETE FROM team_github_repos WHERE team_id = ? AND repository_id NOT IN (%s)`,
			strings.Join(placeholders, ", "),
		)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("prune team_github_repos: %w", err)
		}
		return nil
	})
}

func (s *teamGitHubReposStore) TracksRepoSystem(ctx context.Context, teamID, owner, repo string) (bool, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `
		SELECT 1 FROM team_github_repos g
		JOIN repositories r ON r.id = g.repository_id
		WHERE g.team_id = ? AND LOWER(r.owner) = LOWER(?) AND LOWER(r.repo) = LOWER(?)
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

// RepoUpdateRecipientsSystem — identical query to the Postgres impl so
// both backends pin to one result (the TeamIDsForUserInOrgSystem
// precedent for claims-free system reads). Production local mode never
// calls it — the repoevent.Notifier is built without a RecipientsFunc
// there and broadcasts org-wide — but the shared conformance suite
// exercises it, so N=1 must not be assumed here.
func (s *teamGitHubReposStore) RepoUpdateRecipientsSystem(ctx context.Context, orgID, owner, repo string) ([]string, error) {
	// UNION (not UNION ALL) dedups a user who is both an org admin and a
	// tracking-team member. No teams.deleted_at filter, deliberately —
	// see the Postgres impl's comment: the tracking arm mirrors the REST
	// read's visibility scoping, and archiving hides nothing.
	rows, err := s.q.QueryContext(ctx, `
		SELECT user_id FROM (
			SELECT om.user_id
			FROM org_memberships om
			WHERE om.org_id = ? AND om.role IN ('owner', 'admin')
			UNION
			SELECT m.user_id
			FROM team_github_repos g
			JOIN teams t ON t.id = g.team_id
			JOIN repositories r ON r.id = g.repository_id
			JOIN memberships m ON m.team_id = g.team_id
			WHERE t.org_id = ?
			  AND LOWER(r.owner) = LOWER(?) AND LOWER(r.repo) = LOWER(?)
		)
		ORDER BY user_id ASC
	`, orgID, orgID, owner, repo)
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

// TracksRepoViewerScoped always reports true — this backend only ever runs
// local mode, where N=1 has no team boundary to enforce, mirroring the
// local-mode asymmetry of ListActiveJiraTeamScoped /
// FactoryReadStore.Entities (TFAC-559).
//
// The handler gate never reaches it — isOrgAdmin short-circuits to true in
// local before any store call, and multi-mode resolves against the Postgres
// implementation instead. It exists for interface conformance, and for
// store-level callers (tests) that address it directly.
func (s *teamGitHubReposStore) TracksRepoViewerScoped(ctx context.Context, orgID, owner, repo string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	return true, nil
}

// TracksRepoViewerAdminScoped reports true for the same reason as
// TracksRepoViewerScoped: this backend only ever runs local mode, where
// N=1 has a single implicit owner and so neither a team boundary nor an
// admin/member distinction to enforce.
//
// The handler gate never reaches it — isOrgAdmin short-circuits to true
// in local before any store call, and multi-mode resolves against the
// Postgres implementation instead. It exists for interface conformance,
// and for store-level callers (tests) that address it directly.
func (s *teamGitHubReposStore) TracksRepoViewerAdminScoped(ctx context.Context, orgID, owner, repo string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	return true, nil
}

// assertTeamInOrg refuses a write whose orgID and teamID name different
// tenants. Local mode is N=1, so assertLocalOrg has already pinned orgID and
// this can only fire on a teamID from nowhere — which is still worth naming,
// since the tracking row it would write is one no org-scoped read returns.
func assertTeamInOrg(ctx context.Context, q queryer, orgID, teamID string) error {
	var ok bool
	if err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM teams WHERE id = ? AND org_id = ?)
	`, teamID, orgID).Scan(&ok); err != nil {
		return fmt.Errorf("verify team %s belongs to org %s: %w", teamID, orgID, err)
	}
	if !ok {
		return fmt.Errorf("%w: team %s, org %s", db.ErrTeamNotInOrg, teamID, orgID)
	}
	return nil
}
