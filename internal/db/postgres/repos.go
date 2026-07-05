package postgres

import (
	"context"
	"database/sql"
	"errors"
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
// UNIQUE(org_id, owner, repo) natural key. The store interface
// surfaces every repo by its "owner/repo" string — that's what every
// caller passes and what domain.RepoProfile.ID returns. So this impl:
//
//   - Accepts `repoID` ("owner/repo") on every method, splits to
//     (owner, repo), and queries by the natural key.
//   - Returns RepoProfile.ID as `owner + "/" + repo` so callers see
//     identical shapes between backends — the synthetic uuid never
//     leaks across the boundary.
//
// Upsert uses ON CONFLICT (org_id, owner, repo) for the same reason.
//
// # Pool split (SKY-296)
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
	// On conflict refresh profiling metadata only — base_branch and
	// clone-status fields are user/clone-hook owned and shouldn't be
	// clobbered by a re-profile. Matches the SQLite impl's exclude
	// list verbatim.
	_, err := q.ExecContext(ctx, `
		INSERT INTO repo_profiles
		  (org_id, owner, repo, description, has_readme, has_claude_md, has_agents_md,
		   profile_text, clone_url, default_branch, profiled_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7,
		        NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''), $11)
		ON CONFLICT (org_id, owner, repo) DO UPDATE SET
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
		orgID, p.Owner, p.Repo,
		p.Description,
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		p.ProfileText, p.CloneURL, p.DefaultBranch,
		p.ProfiledAt,
	)
	return err
}

func (s *repoStore) List(ctx context.Context, orgID string) ([]domain.RepoProfile, error) {
	return listRepoProfiles(ctx, s.q, orgID)
}

func (s *repoStore) ListSystem(ctx context.Context, orgID string) ([]domain.RepoProfile, error) {
	return listRepoProfiles(ctx, s.admin, orgID)
}

func listRepoProfiles(ctx context.Context, q queryer, orgID string) ([]domain.RepoProfile, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT owner, repo, description, has_readme, has_claude_md, has_agents_md,
		       profile_text, clone_url, default_branch, base_branch, profiled_at,
		       clone_status, clone_error, clone_error_kind
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
		SELECT rp.owner, rp.repo, rp.description, rp.has_readme, rp.has_claude_md, rp.has_agents_md,
		       rp.profile_text, rp.clone_url, rp.default_branch, rp.base_branch, rp.profiled_at,
		       rp.clone_status, rp.clone_error, rp.clone_error_kind
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

		// Upsert skeleton rows for new repos. ON CONFLICT just bumps
		// updated_at — preserves any existing profile data.
		for _, name := range repoNames {
			owner, repo := splitRepoSlug(name)
			if owner == "" || repo == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO repo_profiles (org_id, owner, repo)
				VALUES ($1, $2, $3)
				ON CONFLICT (org_id, owner, repo) DO UPDATE SET updated_at = now()
			`, orgID, owner, repo); err != nil {
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
	row := q.QueryRowContext(ctx, `
		SELECT owner, repo, description, has_readme, has_claude_md, has_agents_md,
		       profile_text, clone_url, default_branch, base_branch, profiled_at,
		       clone_status, clone_error, clone_error_kind
		FROM repo_profiles WHERE org_id = $1 AND owner = $2 AND repo = $3
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
		 WHERE org_id = $4 AND owner = $5 AND repo = $6
	`, status, errMsg, errKind, orgID, owner, repo)
	return err
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
		 WHERE org_id = $1 AND owner = $2 AND repo = $3
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
		 WHERE org_id = $3 AND owner = $4 AND repo = $5
	`, etag, polledAt, orgID, owner, repo)
	return err
}

// pgRowScanner is the common Scan surface of *sql.Row and *sql.Rows.
// Needed because Get scans a single row from QueryRowContext while
// List scans repeatedly from QueryContext.
type pgRowScanner interface {
	Scan(dest ...any) error
}

// pgScanRepoProfileFull reads the 14-column projection shared by
// Get and List (no id column — the natural key is reconstructed
// from owner + "/" + repo so callers see the "owner/repo" form
// uniformly across backends).
func pgScanRepoProfileFull(row pgRowScanner) (domain.RepoProfile, error) {
	var p domain.RepoProfile
	var description, profileText, cloneURL, defaultBranch, baseBranch, cloneError, cloneErrorKind sql.NullString
	var profiledAt sql.NullTime
	if err := row.Scan(&p.Owner, &p.Repo, &description, &p.HasReadme, &p.HasClaudeMd, &p.HasAgentsMd, &profileText, &cloneURL, &defaultBranch, &baseBranch, &profiledAt, &p.CloneStatus, &cloneError, &cloneErrorKind); err != nil {
		return p, err
	}
	p.ID = p.Owner + "/" + p.Repo
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
