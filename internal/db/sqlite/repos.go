package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// repoStore is the SQLite impl of db.RepositoryStore. SQL bodies are
// ported from the pre-D2 internal/db/repos.go; the only behavioral
// change is the orgID assertion at each method entry. SQLite's
// repositories table has an org_id column with a default pointing
// at the local sentinel, so writes don't need to set it explicitly.
//
// SQLite uses the natural "owner/repo" string as the id column
// directly — no synthetic uuid to translate.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl, but SQLite has one connection — both
// arguments collapse onto the same queryer. The `...System` admin-
// pool variants are thin wrappers around the non-System methods.
type repoStore struct{ q queryer }

func newRepositoryStore(q, _ queryer) db.RepositoryStore { return &repoStore{q: q} }

var _ db.RepositoryStore = (*repoStore)(nil)

func (s *repoStore) Upsert(ctx context.Context, orgID string, p domain.Repository) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	source, err := domain.NormalizeRepoSource(p.Source)
	if err != nil {
		return err
	}
	// source is absent from the conflict list: it is a create-time identity
	// column, not profiling metadata. external_id refreshes only when the
	// writer actually has one — COALESCE over the NULLIF keeps an empty
	// write from clearing an id already learned.
	//
	// The conflict target is the folded identity index, not the slug primary
	// key, so an upsert spelling the repository differently than the stored row
	// updates that row instead of creating a second one. Upsert creates as well
	// as updates, which makes it a create path too; keying it on the
	// case-sensitive id would leave it as the one create path that could still
	// duplicate. id/owner/repo are absent from the SET list, so the stored
	// casing stays sticky.
	_, err = s.q.ExecContext(ctx, `
		INSERT INTO repositories (id, owner, repo, source, external_id, description, has_readme, has_claude_md, has_agents_md, profile_text, clone_url, default_branch, profiled_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(org_id, source, LOWER(owner), LOWER(repo)) DO UPDATE SET
			external_id    = COALESCE(excluded.external_id, repositories.external_id),
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
		p.ID, p.Owner, p.Repo, source,
		nullIfEmpty(p.ExternalID),
		nullIfEmpty(p.Description),
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		nullIfEmpty(p.ProfileText),
		nullIfEmpty(p.CloneURL),
		nullIfEmpty(p.DefaultBranch),
		p.ProfiledAt,
	)
	return err
}

func (s *repoStore) GetOrCreateSystem(ctx context.Context, orgID string, ref domain.RepoRef) (*domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return getOrCreateRepository(ctx, s.q, ref)
}

// getOrCreateRepository is the store method's body, taking a queryer so it
// composes inside a caller's transaction. Its create half is
// insertRepositoryRow, which the tracked-set reconcile in
// team_github_repos.go calls directly — that reconcile has already computed
// which repos are new, so it skips the lookup rather than the insert.
//
// The lookup before the insert is not what prevents a duplicate (the identity
// index is); it is what returns the existing row without writing to it.
func getOrCreateRepository(ctx context.Context, q queryer, ref domain.RepoRef) (*domain.Repository, error) {
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return nil, err
	}
	existing, err := findRepositoryByRef(ctx, q, source, ref)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		if err := insertRepositoryRow(ctx, q, ref); err != nil {
			return nil, err
		}
		// Re-read rather than synthesizing the row: the INSERT is a
		// DO NOTHING, so this is also what returns the winner's row when a
		// concurrent creator got there first.
		created, err := findRepositoryByRef(ctx, q, source, ref)
		if err != nil {
			return nil, err
		}
		if created == nil {
			return nil, fmt.Errorf("repositories row for %s vanished immediately after insert", ref.Slug())
		}
		return created, nil
	}
	// The row stands as it is, except for an id the caller just learned.
	if ref.ExternalID != "" && existing.ExternalID != ref.ExternalID {
		if _, err := q.ExecContext(ctx,
			`UPDATE repositories SET external_id = ?, updated_at = datetime('now') WHERE id = ?`,
			ref.ExternalID, existing.ID,
		); err != nil {
			return nil, fmt.Errorf("record external id for %s: %w", ref.Slug(), err)
		}
		existing.ExternalID = ref.ExternalID
	}
	return existing, nil
}

// findRepositoryByRef looks a repository up by provider identity, matching
// owner/repo case-INSENSITIVELY (GitHub identifiers are). Returns nil when the
// repository has no row.
func findRepositoryByRef(ctx context.Context, q queryer, source string, ref domain.RepoRef) (*domain.Repository, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repositories
		WHERE source = ? AND LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
	`, source, ref.Owner, ref.Repo)
	p, err := scanRepositoryFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// insertRepositoryRow is the single INSERT that brings a repository into
// repositories — get-or-create, SetConfigured, and the tracked-set reconcile
// all create through it, so no create path can write a row without the
// identity columns, and all three agree on what "already exists" means.
//
// "Already exists" means the repositories_identity index: (org_id, source)
// plus the case-FOLDED slug, because GitHub identifiers are case-insensitive
// and one repository is one row however it is spelled. A repository already
// present under different casing keeps its row — and its cached profile, base
// branch, clone state, ETag — rather than gaining a second one, and the index
// is what refuses rather than a guard any writer has to remember.
//
// DO NOTHING carries no conflict target on purpose. This row can violate two
// uniqueness constraints — the identity index and the slug primary key — and
// both mean the same thing, that the repository is already here. A targeted
// clause would raise on whichever one it did not name.
func insertRepositoryRow(ctx context.Context, q queryer, ref domain.RepoRef) error {
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO repositories (id, owner, repo, source, external_id, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT DO NOTHING
	`, ref.Slug(), ref.Owner, ref.Repo, source, nullIfEmpty(ref.ExternalID)); err != nil {
		return fmt.Errorf("insert repositories[%s]: %w", ref.Slug(), err)
	}
	return nil
}

func (s *repoStore) List(ctx context.Context, orgID string) ([]domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repositories
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.Repository{}
	for rows.Next() {
		p, err := scanRepositoryFull(rows)
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
func (s *repoStore) ListTeamScoped(ctx context.Context, orgID string) ([]domain.Repository, error) {
	return s.List(ctx, orgID)
}

func (s *repoStore) ListWithContent(ctx context.Context, orgID string) ([]domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, owner, repo, description, has_readme, has_claude_md, has_agents_md, profile_text, clone_url, default_branch, base_branch
		FROM repositories
		WHERE profile_text IS NOT NULL AND profile_text != ''
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.Repository{}
	for rows.Next() {
		var p domain.Repository
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
		// Build the desired set case-folded, and compare folded — the same
		// rule the tracked-set reconcile uses, and for the same reason.
		// A case-SENSITIVE compare turns a resubmission under different
		// casing ("Owner/Repo" then "owner/repo") into a delete of a
		// repository that is still selected, followed by a create of a bare
		// row: the profile text, doc flags, clone URL and status, base branch
		// and poll ETag all go, silently, because a person capitalized it
		// differently. GitHub identifiers are case-insensitive, so the two
		// spellings were never two repositories.
		desired := make(map[string]bool, len(repoNames))
		for _, name := range repoNames {
			desired[strings.ToLower(name)] = true
		}

		// Delete repos no longer selected.
		existing, err := listRepoIDsInTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, id := range existing {
			if !desired[strings.ToLower(id)] {
				if _, err := tx.ExecContext(ctx, `DELETE FROM repositories WHERE id = ?`, id); err != nil {
					return err
				}
			}
		}

		// Create skeleton rows for new repos through the shared insert, so
		// they carry the same identity columns the tracked-set reconcile
		// writes. A repo already present is left exactly as it stands — the
		// insert conflicts on the folded identity index and does nothing, so
		// a resubmission under different casing keeps the stored casing and
		// every cached column with it.
		for _, name := range repoNames {
			owner, repo := splitRepoSlug(name)
			if owner == "" || repo == "" {
				continue
			}
			if err := insertRepositoryRow(ctx, tx, domain.RepoRef{Owner: owner, Repo: repo}); err != nil {
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
	rows, err := s.q.QueryContext(ctx, `SELECT id FROM repositories ORDER BY id`)
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
	err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM repositories`).Scan(&count)
	return count, err
}

func (s *repoStore) UpdateBaseBranch(ctx context.Context, orgID, repoID, baseBranch string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// Case-insensitive match (GitHub identifiers are, per TracksRepoSystem) —
	// TFAC-559's repoAccessAllowed gate matches case-insensitively, so a
	// caller reaching this with different casing than what's stored must
	// still find the row rather than silently affecting 0.
	_, err := s.q.ExecContext(ctx,
		`UPDATE repositories SET base_branch = ?, updated_at = datetime('now') WHERE LOWER(id) = LOWER(?)`,
		nullIfEmpty(baseBranch), repoID,
	)
	return err
}

func (s *repoStore) SeedCloneURL(ctx context.Context, orgID, repoID, cloneURL string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if strings.TrimSpace(cloneURL) == "" {
		return nil
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE repositories
		   SET clone_url = ?, updated_at = datetime('now')
		 WHERE LOWER(id) = LOWER(?)
		   AND (clone_url IS NULL OR clone_url = '')
	`, cloneURL, repoID)
	return err
}

func (s *repoStore) Get(ctx context.Context, orgID, repoID string) (*domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Case-insensitive, like every other lookup that takes a caller-supplied
	// slug. The callers that pass one they did NOT read out of this table are
	// the ones that matter: `tfac exec workspace add owner/repo` hands the
	// agent's argv straight through, and a miss there is reported as "repo is
	// not configured in Triage Factory" — while the team-tracking gate on the
	// next line of that same function matches case-insensitively and would
	// have said yes.
	row := s.q.QueryRowContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repositories WHERE LOWER(id) = LOWER(?)
	`, repoID)
	p, err := scanRepositoryFull(row)
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
		UPDATE repositories
		SET clone_status = ?, clone_error = ?, clone_error_kind = ?, updated_at = datetime('now')
		WHERE LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
	`, status, nullIfEmpty(errMsg), nullIfEmpty(errKind), owner, repo)
	return err
}

// --- Admin-pool (`...System`) variants ---
//
// SQLite has one connection so each System variant
// delegates to its non-System counterpart.

func (s *repoStore) ListSystem(ctx context.Context, orgID string) ([]domain.Repository, error) {
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

func (s *repoStore) GetSystem(ctx context.Context, orgID, repoID string) (*domain.Repository, error) {
	return s.Get(ctx, orgID, repoID)
}

func (s *repoStore) UpsertSystem(ctx context.Context, orgID string, p domain.Repository) error {
	return s.Upsert(ctx, orgID, p)
}

func (s *repoStore) FillMissingExternalIDsSystem(ctx context.Context, orgID string, refs []domain.RepoRef) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	if len(refs) == 0 {
		return 0, nil
	}
	filled := 0
	// One statement per ref rather than a bulk form: SQLite is
	// single-connection and the tracked set is tens of repos, so the loop
	// costs nothing the bulk shape would save, and it keeps the WHERE
	// identical to every other slug lookup in this file.
	err := inTx(ctx, s.q, func(tx queryer) error {
		for _, ref := range refs {
			if ref.ExternalID == "" {
				continue
			}
			source, err := domain.NormalizeRepoSource(ref.Source)
			if err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `
				UPDATE repositories
				   SET external_id = ?, updated_at = datetime('now')
				 WHERE source = ?
				   AND LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
				   AND external_id IS NULL
			`, ref.ExternalID, source, ref.Owner, ref.Repo)
			if err != nil {
				return fmt.Errorf("fill external id for %s: %w", ref.Slug(), err)
			}
			if n, err := res.RowsAffected(); err == nil {
				filled += int(n)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return filled, nil
}

func (s *repoStore) GetPullsPollStateSystem(ctx context.Context, orgID, repoID string) (string, *time.Time, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", nil, err
	}
	var etag sql.NullString
	var polledAt sql.NullTime
	err := s.q.QueryRowContext(ctx,
		`SELECT pulls_etag, pulls_polled_at FROM repositories WHERE LOWER(id) = LOWER(?)`, repoID,
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
		`UPDATE repositories SET pulls_etag = ?, pulls_polled_at = ? WHERE LOWER(id) = LOWER(?)`,
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

// repoProfileFullColumns is the projection scanRepositoryFull reads, named
// once so the three queries that share it cannot drift out of column order.
const repoProfileFullColumns = `id, owner, repo, source, external_id, description, has_readme, has_claude_md, has_agents_md, profile_text, clone_url, default_branch, base_branch, profiled_at, clone_status, clone_error, clone_error_kind`

// scanRepositoryFull reads the repositories row shape shared by Get, List,
// and the get-or-create lookup. Distinct from ListWithContent's narrower
// projection — that one skips the identity and clone_* columns.
func scanRepositoryFull(row rowScanner) (domain.Repository, error) {
	var p domain.Repository
	var externalID, description, profileText, cloneURL, defaultBranch, baseBranch, cloneError, cloneErrorKind sql.NullString
	var profiledAt sql.NullTime
	if err := row.Scan(&p.ID, &p.Owner, &p.Repo, &p.Source, &externalID, &description, &p.HasReadme, &p.HasClaudeMd, &p.HasAgentsMd, &profileText, &cloneURL, &defaultBranch, &baseBranch, &profiledAt, &p.CloneStatus, &cloneError, &cloneErrorKind); err != nil {
		return p, err
	}
	p.ExternalID = externalID.String
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
	rows, err := tx.QueryContext(ctx, `SELECT id FROM repositories`)
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
