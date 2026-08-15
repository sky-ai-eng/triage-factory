package postgres

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

// repoStore is the Postgres impl of db.RepoStore. Wired against the
// app pool in postgres.New: every consumer is request-equivalent
// (repos handler, settings handler, projects handler, curator) or
// runs in a startup/profiler goroutine that already operates within
// the org's identity scope. RLS policy repo_profiles_all gates every
// statement on (org_id = current_org_id() AND user_has_org_access);
// org_id is also included in every WHERE/INSERT clause as defense
// in depth.
//
// # Synthetic uuid vs natural "owner/repo" id
//
// The Postgres schema gives repo_profiles a synthetic uuid PK plus a
// UNIQUE(org_id, source, owner, repo) natural key. The store interface
// surfaces every repo by its "owner/repo" string — that's what every
// caller passes and what domain.RepoProfile.ID returns. So this impl:
//
//   - Accepts `repoID` ("owner/repo") on every method, splits to
//     (owner, repo), and queries by the natural key.
//   - Returns RepoProfile.ID as `owner + "/" + repo` so callers see
//     identical shapes between backends — the synthetic uuid never
//     leaks across the boundary.
//
// Upsert uses ON CONFLICT (org_id, source, owner, repo) for the same
// reason. The slug-keyed reads leave source out of the WHERE clause:
// they match a repo without naming a provider, which is unambiguous
// while GitHub is the only one that issues repositories.
//
// # Pool split
//
// Holds two pools: q is the app pool (request-equivalent consumers —
// repos handler, settings, projects, curator) and admin is the
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

func newRepoStore(q, admin queryer) db.RepoStore {
	return &repoStore{q: q, admin: admin}
}

var _ db.RepoStore = (*repoStore)(nil)

func (s *repoStore) Upsert(ctx context.Context, orgID string, p domain.RepoProfile) error {
	return upsertRepoProfile(ctx, s.q, orgID, p)
}

func (s *repoStore) UpsertSystem(ctx context.Context, orgID string, p domain.RepoProfile) error {
	return upsertRepoProfile(ctx, s.admin, orgID, p)
}

func upsertRepoProfile(ctx context.Context, q queryer, orgID string, p domain.RepoProfile) error {
	source, err := domain.NormalizeRepoSource(p.Source)
	if err != nil {
		return err
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
	_, err = q.ExecContext(ctx, `
		INSERT INTO repo_profiles
		  (org_id, source, owner, repo, external_id, description, has_readme, has_claude_md, has_agents_md,
		   profile_text, clone_url, default_branch, profiled_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8, $9,
		        NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''), $13)
		ON CONFLICT (org_id, source, lower(owner), lower(repo)) DO UPDATE SET
		  external_id    = COALESCE(EXCLUDED.external_id, repo_profiles.external_id),
		  description    = EXCLUDED.description,
		  has_readme     = EXCLUDED.has_readme,
		  has_claude_md  = EXCLUDED.has_claude_md,
		  has_agents_md  = EXCLUDED.has_agents_md,
		  profile_text   = EXCLUDED.profile_text,
		  clone_url      = EXCLUDED.clone_url,
		  default_branch = EXCLUDED.default_branch,
		  profiled_at    = EXCLUDED.profiled_at,
		  updated_at     = now()
	`,
		orgID, source, p.Owner, p.Repo,
		p.ExternalID,
		p.Description,
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		p.ProfileText, p.CloneURL, p.DefaultBranch,
		p.ProfiledAt,
	)
	return err
}

func (s *repoStore) GetOrCreateSystem(ctx context.Context, orgID string, ref domain.RepoRef) (*domain.RepoProfile, error) {
	return getOrCreateRepoProfile(ctx, s.admin, orgID, ref)
}

// getOrCreateRepoProfile is the store method's body, taking a queryer so it
// composes inside a caller's transaction. Its create half is
// insertRepoProfileRow, which the tracked-set reconcile in
// team_github_repos.go calls directly — that reconcile has already computed
// which repos are new, so it skips the lookup rather than the insert.
func getOrCreateRepoProfile(ctx context.Context, q queryer, orgID string, ref domain.RepoRef) (*domain.RepoProfile, error) {
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return nil, err
	}
	// One transaction around lookup → create → re-read, so the answer this
	// returns is the row that exists at a single point in time rather than
	// three separate ones. A caller that already has a tx keeps it — inTx
	// passes a *sql.Tx through.
	var out *domain.RepoProfile
	err = inTx(ctx, q, func(tx queryer) error {
		existing, err := findRepoProfileByRef(ctx, tx, orgID, source, ref)
		if err != nil {
			return err
		}
		if existing == nil {
			if err := insertRepoProfileRow(ctx, tx, orgID, ref); err != nil {
				return err
			}
			// Re-read rather than RETURNING: the INSERT is guarded and
			// DO NOTHING, so this is also what returns the winner's row when
			// a concurrent creator got there first.
			created, err := findRepoProfileByRef(ctx, tx, orgID, source, ref)
			if err != nil {
				return err
			}
			if created == nil {
				return fmt.Errorf("repo_profiles row for %s vanished immediately after insert", ref.Slug())
			}
			out = created
			return nil
		}
		// The row stands as it is, except for an id the caller just learned.
		if ref.ExternalID != "" && existing.ExternalID != ref.ExternalID {
			if _, err := tx.ExecContext(ctx, `
				UPDATE repo_profiles
				   SET external_id = $1
				 WHERE org_id = $2 AND source = $3 AND owner = $4 AND repo = $5
			`, ref.ExternalID, orgID, source, existing.Owner, existing.Repo); err != nil {
				return fmt.Errorf("record external id for %s: %w", ref.Slug(), err)
			}
			existing.ExternalID = ref.ExternalID
		}
		out = existing
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// findRepoProfileByRef looks a repository up by provider identity, matching
// owner/repo case-INSENSITIVELY (GitHub identifiers are). Returns nil when the
// repository has no row.
func findRepoProfileByRef(ctx context.Context, q queryer, orgID, source string, ref domain.RepoRef) (*domain.RepoProfile, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repo_profiles
		WHERE org_id = $1 AND source = $2 AND lower(owner) = lower($3) AND lower(repo) = lower($4)
	`, orgID, source, ref.Owner, ref.Repo)
	p, err := pgScanRepoProfileFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// insertRepoProfileRow is the single INSERT that brings a repository into
// repo_profiles — get-or-create, SetConfigured, and the tracked-set reconcile
// all create through it, so no create path can write a row without the
// identity columns, and all three agree on what "already exists" means.
//
// "Already exists" means the repo_profiles_identity index: (org_id, source)
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
func insertRepoProfileRow(ctx context.Context, q queryer, orgID string, ref domain.RepoRef) error {
	source, err := domain.NormalizeRepoSource(ref.Source)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO repo_profiles (org_id, source, owner, repo, external_id)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''))
		ON CONFLICT DO NOTHING
	`, orgID, source, ref.Owner, ref.Repo, ref.ExternalID); err != nil {
		return fmt.Errorf("insert repo_profiles[%s]: %w", ref.Slug(), err)
	}
	return nil
}

func (s *repoStore) List(ctx context.Context, orgID string) ([]domain.RepoProfile, error) {
	return listRepoProfiles(ctx, s.q, orgID)
}

func (s *repoStore) ListSystem(ctx context.Context, orgID string) ([]domain.RepoProfile, error) {
	return listRepoProfiles(ctx, s.admin, orgID)
}

func listRepoProfiles(ctx context.Context, q queryer, orgID string) ([]domain.RepoProfile, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repo_profiles
		WHERE org_id = $1
		ORDER BY owner, repo
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.RepoProfile{}
	for rows.Next() {
		p, err := pgScanRepoProfileFull(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// repoProfileTrackedByViewerTeams scopes a repo_profiles row (alias rp) to
// the tracked repos of the viewer's teams — the RLS-does-the-scoping
// semi-join pattern factory.go's factoryGitHubRepoTrackedExists and
// entities.go's jiraTeamProjectMembershipExists use for the same class of
// org-wide-entity-list leak (TFAC-559). Under the app pool (tf_app, RLS
// active) team_github_repos_select already constrains visible rows to the
// caller's memberships, so the EXISTS auto-scopes with no explicit team_id.
// The teams join binds org (rp.org_id) as defense-in-depth for the admin
// pool, where team_github_repos carries no org_id column of its own.
const repoProfileTrackedByViewerTeams = `EXISTS (
	SELECT 1 FROM team_github_repos g
	JOIN teams tm ON tm.id = g.team_id
	WHERE tm.org_id = rp.org_id
	  AND lower(g.owner) = lower(rp.owner)
	  AND lower(g.repo) = lower(rp.repo)
)`

func (s *repoStore) ListTeamScoped(ctx context.Context, orgID string) ([]domain.RepoProfile, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+repoProfileFullColumnsAliased+`
		FROM repo_profiles rp
		WHERE rp.org_id = $1
		  AND `+repoProfileTrackedByViewerTeams+`
		ORDER BY rp.owner, rp.repo
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.RepoProfile{}
	for rows.Next() {
		p, err := pgScanRepoProfileFull(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *repoStore) ListWithContent(ctx context.Context, orgID string) ([]domain.RepoProfile, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT owner, repo, description, has_readme, has_claude_md, has_agents_md,
		       profile_text, clone_url, default_branch, base_branch
		FROM repo_profiles
		WHERE org_id = $1
		  AND profile_text IS NOT NULL AND profile_text != ''
		ORDER BY owner, repo
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.RepoProfile{}
	for rows.Next() {
		var p domain.RepoProfile
		var description, profileText, cloneURL, defaultBranch, baseBranch sql.NullString
		if err := rows.Scan(&p.Owner, &p.Repo, &description, &p.HasReadme, &p.HasClaudeMd, &p.HasAgentsMd, &profileText, &cloneURL, &defaultBranch, &baseBranch); err != nil {
			return nil, err
		}
		p.ID = p.Owner + "/" + p.Repo
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
	// Multi-statement: delete dropped repos + upsert skeleton rows
	// for every desired entry. Inside one tx so the table can't
	// observe a partial mid-sync state.
	return inTx(ctx, s.q, func(tx queryer) error {
		desired := make(map[string]bool, len(repoNames))
		for _, name := range repoNames {
			desired[name] = true
		}

		// List existing (owner/repo) entries scoped to org.
		rows, err := tx.QueryContext(ctx,
			`SELECT owner, repo FROM repo_profiles WHERE org_id = $1`, orgID)
		if err != nil {
			return err
		}
		var existing []struct{ owner, repo string }
		for rows.Next() {
			var owner, repo string
			if err := rows.Scan(&owner, &repo); err != nil {
				rows.Close()
				return err
			}
			existing = append(existing, struct{ owner, repo string }{owner, repo})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// Delete repos no longer selected.
		for _, e := range existing {
			id := e.owner + "/" + e.repo
			if !desired[id] {
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM repo_profiles WHERE org_id = $1 AND owner = $2 AND repo = $3`,
					orgID, e.owner, e.repo,
				); err != nil {
					return err
				}
			}
		}

		// Create skeleton rows for new repos through the shared insert, so
		// they carry the same identity columns the tracked-set reconcile
		// writes. A repo already present is left exactly as it stands.
		for _, name := range repoNames {
			owner, repo := splitRepoSlug(name)
			if owner == "" || repo == "" {
				continue
			}
			if err := insertRepoProfileRow(ctx, tx, orgID, domain.RepoRef{Owner: owner, Repo: repo}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *repoStore) ListConfiguredNames(ctx context.Context, orgID string) ([]string, error) {
	return listConfiguredRepoNames(ctx, s.q, orgID)
}

func (s *repoStore) ListConfiguredNamesSystem(ctx context.Context, orgID string) ([]string, error) {
	return listConfiguredRepoNames(ctx, s.admin, orgID)
}

func listConfiguredRepoNames(ctx context.Context, q queryer, orgID string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT owner, repo FROM repo_profiles WHERE org_id = $1 ORDER BY owner, repo`,
		orgID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var owner, repo string
		if err := rows.Scan(&owner, &repo); err != nil {
			return nil, err
		}
		out = append(out, owner+"/"+repo)
	}
	return out, rows.Err()
}

func (s *repoStore) CountConfigured(ctx context.Context, orgID string) (int, error) {
	return countConfiguredRepos(ctx, s.q, orgID)
}

func (s *repoStore) CountConfiguredSystem(ctx context.Context, orgID string) (int, error) {
	return countConfiguredRepos(ctx, s.admin, orgID)
}

func countConfiguredRepos(ctx context.Context, q queryer, orgID string) (int, error) {
	var count int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM repo_profiles WHERE org_id = $1`, orgID,
	).Scan(&count)
	return count, err
}

func (s *repoStore) UpdateBaseBranch(ctx context.Context, orgID, repoID, baseBranch string) error {
	owner, repo := splitRepoSlug(repoID)
	if owner == "" || repo == "" {
		return nil
	}
	// Case-insensitive match (GitHub identifiers are, per TracksRepoSystem /
	// TracksRepoViewerScoped) — TFAC-559's repoAccessAllowed gate matches
	// case-insensitively, so a caller reaching this with different casing than
	// what's stored must still find the row rather than silently affecting 0.
	_, err := s.q.ExecContext(ctx, `
		UPDATE repo_profiles
		   SET base_branch = NULLIF($1, '')
		 WHERE org_id = $2 AND lower(owner) = lower($3) AND lower(repo) = lower($4)
	`, baseBranch, orgID, owner, repo)
	return err
}

func (s *repoStore) SeedCloneURL(ctx context.Context, orgID, repoID, cloneURL string) error {
	owner, repo := splitRepoSlug(repoID)
	if owner == "" || repo == "" || strings.TrimSpace(cloneURL) == "" {
		return nil
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE repo_profiles
		   SET clone_url = $1, updated_at = now()
		 WHERE org_id = $2 AND lower(owner) = lower($3) AND lower(repo) = lower($4)
		   AND (clone_url IS NULL OR clone_url = '')
	`, cloneURL, orgID, owner, repo)
	return err
}

func (s *repoStore) Get(ctx context.Context, orgID, repoID string) (*domain.RepoProfile, error) {
	return getRepoProfile(ctx, s.q, orgID, repoID)
}

func (s *repoStore) GetSystem(ctx context.Context, orgID, repoID string) (*domain.RepoProfile, error) {
	return getRepoProfile(ctx, s.admin, orgID, repoID)
}

func getRepoProfile(ctx context.Context, q queryer, orgID, repoID string) (*domain.RepoProfile, error) {
	owner, repo := splitRepoSlug(repoID)
	if owner == "" || repo == "" {
		return nil, nil
	}
	// Case-insensitive, like every other lookup that takes a caller-supplied
	// slug. The callers that pass one they did NOT read out of this table are
	// the ones that matter: `tfac exec workspace add owner/repo` hands the
	// agent's argv straight through, and a miss there is reported as "repo is
	// not configured in Triage Factory" — while the team-tracking gate on the
	// next line of that same function matches case-insensitively and would
	// have said yes.
	row := q.QueryRowContext(ctx, `
		SELECT `+repoProfileFullColumns+`
		FROM repo_profiles
		WHERE org_id = $1 AND lower(owner) = lower($2) AND lower(repo) = lower($3)
	`, orgID, owner, repo)
	p, err := pgScanRepoProfileFull(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *repoStore) UpdateCloneStatus(ctx context.Context, orgID, owner, repo, status, errMsg, errKind string) error {
	return updateRepoCloneStatus(ctx, s.q, orgID, owner, repo, status, errMsg, errKind)
}

func (s *repoStore) UpdateCloneStatusSystem(ctx context.Context, orgID, owner, repo, status, errMsg, errKind string) error {
	return updateRepoCloneStatus(ctx, s.admin, orgID, owner, repo, status, errMsg, errKind)
}

func updateRepoCloneStatus(ctx context.Context, q queryer, orgID, owner, repo, status, errMsg, errKind string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE repo_profiles
		   SET clone_status = $1, clone_error = NULLIF($2, ''), clone_error_kind = NULLIF($3, '')
		 WHERE org_id = $4 AND lower(owner) = lower($5) AND lower(repo) = lower($6)
	`, status, errMsg, errKind, orgID, owner, repo)
	return err
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
		UPDATE repo_profiles rp
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

func (s *repoStore) GetPullsPollStateSystem(ctx context.Context, orgID, repoID string) (string, *time.Time, error) {
	owner, repo := splitRepoSlug(repoID)
	if owner == "" || repo == "" {
		return "", nil, nil
	}
	var etag sql.NullString
	var polledAt sql.NullTime
	err := s.admin.QueryRowContext(ctx, `
		SELECT pulls_etag, pulls_polled_at
		  FROM repo_profiles
		 WHERE org_id = $1 AND lower(owner) = lower($2) AND lower(repo) = lower($3)
	`, orgID, owner, repo).Scan(&etag, &polledAt)
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
	owner, repo := splitRepoSlug(repoID)
	if owner == "" || repo == "" {
		return nil
	}
	_, err := s.admin.ExecContext(ctx, `
		UPDATE repo_profiles
		   SET pulls_etag = NULLIF($1, ''), pulls_polled_at = $2
		 WHERE org_id = $3 AND lower(owner) = lower($4) AND lower(repo) = lower($5)
	`, etag, polledAt, orgID, owner, repo)
	return err
}

// pgRowScanner is the common Scan surface of *sql.Row and *sql.Rows.
// Needed because Get scans a single row from QueryRowContext while
// List scans repeatedly from QueryContext.
type pgRowScanner interface {
	Scan(dest ...any) error
}

// repoProfileFullColumns is the projection pgScanRepoProfileFull reads, named
// once so the queries that share it cannot drift out of column order.
// repoProfileFullColumnsAliased is the same list qualified for the `rp` alias
// ListTeamScoped's semi-join needs.
const (
	repoProfileFullColumns = `owner, repo, source, external_id, description, has_readme, has_claude_md, has_agents_md,
		       profile_text, clone_url, default_branch, base_branch, profiled_at,
		       clone_status, clone_error, clone_error_kind`

	repoProfileFullColumnsAliased = `rp.owner, rp.repo, rp.source, rp.external_id, rp.description, rp.has_readme, rp.has_claude_md, rp.has_agents_md,
		       rp.profile_text, rp.clone_url, rp.default_branch, rp.base_branch, rp.profiled_at,
		       rp.clone_status, rp.clone_error, rp.clone_error_kind`
)

// pgScanRepoProfileFull reads the projection shared by Get, List, and the
// get-or-create lookup (no id column — the natural key is reconstructed
// from owner + "/" + repo so callers see the "owner/repo" form
// uniformly across backends).
func pgScanRepoProfileFull(row pgRowScanner) (domain.RepoProfile, error) {
	var p domain.RepoProfile
	var externalID, description, profileText, cloneURL, defaultBranch, baseBranch, cloneError, cloneErrorKind sql.NullString
	var profiledAt sql.NullTime
	if err := row.Scan(&p.Owner, &p.Repo, &p.Source, &externalID, &description, &p.HasReadme, &p.HasClaudeMd, &p.HasAgentsMd, &profileText, &cloneURL, &defaultBranch, &baseBranch, &profiledAt, &p.CloneStatus, &cloneError, &cloneErrorKind); err != nil {
		return p, err
	}
	p.ID = p.Owner + "/" + p.Repo
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

// splitRepoSlug splits "owner/repo" at the first slash. Returns
// empty halves if the input has no slash; the caller treats those
// as no-ops (configured repos PUT silently skips malformed entries,
// Get/UpdateBaseBranch return nil/nil for them).
func splitRepoSlug(s string) (owner, repo string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}
