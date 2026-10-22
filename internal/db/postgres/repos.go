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
// UNIQUE(org_id, source, host, owner, repo) natural key. The uuid is the handle:
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
	if err := db.RequireRepoHost(p.Host); err != nil {
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
	// are absent from the SET list, so the stored casing stays sticky. host is
	// part of the target, so a same-named repository on another host is
	// another row. The index is partial over rows that are not retired, and the
	// target names its predicate, so a retired row under the name is never the
	// one updated: the upsert lands on, or creates, the row that holds the name.
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
	// The update arm resolves it through the repositories_all policy, which is
	// org-scoped for SELECT and UPDATE alike; a policy that narrowed one and not
	// the other would surface as the conformance suite's returned-row assertion
	// failing rather than as a silent nil.
	learnedID := `(SELECT NULLIF($5::text, '') WHERE NOT EXISTS (
		            SELECT 1 FROM repositories o
		             WHERE o.org_id = $1 AND o.source = $2 AND o.host = $14 AND o.external_id = $5::text
		               AND NOT (o.retired_at IS NULL AND lower(o.owner) = lower($3) AND lower(o.repo) = lower($4))))`
	row := q.QueryRowContext(ctx, `
		INSERT INTO repositories
		  (org_id, source, host, owner, repo, external_id, owner_external_id, description, has_readme, has_claude_md, has_agents_md,
		   profile_text, clone_url, default_branch, profiled_at)
		VALUES ($1, $2, $14, $3, $4,
		        `+learnedID+`,
		        CASE WHEN `+learnedID+` IS NOT NULL THEN NULLIF($15, '') END,
		        NULLIF($6, ''), $7, $8, $9,
		        NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''), $13)
		ON CONFLICT (org_id, source, host, lower(owner), lower(repo)) WHERE retired_at IS NULL DO UPDATE SET
		  external_id       = COALESCE(EXCLUDED.external_id, repositories.external_id),
		  owner_external_id = COALESCE(EXCLUDED.owner_external_id, repositories.owner_external_id),
		  description       = EXCLUDED.description,
		  has_readme        = EXCLUDED.has_readme,
		  has_claude_md     = EXCLUDED.has_claude_md,
		  has_agents_md     = EXCLUDED.has_agents_md,
		  profile_text      = EXCLUDED.profile_text,
		  clone_url         = EXCLUDED.clone_url,
		  default_branch    = EXCLUDED.default_branch,
		  profiled_at       = EXCLUDED.profiled_at,
		  updated_at        = now()
		RETURNING `+repoProfileFullColumns,
		orgID, source, p.Owner, p.Repo,
		p.ExternalID,
		p.Description,
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		p.ProfileText, p.CloneURL, p.DefaultBranch,
		p.ProfiledAt, p.Host, p.OwnerExternalID,
	)
	return pgScanRepositoryFull(row)
}

// FindOrCreateByRefSystem is the registry door on the admin pool: the
// get-or-create below, then the row it resolved, read in the same transaction.
func (s *repoStore) FindOrCreateByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef) (domain.Repository, error) {
	var out domain.Repository
	err := inTx(ctx, s.admin, func(tx queryer) error {
		id, err := getOrCreateRepositoryID(ctx, tx, orgID, ref)
		if err != nil {
			return err
		}
		row, err := getRepository(ctx, tx, orgID, id)
		if err != nil {
			return err
		}
		out = *row
		return nil
	})
	return out, err
}

// FindOrCreateByExternalID registers the repository the provider identifies as
// ref.ExternalID on the app pool, under the caller's claims; see the interface
// for the three outcomes. Each pass of registerByExternalID either settles on a
// row or reports that a concurrent writer moved first (see adoptBareRepository
// for the UPDATE; a DO NOTHING insert that waited on another's uncommitted row
// returns nothing), and the next pass reads what that writer committed.
func (s *repoStore) FindOrCreateByExternalID(ctx context.Context, orgID string, ref domain.RepoRef) (domain.Repository, error) {
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
			p, done, err := registerByExternalID(ctx, tx, orgID, source, ref)
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
func registerByExternalID(ctx context.Context, q queryer, orgID, source string, ref domain.RepoRef) (domain.Repository, bool, error) {
	p, err := pgScanRepositoryFull(q.QueryRowContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repositories
		WHERE org_id = $1 AND source = $2 AND host = $3 AND external_id = $4
	`, orgID, source, ref.Host, ref.ExternalID))
	if err == nil {
		return p, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Repository{}, false, err
	}

	named, err := findRepositoryByRef(ctx, q, orgID, source, ref)
	if err != nil {
		return domain.Repository{}, false, err
	}
	if named != nil {
		if named.ExternalID != "" {
			return domain.Repository{}, false, fmt.Errorf("%w: %s is registered under provider id %s, not %s",
				db.ErrRepoSlugOccupied, named.Slug(), named.ExternalID, ref.ExternalID)
		}
		return adoptBareRepository(ctx, q, orgID, named.ID, ref)
	}

	// DO NOTHING with no conflict target, as in insertRepositoryRow: either
	// identity index refusing means a concurrent writer registered the
	// repository first, and the next pass finds its row.
	p, err = pgScanRepositoryFull(q.QueryRowContext(ctx, `
		INSERT INTO repositories (org_id, source, host, owner, repo, external_id, owner_external_id)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))
		ON CONFLICT DO NOTHING
		RETURNING `+repoProfileFullColumns,
		orgID, source, ref.Host, ref.Owner, ref.Repo, ref.ExternalID, ref.OwnerExternalID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Repository{}, false, nil
	}
	if err != nil {
		return domain.Repository{}, false, fmt.Errorf("insert repositories[%s]: %w", ref.Slug(), err)
	}
	return p, true, nil
}

// adoptBareRepository writes ref's provider id, and its owner account id when
// it carries one, onto a row that holds the name without a provider id. done
// is false when another writer moved first, in either of two ways: it filled
// this row's id (READ COMMITTED re-checks the predicate once the row lock is
// free, and the UPDATE matches nothing), or it committed the id on a different
// row while this UPDATE waited on that row's uncommitted entry in
// repositories_external_identity. The second is a unique violation, and an
// error aborts a Postgres transaction, so the UPDATE runs under a savepoint and
// rolls back to it, leaving the next pass able to read the committed row.
func adoptBareRepository(ctx context.Context, q queryer, orgID, id string, ref domain.RepoRef) (domain.Repository, bool, error) {
	if _, err := q.ExecContext(ctx, `SAVEPOINT register_adopt`); err != nil {
		return domain.Repository{}, false, err
	}
	p, err := pgScanRepositoryFull(q.QueryRowContext(ctx, `
		UPDATE repositories
		   SET external_id = $3, owner_external_id = COALESCE(NULLIF($4, ''), owner_external_id),
		       updated_at = now()
		 WHERE org_id = $1 AND id = $2 AND external_id IS NULL
		RETURNING `+repoProfileFullColumns,
		orgID, id, ref.ExternalID, ref.OwnerExternalID))
	done := err == nil
	switch {
	case isUniqueViolation(err):
		if _, err := q.ExecContext(ctx, `ROLLBACK TO SAVEPOINT register_adopt`); err != nil {
			return domain.Repository{}, false, err
		}
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return domain.Repository{}, false, err
	}
	if _, err := q.ExecContext(ctx, `RELEASE SAVEPOINT register_adopt`); err != nil {
		return domain.Repository{}, false, err
	}
	return p, done, nil
}

// getOrCreateRepositoryID returns the id of one repository's registry row,
// minting the row if it does not exist yet. It is the registry's by-name
// get-or-create, and it has two callers: the GitHub entity create in
// entities.go (a GitHub entity references the row) and the door,
// FindOrCreateByRefSystem. It takes a queryer so it composes inside the
// caller's transaction — lookup, create and re-read then answer for one point
// in time rather than three, and a row it mints commits or rolls back with
// that transaction. The transaction can commit without referencing the row: an
// entity create that loses a concurrent insert of the same key returns the
// winner's entity instead. That leaves nothing stray, because the identity
// index allows a repository one row and the winner's entity references it.
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
	if err := db.RequireRepoHost(ref.Host); err != nil {
		return "", err
	}
	id, err := findRepositoryID(ctx, q, orgID, source, ref.Host, ref.Owner, ref.Repo)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	if err := insertRepositoryRow(ctx, q, orgID, ref); err != nil {
		return "", err
	}
	id, err = findRepositoryID(ctx, q, orgID, source, ref.Host, ref.Owner, ref.Repo)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("repositories row for %s vanished immediately after create", ref.Slug())
	}
	return id, nil
}

// findRepositoryID resolves a slug on host to the registry row's surrogate id,
// or "" if no row holds that slug there. Case-INSENSITIVE, like every other
// slug lookup here, and a retired row holds no slug.
func findRepositoryID(ctx context.Context, q queryer, orgID, source, host, owner, repo string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT id FROM repositories
		 WHERE org_id = $1 AND source = $2 AND host = $3
		   AND lower(owner) = lower($4) AND lower(repo) = lower($5)
		   AND retired_at IS NULL
	`, orgID, source, host, owner, repo).Scan(&id)
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
func findRepositoryByRef(ctx context.Context, q queryer, orgID, source string, ref domain.RepoRef) (*domain.Repository, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repositories
		WHERE org_id = $1 AND source = $2 AND host = $3
		  AND lower(owner) = lower($4) AND lower(repo) = lower($5)
		  AND retired_at IS NULL
	`, orgID, source, ref.Host, ref.Owner, ref.Repo)
	p, err := pgScanRepositoryFull(row)
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
// "Already exists" means the repositories_identity index: (org_id, source,
// host) plus the case-FOLDED slug, because GitHub identifiers are case-insensitive
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
	if err := db.RequireRepoHost(ref.Host); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO repositories (org_id, source, host, owner, repo)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING
	`, orgID, source, ref.Host, ref.Owner, ref.Repo); err != nil {
		return fmt.Errorf("insert repositories[%s]: %w", ref.Slug(), err)
	}
	return nil
}

func (s *repoStore) List(ctx context.Context, orgID, host string, opts db.ListOpts) ([]domain.Repository, int, error) {
	return listRepositories(ctx, s.q, orgID, host, opts, "")
}

func (s *repoStore) ListSystem(ctx context.Context, orgID, host string) ([]domain.Repository, error) {
	// System callers resolve the whole registry (a slug lookup, a profiling
	// pass), so they take the unwindowed read and discard the count.
	rows, _, err := listRepositories(ctx, s.admin, orgID, host, db.Unwindowed, "")
	return rows, err
}

// repoTrackedByATeam scopes a repositories row (alias rp) to the rows at least
// one team tracks, and the pool decides which teams count. Under the app pool
// (tf_app, RLS active) team_github_repos_select constrains visible tracking
// rows to the caller's own memberships, so the EXISTS auto-scopes to the
// viewer's teams with no explicit team_id — the RLS-does-the-scoping
// semi-join pattern factory.go's factoryGitHubRepoTrackedExists and
// entities.go's jiraTeamProjectMembershipExists use for the same class of
// org-wide-entity-list leak. Under the admin pool every team in the
// org counts, and the teams join on rp.org_id is what keeps it to that org.
//
// The match is on the row id rather than a folded slug comparison, so it is an
// index lookup on team_github_repos_repository_idx and cannot disagree with
// the registry about what "the same repository" means.
const repoTrackedByATeam = `EXISTS (
	SELECT 1 FROM team_github_repos g
	JOIN teams tm ON tm.id = g.team_id
	WHERE tm.org_id = rp.org_id
	  AND g.repository_id = rp.id
)`

// repoTrackedByTeam scopes a repositories row (alias rp) to the rows one team
// tracks. The team id binds $3, the first placeholder after listRepositories'
// own org and host. Same predicate TracksRepositorySystem answers for one row.
const repoTrackedByTeam = `EXISTS (
	SELECT 1 FROM team_github_repos g
	WHERE g.team_id = $3
	  AND g.repository_id = rp.id
)`

// listRepositories is the one paged read of the registry on host, retired rows
// left out. filter narrows it — empty for the whole registry, else a predicate
// on the row aliased rp whose placeholders start at $3 — and filterArgs binds
// them. The count runs under the same filter on the same connection as the
// page.
func listRepositories(ctx context.Context, q queryer, orgID, host string, opts db.ListOpts, filter string, filterArgs ...any) ([]domain.Repository, int, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return nil, 0, err
	}
	where := `rp.org_id = $1 AND rp.host = $2 AND rp.retired_at IS NULL`
	if filter != "" {
		where += `
		  AND ` + filter
	}
	args := append([]any{orgID, host}, filterArgs...)

	var total int
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM repositories rp WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.Repository{}, total, nil
	}
	query := `
		SELECT ` + repoProfileFullColumnsAliased + `
		FROM repositories rp
		WHERE ` + where + `
		ORDER BY rp.owner, rp.repo, rp.id`
	if opts.Limit > 0 {
		query += fmt.Sprintf(`
		LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
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

func (s *repoStore) ListTeamScoped(ctx context.Context, orgID, host string, opts db.ListOpts) ([]domain.Repository, int, error) {
	return listRepositories(ctx, s.q, orgID, host, opts, repoTrackedByATeam)
}

func (s *repoStore) ListTrackedSystem(ctx context.Context, orgID, host string, opts db.ListOpts) ([]domain.Repository, int, error) {
	return listRepositories(ctx, s.admin, orgID, host, opts, repoTrackedByATeam)
}

func (s *repoStore) ListTrackedByTeamSystem(ctx context.Context, orgID, teamID, host string, opts db.ListOpts) ([]domain.Repository, int, error) {
	return listRepositories(ctx, s.admin, orgID, host, opts, repoTrackedByTeam, teamID)
}

func (s *repoStore) ListTrackedByTeam(ctx context.Context, orgID, teamID, host string, opts db.ListOpts) ([]domain.Repository, int, error) {
	return listRepositories(ctx, s.q, orgID, host, opts, repoTrackedByTeam, teamID)
}

func (s *repoStore) ListTrackedNamesSystem(ctx context.Context, orgID, host string) ([]string, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT DISTINCT rp.owner || '/' || rp.repo
		  FROM repositories rp
		  JOIN team_github_repos g ON g.repository_id = rp.id
		  JOIN teams tm ON tm.id = g.team_id AND tm.org_id = rp.org_id
		 WHERE rp.org_id = $1 AND rp.host = $2 AND rp.retired_at IS NULL
		 ORDER BY 1
	`, orgID, host)
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
	return getRepository(ctx, s.q, orgID, id)
}

func (s *repoStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Repository, error) {
	return getRepository(ctx, s.admin, orgID, id)
}

func getRepository(ctx context.Context, q queryer, orgID, id string) (*domain.Repository, error) {
	if err := validRepoID(id); err != nil {
		return nil, err
	}
	row := q.QueryRowContext(ctx, `
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
	if err := db.RequireRepoHost(ref.Host); err != nil {
		return nil, err
	}
	return findRepositoryByRef(ctx, q, orgID, source, ref)
}

func (s *repoStore) UpdateCloneStatusSystem(ctx context.Context, orgID, id, status, errMsg, errKind string) (domain.Repository, error) {
	if err := validRepoID(id); err != nil {
		return domain.Repository{}, err
	}
	row := s.admin.QueryRowContext(ctx, `
		UPDATE repositories
		   SET clone_status = $1, clone_error = NULLIF($2, ''), clone_error_kind = NULLIF($3, '')
		 WHERE org_id = $4 AND id = $5
		RETURNING `+repoProfileFullColumns,
		status, errMsg, errKind, orgID, id)
	return scanUpdatedRepository(row, id)
}

func (s *repoStore) UpdateProfileSystem(ctx context.Context, orgID, id string, p domain.Repository) (domain.Repository, error) {
	if err := validRepoID(id); err != nil {
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
	externalID := `COALESCE((SELECT NULLIF($3::text, '') WHERE NOT EXISTS (
		                            SELECT 1 FROM repositories o
		                             WHERE o.org_id = r.org_id AND o.source = r.source AND o.host = r.host
		                               AND o.external_id = $3::text AND o.id <> r.id)),
		                            r.external_id)`
	row := s.admin.QueryRowContext(ctx, `
		UPDATE repositories r
		   SET external_id       = `+externalID+`,
		       owner_external_id = CASE WHEN NULLIF($12, '') IS NOT NULL AND `+externalID+` = $3::text
		                                THEN $12 ELSE r.owner_external_id END,
		       description       = NULLIF($4, ''),
		       has_readme        = $5,
		       has_claude_md     = $6,
		       has_agents_md     = $7,
		       profile_text      = NULLIF($8, ''),
		       clone_url         = NULLIF($9, ''),
		       default_branch    = NULLIF($10, ''),
		       profiled_at       = $11,
		       updated_at        = now()
		 WHERE r.org_id = $1 AND r.id = $2 AND r.retired_at IS NULL
		RETURNING `+repoProfileFullColumns,
		orgID, id, p.ExternalID,
		p.Description,
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		p.ProfileText, p.CloneURL, p.DefaultBranch,
		p.ProfiledAt, p.OwnerExternalID,
	)
	return scanUpdatedRepository(row, id)
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
// db.ErrNoSuchRepository. RETURNING answers "did this land" and "what does the
// row say now" in one statement, so there is nothing left to count.
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
	hosts := make([]string, 0, len(refs))
	owners := make([]string, 0, len(refs))
	names := make([]string, 0, len(refs))
	ids := make([]string, 0, len(refs))
	ownerIDs := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.ExternalID == "" {
			continue // absent is not an id
		}
		source, err := domain.NormalizeRepoSource(ref.Source)
		if err != nil {
			return 0, err
		}
		if err := db.RequireRepoHost(ref.Host); err != nil {
			return 0, err
		}
		sources = append(sources, source)
		hosts = append(hosts, ref.Host)
		owners = append(owners, ref.Owner)
		names = append(names, ref.Repo)
		ids = append(ids, ref.ExternalID)
		ownerIDs = append(ownerIDs, ref.OwnerExternalID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	// One statement for the whole batch — this runs per installation per poll
	// cycle, so the round-trip is the cost worth avoiding. Writing only NULLs
	// makes it a no-op in the steady state (every id already recorded), which
	// is what it will be on all but the first cycle after an upgrade.
	//
	// Each ref writes one row at most: the row carrying its id, which gets the
	// owner id when it has none, whatever it is called; else the row holding
	// its name without an id, which gets both unless another row on the host
	// already carries the id, because that row is the repository GitHub calls
	// by it.
	rows, err := s.admin.QueryContext(ctx, `
		UPDATE repositories rp
		   SET external_id = COALESCE(rp.external_id, v.external_id),
		       owner_external_id = COALESCE(rp.owner_external_id, NULLIF(v.owner_external_id, ''))
		  FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[])
		       AS v(source, host, owner, repo, external_id, owner_external_id)
		 WHERE rp.org_id = $1
		   AND rp.source = v.source
		   AND rp.host = v.host
		   AND rp.retired_at IS NULL
		   AND ((rp.external_id = v.external_id AND rp.owner_external_id IS NULL AND v.owner_external_id <> '')
		     OR (rp.external_id IS NULL
		         AND lower(rp.owner) = lower(v.owner)
		         AND lower(rp.repo) = lower(v.repo)
		         AND NOT EXISTS (
		             SELECT 1 FROM repositories o
		              WHERE o.org_id = rp.org_id AND o.source = rp.source AND o.host = rp.host
		                AND o.external_id = v.external_id)))
		RETURNING 1
	`, orgID, sources, hosts, owners, names, ids, ownerIDs)
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

func (s *repoStore) GetPullsPollStateSystem(ctx context.Context, orgID, id string) (string, *time.Time, error) {
	if err := validRepoID(id); err != nil {
		return "", nil, err
	}
	var etag sql.NullString
	var polledAt sql.NullTime
	err := s.admin.QueryRowContext(ctx, `
		SELECT pulls_etag, pulls_polled_at
		  FROM repositories
		 WHERE org_id = $1 AND id = $2
	`, orgID, id).Scan(&etag, &polledAt)
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
	// A handle that was never an id names no row, which is the documented
	// no-op rather than the driver's uuid parse failure.
	if validRepoID(id) != nil {
		return nil
	}
	_, err := s.admin.ExecContext(ctx, `
		UPDATE repositories
		   SET pulls_etag = NULLIF($1, ''), pulls_polled_at = $2
		 WHERE org_id = $3 AND id = $4
	`, etag, polledAt, orgID, id)
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
	repoProfileFullColumns = `id, owner, repo, source, host, external_id, description, has_readme, has_claude_md, has_agents_md,
		       profile_text, clone_url, default_branch, base_branch, profiled_at,
		       clone_status, clone_error, clone_error_kind, retired_at, owner_external_id`

	repoProfileFullColumnsAliased = `rp.id, rp.owner, rp.repo, rp.source, rp.host, rp.external_id, rp.description, rp.has_readme, rp.has_claude_md, rp.has_agents_md,
		       rp.profile_text, rp.clone_url, rp.default_branch, rp.base_branch, rp.profiled_at,
		       rp.clone_status, rp.clone_error, rp.clone_error_kind, rp.retired_at, rp.owner_external_id`
)

// pgScanRepositoryFull reads the projection shared by Get, List, and the ref
// lookup.
func pgScanRepositoryFull(row pgRowScanner) (domain.Repository, error) {
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
