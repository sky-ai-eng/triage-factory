package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// translateVisibilityErr maps the projects_insert / projects_update RLS
// policies' WITH CHECK denial (SQLSTATE 42501 — the caller's role doesn't
// match the visibility they're writing: org needs an org admin, private
// needs the row's own creator) to db.ErrVisibilityForbidden, so the
// handler can answer a clean 403 instead of a raw RLS error. Any other
// error (or nil) passes through untouched.
func translateVisibilityErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		return errors.Join(db.ErrVisibilityForbidden, err)
	}
	return err
}

// projectStore is the Postgres impl of db.ProjectStore. Holds two
// pools:
//
//   - q: app pool (tf_app, RLS-active). Every request-equivalent
//     consumer (projects handler, curator, backfill, project_entities)
//     hits this side. RLS policies projects_{select,insert,update,delete}
//     gate every statement; org_id defense-in-depth fires alongside.
//
//   - admin: admin pool (supabase_admin, BYPASSRLS). The project
//     classifier (internal/projectclassify) reads every org's project
//     set during its fan-out and has no JWT-claims context. ListSystem
//     routes through admin so the classifier can pair each org's
//     unclassified entities against that org's projects without
//     impersonating any one user. Same pattern EntityStore /
//     RepositoryStore / ConversationStore use.
//
// PinnedRepos is not a column. It lives in project_pinned_repos, one row per
// (project, repository), so the pin is a checkable reference rather than a
// slug in a JSON array. The store still surfaces it as an ordered []string of
// "owner/repo" — writes resolve each slug to a registry row, reads join back.
type projectStore struct {
	q     queryer
	admin queryer
}

func newProjectStore(q, admin queryer) db.ProjectStore {
	return &projectStore{q: q, admin: admin}
}

var _ db.ProjectStore = (*projectStore)(nil)

func (s *projectStore) Create(ctx context.Context, orgID, teamID string, p domain.Project) (string, error) {
	id := p.ID
	if id == "" {
		id = uuid.New().String()
	}
	// teamID: required only for visibility="team" (the
	// projects_team_visibility_requires_team CHECK). private/org
	// visibility permit a NULL team_id — TFAC-562's teamless-user case:
	// the human picks a team at the Create UI when one applies,
	// but a private/org project may have none. A "first of caller's
	// teams" or "any team in org" fallback would either silently attach
	// to the wrong team or collide with projects_insert RLS
	// (tf.user_can_write_team(team_id)). The handler ships
	// runmode.LocalDefaultTeamID for local mode; the sentinel filter
	// converts that to empty.
	teamBind := teamID
	if teamBind == runmode.LocalDefaultTeamID {
		teamBind = ""
	}
	visibility := p.Visibility
	if visibility == "" {
		visibility = domain.ProjectVisibilityTeam
	}
	if visibility == domain.ProjectVisibilityTeam && teamBind == "" {
		return "", fmt.Errorf("project store: team_id required for Postgres Create under visibility=%q (handler must thread the user-selected team from request context; the SQLite-only LocalDefaultTeamID sentinel does not satisfy the projects_insert RLS policy)", domain.ProjectVisibilityTeam)
	}

	// creator_user_id: pulled from tf.current_user_id() set by WithTx
	// claims — same pattern every other app-pool store uses
	// (event_handlers, swipes, chains, tasks, prompts). Org-owner
	// fallback covers the pgtest admin-pool path (BYPASSRLS, no
	// claims set); in production multi-mode under tf_app, claims are
	// always set and the COALESCE stops at the first branch.
	if err := inTx(ctx, s.q, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO projects
			  (id, org_id, creator_user_id, team_id, visibility, name, description,
			   jira_project_key, linear_project_key, spec_authorship_blueprint_id)
			VALUES
			  ($1, $2,
			   COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			   NULLIF($3, '')::uuid,
			   $4,
			   $5, $6,
			   NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''))
		`,
			id, orgID, teamBind, visibility,
			p.Name, p.Description,
			p.JiraProjectKey, p.LinearProjectKey, p.SpecAuthorshipBlueprintID,
		); err != nil {
			return translateVisibilityErr(err)
		}
		return replacePinnedRepos(ctx, tx, orgID, id, p.PinnedRepos)
	}); err != nil {
		return "", err
	}
	return id, nil
}

func (s *projectStore) Get(ctx context.Context, orgID, id string) (*domain.Project, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT id, name, description,
		       jira_project_key, linear_project_key, spec_authorship_blueprint_id,
		       team_id, visibility, creator_user_id, created_at, updated_at
		FROM projects
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	return getProjectWithPins(ctx, s.q, row)
}

// GetSystem is the admin-pool (BYPASSRLS) variant of Get — a JWT-less
// background job resolving one project by id under an explicit orgID. See the
// interface doc.
func (s *projectStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Project, error) {
	row := s.admin.QueryRowContext(ctx, `
		SELECT id, name, description,
		       jira_project_key, linear_project_key, spec_authorship_blueprint_id,
		       team_id, visibility, creator_user_id, created_at, updated_at
		FROM projects
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	return getProjectWithPins(ctx, s.admin, row)
}

func (s *projectStore) List(ctx context.Context, orgID string, opts db.ListOpts) ([]domain.Project, int, error) {
	return listProjects(ctx, s.q, orgID, opts)
}

func (s *projectStore) ListSystem(ctx context.Context, orgID string) ([]domain.Project, error) {
	// Unwindowed: system callers resolve the whole set.
	rows, _, err := listProjects(ctx, s.admin, orgID, db.Unwindowed)
	return rows, err
}

// ResolveOrgSystem returns the project's owning org id via the admin
// pool — RLS would hide the row from a JWT-claims-free caller, and
// the consumer (kbwatcher's broadcast scoping) has no identity
// context. Returns ("", nil) when no row matches so callers can drop
// the broadcast cleanly (see kbwatcher's ProjectOrgResolver docs —
// the watcher treats empty as "drop", not "fan out system-wide").
//
// projectID comes from a filesystem directory name (the kb watcher
// reads /<projects-root>/<id>/knowledge-base/...), which can be any
// string the operator put on disk. A non-UUID string would otherwise
// surface as SQLSTATE 22P02 from the UUID-typed projects.id column
// rather than the clean no-row shape this method documents. Short-
// circuit invalid UUIDs to ("", nil) per the convention in uuid.go.
func (s *projectStore) ResolveOrgSystem(ctx context.Context, projectID string) (string, error) {
	if !isValidUUID(projectID) {
		return "", nil
	}
	var orgID string
	err := s.admin.QueryRowContext(ctx, `SELECT org_id::text FROM projects WHERE id = $1`, projectID).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return orgID, nil
}

func listProjects(ctx context.Context, q queryer, orgID string, opts db.ListOpts) ([]domain.Project, int, error) {
	var total int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE org_id = $1`, orgID).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `
		SELECT id, name, description,
		       jira_project_key, linear_project_key, spec_authorship_blueprint_id,
		       team_id, visibility, creator_user_id, created_at, updated_at
		FROM projects
		WHERE org_id = $1
		ORDER BY LOWER(name) ASC, id`
	args := []any{orgID}
	if opts.Limit > 0 {
		query += ` LIMIT $2 OFFSET $3`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.Project{}
	for rows.Next() {
		p, err := scanProjectRow(rows)
		if err != nil {
			return nil, 0, err
		}
		if p != nil {
			out = append(out, *p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	refs := make([]*domain.Project, len(out))
	for i := range out {
		refs[i] = &out[i]
	}
	if err := attachPinnedRepos(ctx, q, refs); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// getProjectWithPins scans one project row and fills its pinned repos, on
// whichever pool the caller's row came from.
func getProjectWithPins(ctx context.Context, q queryer, row *sql.Row) (*domain.Project, error) {
	p, err := scanProjectRow(row)
	if err != nil || p == nil {
		return p, err
	}
	if err := attachPinnedRepos(ctx, q, []*domain.Project{p}); err != nil {
		return nil, err
	}
	return p, nil
}

// replacePinnedRepos makes project_pinned_repos match slugs exactly, in the
// order given. position preserves that order: the API surfaces pinned_repos as
// an ordered list and a project round-trips through bundle export/import, so
// re-deriving the order from the slug would silently reshuffle a list somebody
// arranged.
//
// The registry row is get-or-created rather than looked up, because a pin can
// legitimately arrive before the repository is tracked — the bundle import
// creates the project with its pinned list and tracks the repos in the next
// statement of the same transaction. Whether a pin names a repo the team is
// allowed to pin stays the handler's check (validatePinnedRepos); this is the
// reference, and the FK is what keeps it from dangling.
func replacePinnedRepos(ctx context.Context, q queryer, orgID, projectID string, slugs []string) error {
	if _, err := q.ExecContext(ctx,
		`DELETE FROM project_pinned_repos WHERE project_id = $1`, projectID,
	); err != nil {
		return fmt.Errorf("clear pinned repos for project %s: %w", projectID, err)
	}
	for i, slug := range slugs {
		owner, repo := splitRepoSlug(slug)
		if owner == "" || repo == "" {
			continue // malformed entry, skipped like every other slug parse here
		}
		repositoryID, err := getOrCreateRepositoryID(ctx, q, orgID, domain.RepoRef{Owner: owner, Repo: repo})
		if err != nil {
			return fmt.Errorf("resolve pinned repo %s: %w", slug, err)
		}
		if _, err := q.ExecContext(ctx, `
			INSERT INTO project_pinned_repos (project_id, repository_id, "position")
			VALUES ($1, $2, $3)
			ON CONFLICT (project_id, repository_id) DO NOTHING
		`, projectID, repositoryID, i); err != nil {
			return fmt.Errorf("pin repo %s to project %s: %w", slug, projectID, err)
		}
	}
	return nil
}

// attachPinnedRepos fills PinnedRepos on each project from the join table. One
// query for the whole page rather than one per project — the list read is the
// hot one.
func attachPinnedRepos(ctx context.Context, q queryer, projects []*domain.Project) error {
	if len(projects) == 0 {
		return nil
	}
	byID := make(map[string]*domain.Project, len(projects))
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		byID[p.ID] = p
		ids = append(ids, p.ID)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT pr.project_id::text, r.owner || '/' || r.repo
		  FROM project_pinned_repos pr
		  JOIN repositories r ON r.id = pr.repository_id
		 WHERE pr.project_id = ANY ($1::uuid[])
		 ORDER BY pr.project_id, pr."position"
	`, pgUUIDArray(ids))
	if err != nil {
		return fmt.Errorf("read pinned repos: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var projectID, slug string
		if err := rows.Scan(&projectID, &slug); err != nil {
			return err
		}
		if p := byID[projectID]; p != nil {
			p.PinnedRepos = append(p.PinnedRepos, slug)
		}
	}
	return rows.Err()
}

func (s *projectStore) Update(ctx context.Context, orgID string, p domain.Project) error {
	return inTx(ctx, s.q, func(tx queryer) error {
		return updateProject(ctx, tx, orgID, p)
	})
}

func updateProject(ctx context.Context, q queryer, orgID string, p domain.Project) error {
	res, err := q.ExecContext(ctx, `
		UPDATE projects
		SET name = $1, description = $2,
		    jira_project_key = NULLIF($3, ''),
		    linear_project_key = NULLIF($4, ''),
		    spec_authorship_blueprint_id = NULLIF($5, ''),
		    visibility = $6,
		    updated_at = now()
		WHERE org_id = $7 AND id = $8
	`,
		p.Name, p.Description,
		p.JiraProjectKey, p.LinearProjectKey, p.SpecAuthorshipBlueprintID,
		p.Visibility,
		orgID, p.ID,
	)
	if err != nil {
		return translateVisibilityErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return replacePinnedRepos(ctx, q, orgID, p.ID, p.PinnedRepos)
}

func (s *projectStore) Delete(ctx context.Context, orgID, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM projects WHERE org_id = $1 AND id = $2`, orgID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *projectStore) BumpUpdatedAt(ctx context.Context, orgID, id string) error {
	_, err := s.q.ExecContext(ctx,
		`UPDATE projects SET updated_at = now() WHERE org_id = $1 AND id = $2`,
		orgID, id,
	)
	return err
}

// scanProjectRow reads a SELECT … FROM projects row into a
// *domain.Project. (nil, nil) on sql.ErrNoRows so callers can map
// missing rows to a 404 without a sentinel comparison.
func scanProjectRow(row interface {
	Scan(dest ...any) error
}) (*domain.Project, error) {
	var (
		p               domain.Project
		jiraKey         sql.NullString
		linearKey       sql.NullString
		specBlueprintID sql.NullString
		teamID          sql.NullString
	)
	err := row.Scan(
		&p.ID, &p.Name, &p.Description,
		&jiraKey, &linearKey, &specBlueprintID,
		&teamID, &p.Visibility, &p.CreatorUserID, &p.CreatedAt, &p.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.JiraProjectKey = jiraKey.String
	p.LinearProjectKey = linearKey.String
	p.SpecAuthorshipBlueprintID = specBlueprintID.String
	p.TeamID = teamID.String
	p.PinnedRepos = []string{}
	return &p, nil
}
