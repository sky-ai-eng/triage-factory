package postgres

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

// repoStore is the Postgres impl of db.RepositoryStore. Wired against the
// app pool in postgres.New: every consumer is request-equivalent
// (repos handler, settings handler, projects handler) or
// runs in a startup/profiler goroutine that already operates within
// the org's identity scope. RLS policy repositories_all gates every
// statement on (org_id = current_org_id() AND user_has_org_access);
// org_id is also included in every WHERE/INSERT clause as defense
// in depth.
//
// # Row id vs "owner/repo"
//
// The schema gives repositories a uuid PK plus a folded
// UNIQUE(org_id, source, owner, repo) natural key. The uuid is the handle:
// it is what domain.Repository.ID carries, what every referencing table
// stores, and what the id-keyed methods query — with a miss refused as
// db.ErrNoSuchRepository. The ref-keyed (…ByRef) methods query the folded
// natural key and answer a miss with nil.
//
// The uuid is validated in Go before it reaches a statement, so a caller that
// hands over something that was never an id gets the same ErrNoSuchRepository
// SQLite gives it rather than a dialect-specific uuid parse failure.
//
// # Pool split
//
// Holds two pools: q is the app pool (request-equivalent consumers —
// repos handler, settings, projects) and admin is the
// admin pool (system services — poller bootstrap reading every
// configured repo at startup, clone-status writes from the startup
// clone path before any JWT-claims context can exist). The
// `...System` methods route through admin; everything else stays on
// q. org_id filtering is in every WHERE clause as defense in depth
// on both pools.
type repoStore struct {
	q     queryer
	admin queryer
}

func newRepositoryStore(q, admin queryer) db.RepositoryStore {
	return &repoStore{q: q, admin: admin}
}

var _ db.RepositoryStore = (*repoStore)(nil)

func (s *repoStore) Upsert(ctx context.Context, orgID string, p domain.Repository) (domain.Repository, error) {
	return upsertRepository(ctx, s.q, orgID, p)
}

func (s *repoStore) UpsertSystem(ctx context.Context, orgID string, p domain.Repository) (domain.Repository, error) {
	return upsertRepository(ctx, s.admin, orgID, p)
}

func upsertRepository(ctx context.Context, q queryer, orgID string, p domain.Repository) (domain.Repository, error) {
	source, err := domain.NormalizeRepoSource(p.Source)
	if err != nil {
		return domain.Repository{}, err
	}
	// On conflict refresh profiling metadata only — base_branch and
	// clone-status fields are user/clone-hook owned and shouldn't be
	// clobbered by a re-profile. Matches the SQLite impl's exclude
	// list verbatim, including external_id refreshing only when the writer
	// actually has an id (an empty one never clears a stored one) and source
	// staying put as the create-time identity column it is — here it is part
	// of the conflict target, so a conflicting row already agrees on it.
	//
	// The conflict target is the folded identity index, so an upsert spelling
	// the repository differently than the stored row updates that row instead
	// of creating a second one. Upsert creates as well as updates, which makes
	// it a create path too; inferring the case-sensitive columns here would
	// leave it as the one create path that could still duplicate. owner/repo
	// are absent from the SET list, so the stored casing stays sticky.
	//
	// RETURNING projects the point read's column list, so the row handed back
	// carries the COALESCEd external_id, the stored casing and the base branch
	// and clone state this statement left alone — none of which p can describe.
	// The update arm resolves it through the repositories_all policy, which is
	// org-scoped for SELECT and UPDATE alike; a policy that narrowed one and not
	// the other would surface as the conformance suite's returned-row assertion
	// failing rather than as a silent nil.
	row := q.QueryRowContext(ctx, `
		INSERT INTO repositories
		  (org_id, source, owner, repo, external_id, description, has_readme, has_claude_md, has_agents_md,
		   profile_text, clone_url, default_branch, profiled_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8, $9,
		        NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''), $13)
		ON CONFLICT (org_id, source, lower(owner), lower(repo)) DO UPDATE SET
		  external_id    = COALESCE(EXCLUDED.external_id, repositories.external_id),
		  description    = EXCLUDED.description,
		  has_readme     = EXCLUDED.has_readme,
		  has_claude_md  = EXCLUDED.has_claude_md,
		  has_agents_md  = EXCLUDED.has_agents_md,
		  profile_text   = EXCLUDED.profile_text,
		  clone_url      = EXCLUDED.clone_url,
		  default_branch = EXCLUDED.default_branch,
		  profiled_at    = EXCLUDED.profiled_at,
		  updated_at     = now()
		RETURNING `+repoProfileFullColumns,
		orgID, source, p.Owner, p.Repo,
		p.ExternalID,
		p.Description,
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		p.ProfileText, p.CloneURL, p.DefaultBranch,
		p.ProfiledAt,
	)
	return pgScanRepositoryFull(row)
}

// getOrCreateRepositoryID returns the surrogate id of one repository's registry
// row, minting the row if it does not exist yet. It is the resolver the
// tracked-set reconcile in team_github_repos.go uses: a save holding a slug
// needs the id the tracking row's FK points at, and the store contract keeps
// the surrogate out of domain.Repository. It takes a queryer so it composes
// inside the reconcile's transaction — lookup, create and re-read then answer
// for one point in time rather than three.
//
// An existing row is left exactly as it stands — same id, same profile text,
// same base branch, same clone and poll state. The lookup before the insert is
// not what prevents a duplicate (the identity index is); it is what returns
// the existing row without writing to it. The re-read after the insert is what
// returns the winner's row when a concurrent creator got there first, since
// the INSERT is guarded and DO NOTHING, and reports nothing when it loses.
func getOrCreateRepositoryID(ctx context.Context, q queryer, orgID string, ref domain.RepoRef) (string, error) {
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return "", err
	}
	id, err := findRepositoryID(ctx, q, orgID, source, ref.Owner, ref.Repo)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	if err := insertRepositoryRow(ctx, q, orgID, ref); err != nil {
		return "", err
	}
	id, err = findRepositoryID(ctx, q, orgID, source, ref.Owner, ref.Repo)
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
func findRepositoryID(ctx context.Context, q queryer, orgID, source, owner, repo string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT id FROM repositories
		 WHERE org_id = $1 AND source = $2 AND lower(owner) = lower($3) AND lower(repo) = lower($4)
	`, orgID, source, owner, repo).Scan(&id)
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
func findRepositoryByRef(ctx context.Context, q queryer, orgID, source string, ref domain.RepoRef) (*domain.Repository, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repositories
		WHERE org_id = $1 AND source = $2 AND lower(owner) = lower($3) AND lower(repo) = lower($4)
	`, orgID, source, ref.Owner, ref.Repo)
	p, err := pgScanRepositoryFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// insertRepositoryRow is the single INSERT that brings a repository into
// repositories. Tracking is the only path that creates one, and it creates
// through here, so a row cannot exist without the identity columns the create
// sets. Tracking learns no provider id, so none is written: the poller and
// the profiler record external_id once they have read it off a provider
// payload.
//
// "Already exists" means the repositories_identity index: (org_id, source)
// plus the case-FOLDED slug, because GitHub identifiers are case-insensitive
// and one repository is one row however it is spelled. Enforcing that in the
// index rather than in a guard is what makes concurrent creates safe without a
// lock — a second writer conflicts on the index and blocks on the first while
// it is in flight, then DO NOTHING turns the resolved conflict into the no-op
// the create wants. A guarded INSERT over a case-sensitive key could not do
// that: neither writer sees the other's uncommitted row, and their keys would
// differ, so both would land.
//
// DO NOTHING carries no conflict target on purpose. Every uniqueness
// constraint this row can violate — the identity index, and the slug primary
// key on the SQLite side — means the same thing, that the repository is
// already here, and a targeted clause would raise on the one it did not name.
func insertRepositoryRow(ctx context.Context, q queryer, orgID string, ref domain.RepoRef) error {
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO repositories (org_id, source, owner, repo)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING
	`, orgID, source, ref.Owner, ref.Repo); err != nil {
		return fmt.Errorf("insert repositories[%s]: %w", ref.Slug(), err)
	}
	return nil
}

func (s *repoStore) List(ctx context.Context, orgID string, opts db.ListOpts) ([]domain.Repository, int, error) {
	return listRepositories(ctx, s.q, orgID, opts)
}

func (s *repoStore) ListSystem(ctx context.Context, orgID string) ([]domain.Repository, error) {
	// System callers resolve the whole registry (a slug lookup, a profiling
	// pass), so they take the unwindowed read and discard the count.
	rows, _, err := listRepositories(ctx, s.admin, orgID, db.Unwindowed)
	return rows, err
}

func listRepositories(ctx context.Context, q queryer, orgID string, opts db.ListOpts) ([]domain.Repository, int, error) {
	var total int
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM repositories WHERE org_id = $1
	`, orgID).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.Repository{}, total, nil
	}
	query := `
		SELECT ` + repoProfileFullColumns + `
		FROM repositories
		WHERE org_id = $1
		ORDER BY owner, repo, id`
	args := []any{orgID}
	if opts.Limit > 0 {
		query += `
		LIMIT $2 OFFSET $3`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []domain.Repository{}
	for rows.Next() {
		p, err := pgScanRepositoryFull(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

// repoProfileTrackedByViewerTeams scopes a repositories row (alias rp) to
// the tracked repos of the viewer's teams — the RLS-does-the-scoping
// semi-join pattern factory.go's factoryGitHubRepoTrackedExists and
// entities.go's jiraTeamProjectMembershipExists use for the same class of
// org-wide-entity-list leak (TFAC-559). Under the app pool (tf_app, RLS
// active) team_github_repos_select already constrains visible rows to the
// caller's memberships, so the EXISTS auto-scopes with no explicit team_id.
// The teams join binds org (rp.org_id) as defense-in-depth for the admin
// pool, where team_github_repos carries no org_id column of its own.
//
// The match is on the row id rather than a folded slug comparison, so it is an
// index lookup on team_github_repos_repository_idx and cannot disagree with
// the registry about what "the same repository" means.
const repoProfileTrackedByViewerTeams = `EXISTS (
	SELECT 1 FROM team_github_repos g
	JOIN teams tm ON tm.id = g.team_id
	WHERE tm.org_id = rp.org_id
	  AND g.repository_id = rp.id
)`

func (s *repoStore) ListTeamScoped(ctx context.Context, orgID string, opts db.ListOpts) ([]domain.Repository, int, error) {
	var total int
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM repositories rp
		WHERE rp.org_id = $1
		  AND `+repoProfileTrackedByViewerTeams+`
	`, orgID).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.Repository{}, total, nil
	}
	query := `
		SELECT ` + repoProfileFullColumnsAliased + `
		FROM repositories rp
		WHERE rp.org_id = $1
		  AND ` + repoProfileTrackedByViewerTeams + `
		ORDER BY rp.owner, rp.repo, rp.id`
	args := []any{orgID}
	if opts.Limit > 0 {
		query += `
		LIMIT $2 OFFSET $3`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []domain.Repository{}
	for rows.Next() {
		p, err := pgScanRepositoryFull(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

func (s *repoStore) ListTrackedNamesSystem(ctx context.Context, orgID string) ([]string, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT DISTINCT rp.owner || '/' || rp.repo
		  FROM repositories rp
		  JOIN team_github_repos g ON g.repository_id = rp.id
		  JOIN teams tm ON tm.id = g.team_id AND tm.org_id = rp.org_id
		 WHERE rp.org_id = $1
		 ORDER BY 1
	`, orgID)
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
	var count int
	err := s.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM repositories WHERE org_id = $1`, orgID,
	).Scan(&count)
	return count, err
}

func (s *repoStore) UpdateBaseBranch(ctx context.Context, orgID, id, baseBranch string) (domain.Repository, error) {
	if err := validRepoID(id); err != nil {
		return domain.Repository{}, err
	}
	row := s.q.QueryRowContext(ctx, `
		UPDATE repositories
		   SET base_branch = NULLIF($1, '')
		 WHERE org_id = $2 AND id = $3
		RETURNING `+repoProfileFullColumns,
		baseBranch, orgID, id)
	return scanUpdatedRepository(row, id)
}

func (s *repoStore) Get(ctx context.Context, orgID, id string) (*domain.Repository, error) {
	if err := validRepoID(id); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repositories
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	p, err := pgScanRepositoryFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", db.ErrNoSuchRepository, id)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *repoStore) GetByRef(ctx context.Context, orgID string, ref domain.RepoRef) (*domain.Repository, error) {
	return getRepositoryByRef(ctx, s.q, orgID, ref)
}

func (s *repoStore) GetByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef) (*domain.Repository, error) {
	return getRepositoryByRef(ctx, s.admin, orgID, ref)
}

// getRepositoryByRef is the name-keyed read. Case-insensitive, like every
// other ref lookup here. The callers that matter are the ones holding a name
// they did NOT read out of this table: `tfac exec workspace add owner/repo`
// hands the agent's argv straight through, and a miss there is reported as
// "repo is not configured in Triage Factory" — while the team-tracking gate on
// the next line of that same function matches case-insensitively and would
// have said yes.
func getRepositoryByRef(ctx context.Context, q queryer, orgID string, ref domain.RepoRef) (*domain.Repository, error) {
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return nil, err
	}
	return findRepositoryByRef(ctx, q, orgID, source, ref)
}

func (s *repoStore) UpdateCloneStatusByRef(ctx context.Context, orgID string, ref domain.RepoRef, status, errMsg, errKind string) (*domain.Repository, error) {
	return updateRepoCloneStatus(ctx, s.q, orgID, ref, status, errMsg, errKind)
}

func (s *repoStore) UpdateCloneStatusByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef, status, errMsg, errKind string) (*domain.Repository, error) {
	return updateRepoCloneStatus(ctx, s.admin, orgID, ref, status, errMsg, errKind)
}

func updateRepoCloneStatus(ctx context.Context, q queryer, orgID string, ref domain.RepoRef, status, errMsg, errKind string) (*domain.Repository, error) {
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return nil, err
	}
	row := q.QueryRowContext(ctx, `
		UPDATE repositories
		   SET clone_status = $1, clone_error = NULLIF($2, ''), clone_error_kind = NULLIF($3, '')
		 WHERE org_id = $4 AND source = $5
		   AND lower(owner) = lower($6) AND lower(repo) = lower($7)
		RETURNING `+repoProfileFullColumns,
		status, errMsg, errKind, orgID, source, ref.Owner, ref.Repo)
	p, err := pgScanRepositoryFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing answers to the name — the documented no-op, not a fault.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// validRepoID rejects a handle that was never a registry id before it reaches
// a statement. Postgres types the column as uuid, so a leftover "owner/repo"
// would fail the parse in the driver with a message about syntax rather than
// about the repository — and SQLite, whose column is TEXT, would answer the
// same call with a clean miss. Deciding it here keeps the two dialects
// agreeing on the one answer that is true either way.
func validRepoID(id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("%w: %s", db.ErrNoSuchRepository, id)
	}
	return nil
}

// scanUpdatedRepository reads the row an id-keyed UPDATE … RETURNING produced,
// turning the empty result — the write matched nothing — into
// db.ErrNoSuchRepository. It replaces the rows-affected probe these writes used
// to run: RETURNING answers "did this land" and "what does the row say now" in
// one statement, so there is nothing left to count.
func scanUpdatedRepository(row pgRowScanner, id string) (domain.Repository, error) {
	p, err := pgScanRepositoryFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Repository{}, fmt.Errorf("%w: %s", db.ErrNoSuchRepository, id)
	}
	if err != nil {
		return domain.Repository{}, err
	}
	return p, nil
}

func (s *repoStore) FillMissingExternalIDsSystem(ctx context.Context, orgID string, refs []domain.RepoRef) (int, error) {
	sources := make([]string, 0, len(refs))
	owners := make([]string, 0, len(refs))
	names := make([]string, 0, len(refs))
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.ExternalID == "" {
			continue // absent is not an id
		}
		source, err := domain.NormalizeRepoSource(ref.Source)
		if err != nil {
			return 0, err
		}
		sources = append(sources, source)
		owners = append(owners, ref.Owner)
		names = append(names, ref.Repo)
		ids = append(ids, ref.ExternalID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	// One statement for the whole batch — this runs per installation per poll
	// cycle, so the round-trip is the cost worth avoiding. external_id IS NULL
	// makes it a no-op in the steady state (every id already recorded), which
	// is what it will be on all but the first cycle after an upgrade.
	rows, err := s.admin.QueryContext(ctx, `
		UPDATE repositories rp
		   SET external_id = v.external_id
		  FROM unnest($2::text[], $3::text[], $4::text[], $5::text[])
		       AS v(source, owner, repo, external_id)
		 WHERE rp.org_id = $1
		   AND rp.source = v.source
		   AND lower(rp.owner) = lower(v.owner)
		   AND lower(rp.repo) = lower(v.repo)
		   AND rp.external_id IS NULL
		RETURNING 1
	`, orgID, sources, owners, names, ids)
	if err != nil {
		return 0, fmt.Errorf("fill external ids: %w", err)
	}
	defer rows.Close()
	filled := 0
	for rows.Next() {
		filled++
	}
	return filled, rows.Err()
}

func (s *repoStore) GetPullsPollStateByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef) (string, *time.Time, error) {
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return "", nil, err
	}
	var etag sql.NullString
	var polledAt sql.NullTime
	err = s.admin.QueryRowContext(ctx, `
		SELECT pulls_etag, pulls_polled_at
		  FROM repositories
		 WHERE org_id = $1 AND source = $2
		   AND lower(owner) = lower($3) AND lower(repo) = lower($4)
	`, orgID, source, ref.Owner, ref.Repo).Scan(&etag, &polledAt)
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
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return err
	}
	_, err = s.admin.ExecContext(ctx, `
		UPDATE repositories
		   SET pulls_etag = NULLIF($1, ''), pulls_polled_at = $2
		 WHERE org_id = $3 AND source = $4
		   AND lower(owner) = lower($5) AND lower(repo) = lower($6)
	`, etag, polledAt, orgID, source, ref.Owner, ref.Repo)
	return err
}

// pgRowScanner is the common Scan surface of *sql.Row and *sql.Rows.
// Needed because Get scans a single row from QueryRowContext while
// List scans repeatedly from QueryContext.
type pgRowScanner interface {
	Scan(dest ...any) error
}

// repoProfileFullColumns is the projection pgScanRepositoryFull reads, named
// once so the queries that share it cannot drift out of column order.
// repoProfileFullColumnsAliased is the same list qualified for the `rp` alias
// ListTeamScoped's semi-join needs.
const (
	repoProfileFullColumns = `id, owner, repo, source, external_id, description, has_readme, has_claude_md, has_agents_md,
		       profile_text, clone_url, default_branch, base_branch, profiled_at,
		       clone_status, clone_error, clone_error_kind`

	repoProfileFullColumnsAliased = `rp.id, rp.owner, rp.repo, rp.source, rp.external_id, rp.description, rp.has_readme, rp.has_claude_md, rp.has_agents_md,
		       rp.profile_text, rp.clone_url, rp.default_branch, rp.base_branch, rp.profiled_at,
		       rp.clone_status, rp.clone_error, rp.clone_error_kind`
)

// pgScanRepositoryFull reads the projection shared by Get, List, and the ref
// lookup.
func pgScanRepositoryFull(row pgRowScanner) (domain.Repository, error) {
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

// splitRepoSlug splits "owner/repo" at the first slash. Returns an empty
// repo half if the input has no slash; the callers — the worktree ledger's
// slug resolution and the rename rewrite — treat that as a name nothing
// answers to.
func splitRepoSlug(s string) (owner, repo string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}
