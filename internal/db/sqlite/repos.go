package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// UNIQUE(org_id, source, host, owner, repo) — the same shape Postgres has always
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
	if err := db.RequireRepoHost(p.Host); err != nil {
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
	// for a conflicting INSERT is discarded along with the rest of it. host is
	// part of the target, so a same-named repository on another host is another
	// row. The index is partial over rows that are not retired, and the target
	// names its predicate, so a retired row under the name is never the one
	// updated: the upsert lands on, or creates, the row that holds the name.
	//
	// The id written is p.ExternalID unless another row on the host already
	// carries it — a rename not yet applied, or one refused, or a retired row
	// under this same name, which is not the row the upsert lands on and is
	// restored only through RenameSystem. The scalar subquery answers NULL
	// then, and COALESCE keeps whatever this row had.
	//
	// p.OwnerExternalID goes with that id: the VALUES carry it only when the
	// id is written, so the conflict arm's COALESCE writes it onto a row that
	// carries p.ExternalID once the statement lands and keeps the stored one
	// otherwise. An owner id read off a response about one repository is not
	// written onto a row that is another, and an empty one clears nothing.
	//
	// RETURNING projects the point read's column list, so the row handed back
	// carries the COALESCEd external_id, the stored casing and the base branch
	// and clone state this statement left alone — none of which p can describe.
	learnedID := `(SELECT ?6 WHERE NOT EXISTS (
		            SELECT 1 FROM repositories o
		             WHERE o.source = ?4 AND o.host = ?5 AND o.external_id = ?6
		               AND NOT (o.retired_at IS NULL AND LOWER(o.owner) = LOWER(?2) AND LOWER(o.repo) = LOWER(?3))))`
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO repositories (id, owner, repo, source, host, external_id, owner_external_id, description, has_readme, has_claude_md, has_agents_md, profile_text, clone_url, default_branch, profiled_at, updated_at)
		VALUES (?1, ?2, ?3, ?4, ?5,
		        `+learnedID+`,
		        CASE WHEN `+learnedID+` IS NOT NULL THEN ?15 END,
		        ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, datetime('now'))
		ON CONFLICT(org_id, source, host, LOWER(owner), LOWER(repo)) WHERE retired_at IS NULL DO UPDATE SET
			external_id       = COALESCE(excluded.external_id, repositories.external_id),
			owner_external_id = COALESCE(excluded.owner_external_id, repositories.owner_external_id),
			description       = excluded.description,
			has_readme        = excluded.has_readme,
			has_claude_md     = excluded.has_claude_md,
			has_agents_md     = excluded.has_agents_md,
			profile_text      = excluded.profile_text,
			clone_url         = excluded.clone_url,
			default_branch    = excluded.default_branch,
			profiled_at       = excluded.profiled_at,
			updated_at        = datetime('now')
		RETURNING `+repoProfileFullColumns,
		uuid.New().String(), p.Owner, p.Repo, source, p.Host,
		nullIfEmpty(p.ExternalID),
		nullIfEmpty(p.Description),
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		nullIfEmpty(p.ProfileText),
		nullIfEmpty(p.CloneURL),
		nullIfEmpty(p.DefaultBranch),
		p.ProfiledAt,
		nullIfEmpty(p.OwnerExternalID),
	)
	return scanRepositoryFull(row)
}

// FindOrCreateByRefSystem is the registry door: the get-or-create below, then
// the row it resolved, read in the same transaction.
func (s *repoStore) FindOrCreateByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef) (domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Repository{}, err
	}
	var out domain.Repository
	err := inTx(ctx, s.q, func(tx queryer) error {
		id, err := getOrCreateRepositoryID(ctx, tx, ref)
		if err != nil {
			return err
		}
		p, err := scanRepositoryFull(tx.QueryRowContext(ctx, `
			SELECT `+repoProfileFullColumns+`
			FROM repositories WHERE id = ?
		`, id))
		if err != nil {
			return err
		}
		out = p
		return nil
	})
	return out, err
}

// FindOrCreateByExternalID registers the repository the provider identifies as
// ref.ExternalID; see the interface for the three outcomes. Each pass of
// registerByExternalID either settles on a row or reports that a concurrent
// writer moved first, and the next pass reads what that writer left.
func (s *repoStore) FindOrCreateByExternalID(ctx context.Context, orgID string, ref domain.RepoRef) (domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Repository{}, err
	}
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return domain.Repository{}, err
	}
	if err := db.RequireRepoHost(ref.Host); err != nil {
		return domain.Repository{}, err
	}
	if ref.ExternalID == "" {
		return domain.Repository{}, errors.New("register repository: a provider id is required")
	}
	var out domain.Repository
	err = inTx(ctx, s.q, func(tx queryer) error {
		for range registerAttempts {
			p, done, err := registerByExternalID(ctx, tx, source, ref)
			if err != nil {
				return err
			}
			if done {
				out = p
				return nil
			}
		}
		return fmt.Errorf("register repository %s: concurrent writers kept moving it", ref.Slug())
	})
	return out, err
}

// registerAttempts bounds the passes FindOrCreateByExternalID makes. A pass
// that does not settle lost to one concurrent writer, whose row the next pass
// finds, so two would do; the third is margin, not a retry policy.
const registerAttempts = 3

// registerByExternalID is one pass of the registration: the row carrying the
// id, else the id written onto a row holding the name without one, else a new
// row. done is false when a write found its precondition gone (another writer
// filled the id or inserted the row first).
func registerByExternalID(ctx context.Context, q queryer, source string, ref domain.RepoRef) (domain.Repository, bool, error) {
	p, err := scanRepositoryFull(q.QueryRowContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repositories
		WHERE source = ? AND host = ? AND external_id = ?
	`, source, ref.Host, ref.ExternalID))
	if err == nil {
		return p, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Repository{}, false, err
	}

	named, err := findRepositoryByRef(ctx, q, source, ref)
	if err != nil {
		return domain.Repository{}, false, err
	}
	if named != nil {
		if named.ExternalID != "" {
			return domain.Repository{}, false, fmt.Errorf("%w: %s is registered under provider id %s, not %s",
				db.ErrRepoSlugOccupied, named.Slug(), named.ExternalID, ref.ExternalID)
		}
		p, err := scanRepositoryFull(q.QueryRowContext(ctx, `
			UPDATE repositories
			   SET external_id = ?, owner_external_id = COALESCE(?, owner_external_id),
			       updated_at = datetime('now')
			 WHERE id = ? AND external_id IS NULL
			RETURNING `+repoProfileFullColumns,
			ref.ExternalID, nullIfEmpty(ref.OwnerExternalID), named.ID))
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Repository{}, false, nil
		}
		return p, err == nil, err
	}

	// DO NOTHING with no conflict target, as in insertRepositoryRow: either
	// identity index refusing means a concurrent writer registered the
	// repository first, and the next pass finds its row.
	p, err = scanRepositoryFull(q.QueryRowContext(ctx, `
		INSERT INTO repositories (id, owner, repo, source, host, external_id, owner_external_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT DO NOTHING
		RETURNING `+repoProfileFullColumns,
		uuid.New().String(), ref.Owner, ref.Repo, source, ref.Host, ref.ExternalID, nullIfEmpty(ref.OwnerExternalID)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Repository{}, false, nil
	}
	if err != nil {
		return domain.Repository{}, false, fmt.Errorf("insert repositories[%s]: %w", ref.Slug(), err)
	}
	return p, true, nil
}

// getOrCreateRepositoryID returns the id of one repository's registry row,
// minting the row if it does not exist yet. It is the registry's by-name
// get-or-create, and it has two callers: the GitHub entity create in
// entities.go (a GitHub entity references the row) and the door,
// FindOrCreateByRefSystem. It takes a queryer so it composes inside the
// caller's transaction, and a row it mints commits or rolls back with that
// transaction. The transaction can commit without referencing the row: an
// entity create that loses a concurrent insert of the same key returns the
// winner's entity instead. That leaves nothing stray, because the identity
// index allows a repository one row and the winner's entity references it.
//
// An existing row is left exactly as it stands — same id, same profile text,
// same base branch, same clone and poll state. The lookup before the insert is
// not what prevents a duplicate (the identity index is); it is what returns
// the existing row without writing to it. The re-read after the insert is what
// returns the winner's row when a concurrent creator got there first, since
// the INSERT is a DO NOTHING and reports nothing when it loses.
func getOrCreateRepositoryID(ctx context.Context, q queryer, ref domain.RepoRef) (string, error) {
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return "", err
	}
	if err := db.RequireRepoHost(ref.Host); err != nil {
		return "", err
	}
	id, err := findRepositoryID(ctx, q, source, ref.Host, ref.Owner, ref.Repo)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	if err := insertRepositoryRow(ctx, q, ref); err != nil {
		return "", err
	}
	id, err = findRepositoryID(ctx, q, source, ref.Host, ref.Owner, ref.Repo)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("repositories row for %s vanished immediately after create", ref.Slug())
	}
	return id, nil
}

// findRepositoryID resolves a slug to the registry row's surrogate id, or "" if
// no row holds that slug. Case-INSENSITIVE, like every other slug lookup here,
// and a retired row holds no slug.
func findRepositoryID(ctx context.Context, q queryer, source, host, owner, repo string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT id FROM repositories
		 WHERE source = ? AND host = ? AND LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
		   AND retired_at IS NULL
	`, source, host, owner, repo).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// findRepositoryByRef looks a repository up by provider identity, matching
// owner/repo case-INSENSITIVELY (GitHub identifiers are). Returns nil when no
// row holds the name; a retired row holds none.
func findRepositoryByRef(ctx context.Context, q queryer, source string, ref domain.RepoRef) (*domain.Repository, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repositories
		WHERE source = ? AND host = ? AND LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
		  AND retired_at IS NULL
	`, source, ref.Host, ref.Owner, ref.Repo)
	p, err := scanRepositoryFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// insertRepositoryRow is the INSERT that brings a repository into
// repositories by name, so a row cannot exist without the identity columns the
// create sets. Its callers hold no provider id, so none is written: the
// poller and the profiler record external_id once they have read it off a
// provider payload. A registration by provider id inserts through
// registerByExternalID instead, with the id.
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
	if err := db.RequireRepoHost(ref.Host); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO repositories (id, owner, repo, source, host, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT DO NOTHING
	`, uuid.New().String(), ref.Owner, ref.Repo, source, ref.Host); err != nil {
		return fmt.Errorf("insert repositories[%s]: %w", ref.Slug(), err)
	}
	return nil
}

func (s *repoStore) List(ctx context.Context, orgID, host string, opts db.ListOpts) ([]domain.Repository, int, error) {
	return s.listRepositories(ctx, orgID, host, opts, "")
}

// repoTrackedByATeam scopes a repositories row to the rows at least one team
// tracks. Without RLS every team counts, so it is the Postgres predicate's
// admin-pool reading.
const repoTrackedByATeam = `EXISTS (
	SELECT 1 FROM team_github_repos g
	 WHERE g.repository_id = repositories.id
)`

// repoTrackedByTeam scopes a repositories row to the rows one team tracks; its
// one placeholder is the team id.
const repoTrackedByTeam = `EXISTS (
	SELECT 1 FROM team_github_repos g
	 WHERE g.team_id = ?
	   AND g.repository_id = repositories.id
)`

// listRepositories is the one paged read of the registry on host, retired rows
// left out. filter narrows it — empty for the whole registry, else a predicate
// on the repositories row — and filterArgs binds its placeholders. The count
// runs under the same filter as the page.
func (s *repoStore) listRepositories(ctx context.Context, orgID, host string, opts db.ListOpts, filter string, filterArgs ...any) ([]domain.Repository, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	if err := db.RequireRepoHost(host); err != nil {
		return nil, 0, err
	}
	where := `host = ? AND retired_at IS NULL`
	if filter != "" {
		where += `
		  AND ` + filter
	}
	args := append([]any{host}, filterArgs...)

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM repositories WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.Repository{}, total, nil
	}
	query := `
		SELECT ` + repoProfileFullColumns + `
		FROM repositories
		WHERE ` + where + `
		ORDER BY owner, repo, id`
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

// ListTeamScoped is the tracked set in local mode — N=1, so there is no other
// team to scope away (matches the ListActiveJiraTeamScoped /
// FactoryReadStore.Entities local-mode asymmetry, TFAC-559).
func (s *repoStore) ListTeamScoped(ctx context.Context, orgID, host string, opts db.ListOpts) ([]domain.Repository, int, error) {
	return s.ListTrackedSystem(ctx, orgID, host, opts)
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

func (s *repoStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Repository, error) {
	return s.Get(ctx, orgID, id)
}

func (s *repoStore) GetByRef(ctx context.Context, orgID string, ref domain.RepoRef) (*domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return nil, err
	}
	if err := db.RequireRepoHost(ref.Host); err != nil {
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

// scanUpdatedRepository reads the row an id-keyed UPDATE … RETURNING produced,
// turning the empty result — the write matched nothing — into
// db.ErrNoSuchRepository. RETURNING answers "did this land" and "what does the
// row say now" in one statement, so there is nothing left to count.
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

func (s *repoStore) ListSystem(ctx context.Context, orgID, host string) ([]domain.Repository, error) {
	// Unwindowed: system callers resolve the whole registry.
	rows, _, err := s.List(ctx, orgID, host, db.Unwindowed)
	return rows, err
}

func (s *repoStore) ListTrackedSystem(ctx context.Context, orgID, host string, opts db.ListOpts) ([]domain.Repository, int, error) {
	return s.listRepositories(ctx, orgID, host, opts, repoTrackedByATeam)
}

func (s *repoStore) ListTrackedByTeamSystem(ctx context.Context, orgID, teamID, host string, opts db.ListOpts) ([]domain.Repository, int, error) {
	return s.listRepositories(ctx, orgID, host, opts, repoTrackedByTeam, teamID)
}

func (s *repoStore) ListTrackedByTeam(ctx context.Context, orgID, teamID, host string, opts db.ListOpts) ([]domain.Repository, int, error) {
	return s.ListTrackedByTeamSystem(ctx, orgID, teamID, host, opts)
}

func (s *repoStore) ListTrackedNamesSystem(ctx context.Context, orgID, host string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT r.owner || '/' || r.repo
		  FROM repositories r
		  JOIN team_github_repos g ON g.repository_id = r.id
		 WHERE r.host = ? AND r.retired_at IS NULL
		 ORDER BY r.owner, r.repo
	`, host)
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

func (s *repoStore) UpdateCloneStatusSystem(ctx context.Context, orgID, id, status, errMsg, errKind string) (domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Repository{}, err
	}
	row := s.q.QueryRowContext(ctx, `
		UPDATE repositories
		   SET clone_status = ?, clone_error = ?, clone_error_kind = ?, updated_at = datetime('now')
		 WHERE id = ?
		RETURNING `+repoProfileFullColumns,
		status, nullIfEmpty(errMsg), nullIfEmpty(errKind), id)
	return scanUpdatedRepository(row, id)
}

func (s *repoStore) GetByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef) (*domain.Repository, error) {
	return s.GetByRef(ctx, orgID, ref)
}

func (s *repoStore) UpsertSystem(ctx context.Context, orgID string, p domain.Repository) (domain.Repository, error) {
	return s.Upsert(ctx, orgID, p)
}

func (s *repoStore) UpdateProfileSystem(ctx context.Context, orgID, id string, p domain.Repository) (domain.Repository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Repository{}, err
	}
	// external_id follows Upsert's rule: written when the caller has one and
	// no other row on the row's host carries it, else left as stored. The
	// scalar subquery answers NULL in either case, and COALESCE keeps the
	// stored value. owner_external_id takes p's when the row carries
	// p.ExternalID once the write lands (that same COALESCE, compared with
	// it) and keeps the stored one otherwise. A retired row is not written
	// (see the interface doc), and owner/repo, base_branch, the clone state
	// and the poll cursor are absent from the SET list.
	externalID := `COALESCE((SELECT ?2 WHERE NOT EXISTS (
		                            SELECT 1 FROM repositories o
		                             WHERE o.source = repositories.source AND o.host = repositories.host
		                               AND o.external_id = ?2 AND o.id <> repositories.id)),
		                            external_id)`
	row := s.q.QueryRowContext(ctx, `
		UPDATE repositories
		   SET external_id       = `+externalID+`,
		       owner_external_id = CASE WHEN ?11 IS NOT NULL AND `+externalID+` = ?2
		                                THEN ?11 ELSE owner_external_id END,
		       description       = ?3,
		       has_readme        = ?4,
		       has_claude_md     = ?5,
		       has_agents_md     = ?6,
		       profile_text      = ?7,
		       clone_url         = ?8,
		       default_branch    = ?9,
		       profiled_at       = ?10,
		       updated_at        = datetime('now')
		 WHERE id = ?1 AND retired_at IS NULL
		RETURNING `+repoProfileFullColumns,
		id,
		nullIfEmpty(p.ExternalID),
		nullIfEmpty(p.Description),
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		nullIfEmpty(p.ProfileText),
		nullIfEmpty(p.CloneURL),
		nullIfEmpty(p.DefaultBranch),
		p.ProfiledAt,
		nullIfEmpty(p.OwnerExternalID),
	)
	return scanUpdatedRepository(row, id)
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
			if err := db.RequireRepoHost(ref.Host); err != nil {
				return err
			}
			// One row at most takes a write: the row carrying the id, which
			// gets the owner id when it has none, whatever it is called; else
			// the row holding the name without an id, which gets both unless
			// another row on the host already carries the id, because that row
			// is the repository GitHub calls by it.
			res, err := tx.ExecContext(ctx, `
				UPDATE repositories
				   SET external_id = COALESCE(external_id, ?1),
				       owner_external_id = COALESCE(owner_external_id, ?6),
				       updated_at = datetime('now')
				 WHERE source = ?2 AND host = ?3 AND retired_at IS NULL
				   AND ((external_id = ?1 AND owner_external_id IS NULL AND ?6 IS NOT NULL)
				     OR (external_id IS NULL
				         AND LOWER(owner) = LOWER(?4) AND LOWER(repo) = LOWER(?5)
				         AND NOT EXISTS (
				             SELECT 1 FROM repositories o
				              WHERE o.source = ?2 AND o.host = ?3 AND o.external_id = ?1)))
			`, ref.ExternalID, source, ref.Host, ref.Owner, ref.Repo, nullIfEmpty(ref.OwnerExternalID))
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

func (s *repoStore) GetPullsPollStateSystem(ctx context.Context, orgID, id string) (string, *time.Time, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", nil, err
	}
	var etag sql.NullString
	var polledAt sql.NullTime
	err := s.q.QueryRowContext(ctx, `
		SELECT pulls_etag, pulls_polled_at
		  FROM repositories
		 WHERE id = ?
	`, id).Scan(&etag, &polledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, fmt.Errorf("%w: %s", db.ErrNoSuchRepository, id)
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

func (s *repoStore) SetPullsPollStateSystem(ctx context.Context, orgID, id, etag string, polledAt time.Time) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE repositories
		   SET pulls_etag = ?, pulls_polled_at = ?
		 WHERE id = ?
	`, nullIfEmpty(etag), polledAt.UTC(), id)
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
const repoProfileFullColumns = `id, owner, repo, source, host, external_id, description, has_readme, has_claude_md, has_agents_md, profile_text, clone_url, default_branch, base_branch, profiled_at, clone_status, clone_error, clone_error_kind, retired_at, owner_external_id`

// scanRepositoryFull reads the repositories row shape shared by Get, List,
// and the ref lookup.
func scanRepositoryFull(row rowScanner) (domain.Repository, error) {
	var p domain.Repository
	var externalID, description, profileText, cloneURL, defaultBranch, baseBranch, cloneError, cloneErrorKind, ownerExternalID sql.NullString
	var profiledAt, retiredAt sql.NullTime
	if err := row.Scan(&p.ID, &p.Owner, &p.Repo, &p.Source, &p.Host, &externalID, &description, &p.HasReadme, &p.HasClaudeMd, &p.HasAgentsMd, &profileText, &cloneURL, &defaultBranch, &baseBranch, &profiledAt, &p.CloneStatus, &cloneError, &cloneErrorKind, &retiredAt, &ownerExternalID); err != nil {
		return p, err
	}
	p.ExternalID = externalID.String
	p.OwnerExternalID = ownerExternalID.String
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
	if retiredAt.Valid {
		p.RetiredAt = &retiredAt.Time
	}
	return p, nil
}
