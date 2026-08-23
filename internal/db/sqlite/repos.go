package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// repoStore is the SQLite impl of db.RepositoryStore. SQLite's
// repositories table has an org_id column with a default pointing
// at the local sentinel, so writes don't need to set it explicitly.
//
// # Row id vs "owner/repo"
//
// The row is keyed on a uuid, with the slug an ordinary column under a folded
// UNIQUE(org_id, source, owner, repo) — the same shape Postgres has always
// had, so the two dialects hold the same reference in the tables that point
// here, and surface the same handle. The id-keyed methods query the primary
// key and refuse a miss with db.ErrNoSuchRepository; the ref-keyed ones query
// the folded natural key and answer a miss with nil.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl, but SQLite has one connection — both
// arguments collapse onto the same queryer. The `...System` admin-
// pool variants are thin wrappers around the non-System methods.
type repoStore struct{ q queryer }

func newRepositoryStore(q, _ queryer) db.RepositoryStore { return &repoStore{q: q} }

var _ db.RepositoryStore = (*repoStore)(nil)

func (s *repoStore) Upsert(ctx context.Context, orgID string, p domain.Repository) (domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Repository{}, err
	}
	source, err := domain.NormalizeRepoSource(p.Source)
	if err != nil {
		return domain.Repository{}, err
	}
	// source is absent from the conflict list: it is a create-time identity
	// column, not profiling metadata. external_id refreshes only when the
	// writer actually has one — COALESCE over the NULLIF keeps an empty
	// write from clearing an id already learned.
	//
	// The conflict target is the folded identity index, so an upsert spelling
	// the repository differently than the stored row updates that row instead of
	// creating a second one. Upsert creates as well as updates, which makes it a
	// create path too; inferring the case-sensitive columns here would leave it
	// as the one create path that could still duplicate. owner/repo are absent
	// from the SET list, so the stored casing stays sticky, and the id minted
	// for a conflicting INSERT is discarded along with the rest of it.
	//
	// RETURNING projects the point read's column list, so the row handed back
	// carries the COALESCEd external_id, the stored casing and the base branch
	// and clone state this statement left alone — none of which p can describe.
	row := s.q.QueryRowContext(ctx, `
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
		RETURNING `+repoProfileFullColumns,
		uuid.New().String(), p.Owner, p.Repo, source,
		nullIfEmpty(p.ExternalID),
		nullIfEmpty(p.Description),
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		nullIfEmpty(p.ProfileText),
		nullIfEmpty(p.CloneURL),
		nullIfEmpty(p.DefaultBranch),
		p.ProfiledAt,
	)
	return scanRepositoryFull(row)
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
		if _, err := q.ExecContext(ctx, `
			UPDATE repositories
			   SET external_id = ?, updated_at = datetime('now')
			 WHERE source = ? AND LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
		`, ref.ExternalID, source, existing.Owner, existing.Repo); err != nil {
			return nil, fmt.Errorf("record external id for %s: %w", ref.Slug(), err)
		}
		existing.ExternalID = ref.ExternalID
	}
	return existing, nil
}

// getOrCreateRepositoryID returns the surrogate id of one repository's registry
// row, minting the row if it does not exist yet. It is the resolver every
// reference site uses: a caller holding a slug — a tracked-set save, a
// worktree reservation, a pinned project — needs the id the FK points at, and
// the store contract keeps the surrogate out of domain.Repository.
func getOrCreateRepositoryID(ctx context.Context, q queryer, ref domain.RepoRef) (string, error) {
	if _, err := getOrCreateRepository(ctx, q, ref); err != nil {
		return "", err
	}
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return "", err
	}
	id, err := findRepositoryID(ctx, q, source, ref.Owner, ref.Repo)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("repositories row for %s vanished immediately after create", ref.Slug())
	}
	return id, nil
}

// findRepositoryID resolves a slug to the registry row's surrogate id, or "" if
// no row holds that slug. Case-INSENSITIVE, like every other slug lookup here.
func findRepositoryID(ctx context.Context, q queryer, source, owner, repo string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT id FROM repositories
		 WHERE source = ? AND LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
	`, source, owner, repo).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
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
// DO NOTHING carries no conflict target on purpose: every uniqueness
// constraint this row can violate means the same thing, that the repository is
// already here, and a targeted clause would raise on the one it did not name.
// The id is minted here rather than defaulted in the schema because SQLite has
// no uuid generator to put in a DEFAULT; a losing INSERT throws its id away.
func insertRepositoryRow(ctx context.Context, q queryer, ref domain.RepoRef) error {
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO repositories (id, owner, repo, source, external_id, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT DO NOTHING
	`, uuid.New().String(), ref.Owner, ref.Repo, source, nullIfEmpty(ref.ExternalID)); err != nil {
		return fmt.Errorf("insert repositories[%s]: %w", ref.Slug(), err)
	}
	return nil
}

func (s *repoStore) List(ctx context.Context, orgID string, opts db.ListOpts) ([]domain.Repository, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM repositories`).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.Repository{}, total, nil
	}
	query := `
		SELECT ` + repoProfileFullColumns + `
		FROM repositories
		ORDER BY owner, repo, id`
	args := []any{}
	if opts.Limit > 0 {
		query += `
		LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []domain.Repository{}
	for rows.Next() {
		p, err := scanRepositoryFull(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

// ListTeamScoped mirrors List in local mode — N=1, so there is no other
// team to scope away (matches the ListActiveJiraTeamScoped /
// FactoryReadStore.Entities local-mode asymmetry, TFAC-559).
func (s *repoStore) ListTeamScoped(ctx context.Context, orgID string, opts db.ListOpts) ([]domain.Repository, int, error) {
	return s.List(ctx, orgID, opts)
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
		existing, err := listRepoRowsInTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, row := range existing {
			if !desired[strings.ToLower(row.slug)] {
				if _, err := tx.ExecContext(ctx, `DELETE FROM repositories WHERE id = ?`, row.id); err != nil {
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
	rows, err := s.q.QueryContext(ctx, `SELECT owner || '/' || repo FROM repositories ORDER BY owner, repo`)
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

func (s *repoStore) UpdateBaseBranch(ctx context.Context, orgID, id, baseBranch string) (domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Repository{}, err
	}
	row := s.q.QueryRowContext(ctx, `
		UPDATE repositories
		   SET base_branch = ?, updated_at = datetime('now')
		 WHERE id = ?
		RETURNING `+repoProfileFullColumns,
		nullIfEmpty(baseBranch), id)
	return scanUpdatedRepository(row, id)
}

func (s *repoStore) Get(ctx context.Context, orgID, id string) (*domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repositories WHERE id = ?
	`, id)
	p, err := scanRepositoryFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", db.ErrNoSuchRepository, id)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *repoStore) GetByRef(ctx context.Context, orgID string, ref domain.RepoRef) (*domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return nil, err
	}
	// Case-insensitive, like every other ref lookup here. The callers that
	// matter are the ones holding a name they did NOT read out of this table:
	// `tfac exec workspace add owner/repo` hands the agent's argv straight
	// through, and a miss there is reported as "repo is not configured in
	// Triage Factory" — while the team-tracking gate on the next line of that
	// same function matches case-insensitively and would have said yes.
	return findRepositoryByRef(ctx, s.q, source, ref)
}

func (s *repoStore) UpdateCloneStatusByRef(ctx context.Context, orgID string, ref domain.RepoRef, status, errMsg, errKind string) (*domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		UPDATE repositories
		SET clone_status = ?, clone_error = ?, clone_error_kind = ?, updated_at = datetime('now')
		WHERE source = ? AND LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
		RETURNING `+repoProfileFullColumns,
		status, nullIfEmpty(errMsg), nullIfEmpty(errKind), source, ref.Owner, ref.Repo)
	p, err := scanRepositoryFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing answers to the name — the documented no-op, not a fault.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// scanUpdatedRepository reads the row an id-keyed UPDATE … RETURNING produced,
// turning the empty result — the write matched nothing — into
// db.ErrNoSuchRepository. It replaces the rows-affected probe these writes used
// to run: RETURNING answers "did this land" and "what does the row say now" in
// one statement, so there is nothing left to count.
func scanUpdatedRepository(row rowScanner, id string) (domain.Repository, error) {
	p, err := scanRepositoryFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Repository{}, fmt.Errorf("%w: %s", db.ErrNoSuchRepository, id)
	}
	if err != nil {
		return domain.Repository{}, err
	}
	return p, nil
}

// --- Admin-pool (`...System`) variants ---
//
// SQLite has one connection so each System variant
// delegates to its non-System counterpart.

func (s *repoStore) ListSystem(ctx context.Context, orgID string) ([]domain.Repository, error) {
	// Unwindowed: system callers resolve the whole registry.
	rows, _, err := s.List(ctx, orgID, db.Unwindowed)
	return rows, err
}

func (s *repoStore) ListTrackedNamesSystem(ctx context.Context, orgID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT r.owner || '/' || r.repo
		  FROM repositories r
		  JOIN team_github_repos g ON g.repository_id = r.id
		 ORDER BY r.owner, r.repo
	`)
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

func (s *repoStore) UpdateCloneStatusByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef, status, errMsg, errKind string) (*domain.Repository, error) {
	return s.UpdateCloneStatusByRef(ctx, orgID, ref, status, errMsg, errKind)
}

func (s *repoStore) CountConfiguredSystem(ctx context.Context, orgID string) (int, error) {
	return s.CountConfigured(ctx, orgID)
}

func (s *repoStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Repository, error) {
	return s.Get(ctx, orgID, id)
}

func (s *repoStore) GetByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef) (*domain.Repository, error) {
	return s.GetByRef(ctx, orgID, ref)
}

func (s *repoStore) UpsertSystem(ctx context.Context, orgID string, p domain.Repository) (domain.Repository, error) {
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

func (s *repoStore) GetPullsPollStateByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef) (string, *time.Time, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", nil, err
	}
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return "", nil, err
	}
	var etag sql.NullString
	var polledAt sql.NullTime
	err = s.q.QueryRowContext(ctx, `
		SELECT pulls_etag, pulls_polled_at
		  FROM repositories
		 WHERE source = ? AND LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
	`, source, ref.Owner, ref.Repo).Scan(&etag, &polledAt)
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

func (s *repoStore) SetPullsPollStateByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef, etag string, polledAt time.Time) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return err
	}
	_, err = s.q.ExecContext(ctx, `
		UPDATE repositories
		   SET pulls_etag = ?, pulls_polled_at = ?
		 WHERE source = ? AND LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
	`, nullIfEmpty(etag), polledAt.UTC(), source, ref.Owner, ref.Repo)
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
// and the ref lookup.
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

// repoRow pairs a registry row's surrogate id with its slug: the slug is what
// a caller's desired set is expressed in, the id is what a DELETE keys on.
type repoRow struct{ id, slug string }

// listRepoRowsInTx is the SetConfigured helper that lists every registry row
// inside the tx so we know which to delete.
func listRepoRowsInTx(ctx context.Context, tx queryer) ([]repoRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, owner || '/' || repo FROM repositories`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repoRow
	for rows.Next() {
		var r repoRow
		if err := rows.Scan(&r.id, &r.slug); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
