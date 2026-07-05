package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// repoStore is the SQLite impl of db.RepoStore. SQL bodies are
// ported from the pre-D2 internal/db/repos.go; the only behavioral
// change is the orgID assertion at each method entry. SQLite's
// repo_profiles table has an org_id column with a default pointing
// at the local sentinel, so writes don't need to set it explicitly.
//
// SQLite uses the natural "owner/repo" string as the id column
// directly — no synthetic uuid to translate.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl (SKY-296), but SQLite has one connection — both
// arguments collapse onto the same queryer. The `...System` admin-
// pool variants are thin wrappers around the non-System methods.
type repoStore struct{ q queryer }

func newRepoStore(q, _ queryer) db.RepoStore { return &repoStore{q: q} }

var _ db.RepoStore = (*repoStore)(nil)

func (s *repoStore) Upsert(ctx context.Context, orgID string, p domain.RepoProfile) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO repo_profiles (id, owner, repo, description, has_readme, has_claude_md, has_agents_md, profile_text, clone_url, default_branch, profiled_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			description    = excluded.description,
			has_readme     = excluded.has_readme,
			has_claude_md  = excluded.has_claude_md,
			has_agents_md  = excluded.has_agents_md,
			profile_text   = excluded.profile_text,
			clone_url      = excluded.clone_url,
			default_branch = excluded.default_branch,
			profiled_at    = excluded.profiled_at,
			updated_at     = datetime('now')
	`,
		p.ID, p.Owner, p.Repo,
		nullIfEmpty(p.Description),
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		nullIfEmpty(p.ProfileText),
		nullIfEmpty(p.CloneURL),
		nullIfEmpty(p.DefaultBranch),
		p.ProfiledAt,
	)
	return err
}

func (s *repoStore) List(ctx context.Context, orgID string) ([]domain.RepoProfile, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, owner, repo, description, has_readme, has_claude_md, has_agents_md, profile_text, clone_url, default_branch, base_branch, profiled_at, clone_status, clone_error, clone_error_kind
		FROM repo_profiles
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.RepoProfile{}
	for rows.Next() {
		p, err := scanRepoProfileFull(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListTeamScoped mirrors List in local mode — N=1, so there is no other
// team to scope away (matches the ListActiveJiraTeamScoped /
// FactoryReadStore.Entities local-mode asymmetry, TFAC-559).
func (s *repoStore) ListTeamScoped(ctx context.Context, orgID string) ([]domain.RepoProfile, error) {
	return s.List(ctx, orgID)
}

func (s *repoStore) ListWithContent(ctx context.Context, orgID string) ([]domain.RepoProfile, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, owner, repo, description, has_readme, has_claude_md, has_agents_md, profile_text, clone_url, default_branch, base_branch
		FROM repo_profiles
		WHERE profile_text IS NOT NULL AND profile_text != ''
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.RepoProfile{}
	for rows.Next() {
		var p domain.RepoProfile
		var description, profileText, cloneURL, defaultBranch, baseBranch sql.NullString
		if err := rows.Scan(&p.ID, &p.Owner, &p.Repo, &description, &p.HasReadme, &p.HasClaudeMd, &p.HasAgentsMd, &profileText, &cloneURL, &defaultBranch, &baseBranch); err != nil {
			return nil, err
		}
		p.Description = description.String
		p.ProfileText = profileText.String
		p.CloneURL = cloneURL.String
		p.DefaultBranch = defaultBranch.String
		p.BaseBranch = baseBranch.String
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *repoStore) SetConfigured(ctx context.Context, orgID string, repoNames []string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(tx queryer) error {
		// Build set of desired repos.
		desired := make(map[string]bool, len(repoNames))
		for _, name := range repoNames {
			desired[name] = true
		}

		// Delete repos no longer selected.
		existing, err := listRepoIDsInTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, id := range existing {
			if !desired[id] {
				if _, err := tx.ExecContext(ctx, `DELETE FROM repo_profiles WHERE id = ?`, id); err != nil {
					return err
				}
			}
		}

		// Upsert skeleton rows for new repos. Preserves any existing
		// profile data on the row that's already there — only
		// updated_at gets bumped.
		for _, name := range repoNames {
			owner, repo := splitRepoSlug(name)
			if owner == "" || repo == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO repo_profiles (id, owner, repo, updated_at)
				VALUES (?, ?, ?, datetime('now'))
				ON CONFLICT(id) DO UPDATE SET updated_at = datetime('now')
			`, name, owner, repo); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *repoStore) ListConfiguredNames(ctx context.Context, orgID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `SELECT id FROM repo_profiles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (s *repoStore) CountConfigured(ctx context.Context, orgID string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var count int
	err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM repo_profiles`).Scan(&count)
	return count, err
}

func (s *repoStore) UpdateBaseBranch(ctx context.Context, orgID, repoID, baseBranch string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx,
		`UPDATE repo_profiles SET base_branch = ?, updated_at = datetime('now') WHERE id = ?`,
		nullIfEmpty(baseBranch), repoID,
	)
	return err
}

func (s *repoStore) Get(ctx context.Context, orgID, repoID string) (*domain.RepoProfile, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT id, owner, repo, description, has_readme, has_claude_md, has_agents_md, profile_text, clone_url, default_branch, base_branch, profiled_at, clone_status, clone_error, clone_error_kind
		FROM repo_profiles WHERE id = ?
	`, repoID)
	p, err := scanRepoProfileFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *repoStore) UpdateCloneStatus(ctx context.Context, orgID, owner, repo, status, errMsg, errKind string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE repo_profiles
		SET clone_status = ?, clone_error = ?, clone_error_kind = ?, updated_at = datetime('now')
		WHERE owner = ? AND repo = ?
	`, status, nullIfEmpty(errMsg), nullIfEmpty(errKind), owner, repo)
	return err
}

// --- Admin-pool (`...System`) variants ---
//
// SKY-296 surface — SQLite has one connection so each System variant
// delegates to its non-System counterpart.

func (s *repoStore) ListSystem(ctx context.Context, orgID string) ([]domain.RepoProfile, error) {
	return s.List(ctx, orgID)
}

func (s *repoStore) ListConfiguredNamesSystem(ctx context.Context, orgID string) ([]string, error) {
	return s.ListConfiguredNames(ctx, orgID)
}

func (s *repoStore) UpdateCloneStatusSystem(ctx context.Context, orgID, owner, repo, status, errMsg, errKind string) error {
	return s.UpdateCloneStatus(ctx, orgID, owner, repo, status, errMsg, errKind)
}

func (s *repoStore) CountConfiguredSystem(ctx context.Context, orgID string) (int, error) {
	return s.CountConfigured(ctx, orgID)
}

func (s *repoStore) GetSystem(ctx context.Context, orgID, repoID string) (*domain.RepoProfile, error) {
	return s.Get(ctx, orgID, repoID)
}

func (s *repoStore) UpsertSystem(ctx context.Context, orgID string, p domain.RepoProfile) error {
	return s.Upsert(ctx, orgID, p)
}

func (s *repoStore) GetPullsPollStateSystem(ctx context.Context, orgID, repoID string) (string, *time.Time, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", nil, err
	}
	var etag sql.NullString
	var polledAt sql.NullTime
	err := s.q.QueryRowContext(ctx,
		`SELECT pulls_etag, pulls_polled_at FROM repo_profiles WHERE id = ?`, repoID,
	).Scan(&etag, &polledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var out *time.Time
	if polledAt.Valid {
		out = &polledAt.Time
	}
	return etag.String, out, nil
}

func (s *repoStore) SetPullsPollStateSystem(ctx context.Context, orgID, repoID, etag string, polledAt time.Time) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx,
		`UPDATE repo_profiles SET pulls_etag = ?, pulls_polled_at = ? WHERE id = ?`,
		nullIfEmpty(etag), polledAt, repoID,
	)
	return err
}

// rowScanner is the common Scan surface of *sql.Row and *sql.Rows.
// We need both because Get scans a single row from QueryRowContext
// while List scans repeatedly from QueryContext.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRepoProfileFull reads the 15-column repo_profiles row shape
// shared by Get and List. Distinct from ListWithContent's narrower
// projection — that one skips the clone_* columns.
func scanRepoProfileFull(row rowScanner) (domain.RepoProfile, error) {
	var p domain.RepoProfile
	var description, profileText, cloneURL, defaultBranch, baseBranch, cloneError, cloneErrorKind sql.NullString
	var profiledAt sql.NullTime
	if err := row.Scan(&p.ID, &p.Owner, &p.Repo, &description, &p.HasReadme, &p.HasClaudeMd, &p.HasAgentsMd, &profileText, &cloneURL, &defaultBranch, &baseBranch, &profiledAt, &p.CloneStatus, &cloneError, &cloneErrorKind); err != nil {
		return p, err
	}
	p.Description = description.String
	p.ProfileText = profileText.String
	p.CloneURL = cloneURL.String
	p.DefaultBranch = defaultBranch.String
	p.BaseBranch = baseBranch.String
	p.CloneError = cloneError.String
	p.CloneErrorKind = cloneErrorKind.String
	if profiledAt.Valid {
		p.ProfiledAt = &profiledAt.Time
	}
	return p, nil
}

// listRepoIDsInTx is the SetConfigured helper that lists every id
// inside the tx so we know which to delete. Inlined into the
// closure body would obscure the diff-vs-pre-D2; pulled out for
// readability.
func listRepoIDsInTx(ctx context.Context, tx queryer) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM repo_profiles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// splitRepoSlug splits "owner/repo" at the first slash. Returns
// empty owner+repo if the input has no slash (malformed entry from
// the configured-repos PUT body — silently skipped at use site).
// Local to this file rather than shared because the Postgres impl
// needs its own copy in a different package; the helper is too
// small to be worth exporting.
func splitRepoSlug(s string) (owner, repo string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}
