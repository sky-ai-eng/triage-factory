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
// because there is a single connection the repositories reconcile runs
// inside the same transaction as the team-row write — no cross-pool
// visibility concern like the Postgres impl has.
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
		SELECT owner, repo
		FROM team_github_repos
		WHERE team_id = ?
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
		SELECT DISTINCT owner, repo
		FROM team_github_repos
		ORDER BY owner ASC, repo ASC
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
		SELECT g.owner, g.repo, t.name
		FROM team_github_repos g
		JOIN teams t ON t.id = g.team_id
		ORDER BY g.owner ASC, g.repo ASC, t.name ASC
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
		// Upsert the desired rows. ON CONFLICT DO NOTHING preserves the
		// original created_at for repos a team already tracked.
		for _, r := range norm {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO team_github_repos (team_id, owner, repo)
				VALUES (?, ?, ?)
				ON CONFLICT(team_id, owner, repo) DO NOTHING
			`, teamID, r.Owner, r.Repo); err != nil {
				return fmt.Errorf("insert team_github_repos[%s/%s]: %w", r.Owner, r.Repo, err)
			}
		}

		// Prune rows no longer desired. SQLite has no array binding, so
		// build the NOT IN placeholder list dynamically; an empty desired
		// set clears the team unconditionally.
		if len(norm) == 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM team_github_repos WHERE team_id = ?`, teamID,
			); err != nil {
				return fmt.Errorf("clear team_github_repos: %w", err)
			}
		} else {
			placeholders := make([]string, len(norm))
			args := make([]any, 0, len(norm)*2+1)
			args = append(args, teamID)
			for i, r := range norm {
				placeholders[i] = "(?, ?)"
				args = append(args, r.Owner, r.Repo)
			}
			query := fmt.Sprintf(
				`DELETE FROM team_github_repos WHERE team_id = ? AND (owner, repo) NOT IN (%s)`,
				strings.Join(placeholders, ", "),
			)
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("prune team_github_repos: %w", err)
			}
		}

		// Reconcile repositories to the new org-wide union, in the same tx
		// (atomic with the team-row write). SQLite is single-connection and
		// serializes writers, so the union read sees the rows just written
		// above and no advisory lock is needed — the Postgres impl's per-org
		// lock has no analogue here.
		union, err := listTeamGitHubReposUnion(ctx, tx)
		if err != nil {
			return err
		}
		return reconcileRepositoriesFromUnion(ctx, tx, union)
	})
}

// reconcileRepositoriesFromUnion makes repositories match the union of
// tracked repos: it deletes rows for repos no team tracks anymore and
// inserts skeleton rows for newly-tracked repos. Mirrors
// RepositoryStore.SetConfigured's delete-removed / insert-skeleton logic —
// the repositories table is now a derived cache of team_github_repos.
//
// Matching is case-INSENSITIVE on both axes (GitHub identifiers are): a
// repo still tracked under different casing keeps its existing row rather
// than being deleted + reinserted, so the cached profile_text /
// base_branch / clone status / etag survive a mere casing change. Casing
// is therefore sticky — repositories keeps the first casing it was
// created with (case-equivalent to whatever team_github_repos now holds).
func reconcileRepositoriesFromUnion(ctx context.Context, tx queryer, union []domain.TeamGitHubRepo) error {
	// Case-fold the union to one canonical row per logical repo so two
	// teams tracking the same repo with different casing don't produce
	// two repositories rows. The union arrives owner/repo-ASC, so
	// NormalizeTeamGitHubRepos's first-seen pick is deterministic.
	desired, err := domain.NormalizeTeamGitHubRepos(union)
	if err != nil {
		return fmt.Errorf("normalize repo union: %w", err)
	}
	wantedLower := make(map[string]bool, len(desired))
	for _, r := range desired {
		wantedLower[strings.ToLower(r.Slug())] = true
	}

	existing, err := listRepoIDsInTx(ctx, tx)
	if err != nil {
		return err
	}
	existingLower := make(map[string]bool, len(existing))
	for _, id := range existing {
		existingLower[strings.ToLower(id)] = true
		if !wantedLower[strings.ToLower(id)] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM repositories WHERE id = ?`, id); err != nil {
				return fmt.Errorf("gc repositories[%s]: %w", id, err)
			}
		}
	}
	// Create a row only for genuinely-new repos (not already present
	// case-insensitively); existing rows keep their casing + cached columns.
	// This is the tracking half of the get-or-create invariant — a repository
	// gets its row here, when it is first tracked, not when profiling first
	// succeeds — and it goes through the same insert the store method does so
	// the identity columns cannot differ by which path created the row. The
	// tracked set is GitHub's by construction, so source is the default.
	for _, r := range desired {
		if existingLower[strings.ToLower(r.Slug())] {
			continue
		}
		if err := insertRepositoryRow(ctx, tx, domain.RepoRef{Owner: r.Owner, Repo: r.Repo}); err != nil {
			return err
		}
	}
	return nil
}

func (s *teamGitHubReposStore) TracksRepoSystem(ctx context.Context, teamID, owner, repo string) (bool, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `
		SELECT 1 FROM team_github_repos
		WHERE team_id = ? AND LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
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
			JOIN memberships m ON m.team_id = g.team_id
			WHERE t.org_id = ?
			  AND LOWER(g.owner) = LOWER(?) AND LOWER(g.repo) = LOWER(?)
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
