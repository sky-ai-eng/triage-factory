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
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// projectStore is the SQLite impl of db.ProjectStore. SQL bodies are
// moved verbatim from the pre-D2 internal/db/projects.go; the only
// behavioral changes are the orgID assertion at each method entry and
// the ctx-aware database/sql methods.
//
// teamID is accepted on Create for signature parity with the Postgres
// impl but ignored — the local schema covers creator_user_id via
// column DEFAULT and pins team_id to LocalDefaultTeamID at insert
// time (matches the projects_team_visibility_requires_team CHECK
// under the default visibility='team').
//
// The constructor takes two queryers for signature parity with the
// Postgres impl, but SQLite has one connection — both
// arguments collapse onto the same queryer. The `...System` admin-
// pool variants are thin wrappers around the non-System methods.
type projectStore struct{ q queryer }

func newProjectStore(q, _ queryer) db.ProjectStore { return &projectStore{q: q} }

var _ db.ProjectStore = (*projectStore)(nil)

func (s *projectStore) Create(ctx context.Context, orgID, teamID string, p domain.Project) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	_ = teamID // ignored in local mode

	id := p.ID
	if id == "" {
		id = uuid.New().String()
	}
	now := time.Now().UTC()
	if err := inTx(ctx, s.q, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO projects (id, name, description, jira_project_key, linear_project_key, spec_authorship_blueprint_id, team_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			id, p.Name, p.Description,
			nullIfEmpty(p.JiraProjectKey), nullIfEmpty(p.LinearProjectKey),
			nullIfEmpty(p.SpecAuthorshipBlueprintID),
			runmode.LocalDefaultTeamID,
			now, now,
		); err != nil {
			return err
		}
		return replacePinnedRepos(ctx, tx, id, p.PinnedRepos)
	}); err != nil {
		return "", err
	}
	return id, nil
}

func (s *projectStore) Get(ctx context.Context, orgID, id string) (*domain.Project, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT id, name, description,
		       jira_project_key, linear_project_key, spec_authorship_blueprint_id,
		       team_id, visibility, creator_user_id, created_at, updated_at
		FROM projects WHERE id = ?
	`, id)
	p, err := scanSqliteProjectRow(row)
	if err != nil || p == nil {
		return p, err
	}
	if err := attachPinnedRepos(ctx, s.q, []*domain.Project{p}); err != nil {
		return nil, err
	}
	return p, nil
}

// GetSystem is N=1-unscoped in SQLite — it collapses to Get (there is no
// admin/app pool split locally). See the interface doc.
func (s *projectStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Project, error) {
	return s.Get(ctx, orgID, id)
}

func (s *projectStore) List(ctx context.Context, orgID string, opts db.ListOpts) ([]domain.Project, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects`).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `
		SELECT id, name, description,
		       jira_project_key, linear_project_key, spec_authorship_blueprint_id,
		       team_id, visibility, creator_user_id, created_at, updated_at
		FROM projects ORDER BY LOWER(name) ASC, id`
	args := []any{}
	if opts.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.Project{}
	for rows.Next() {
		p, err := scanSqliteProjectRow(rows)
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
	if err := attachPinnedRepos(ctx, s.q, refs); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (s *projectStore) Update(ctx context.Context, orgID string, p domain.Project) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	now := time.Now().UTC()
	return inTx(ctx, s.q, func(tx queryer) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE projects
			SET name = ?, description = ?,
			    jira_project_key = ?, linear_project_key = ?,
			    spec_authorship_blueprint_id = ?,
			    updated_at = ?
			WHERE id = ?
		`,
			p.Name, p.Description,
			nullIfEmpty(p.JiraProjectKey), nullIfEmpty(p.LinearProjectKey),
			nullIfEmpty(p.SpecAuthorshipBlueprintID),
			now, p.ID,
		)
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
		return replacePinnedRepos(ctx, tx, p.ID, p.PinnedRepos)
	})
}

// ListSystem mirrors List. The project classifier consumes
// this through the admin pool in Postgres; SQLite has one connection,
// so this delegates straight through with the same assertLocalOrg
// gate.
func (s *projectStore) ListSystem(ctx context.Context, orgID string) ([]domain.Project, error) {
	// Unwindowed: system callers resolve the whole set.
	rows, _, err := s.List(ctx, orgID, db.Unwindowed)
	return rows, err
}

// ResolveOrgSystem returns the local sentinel org for any known
// project id. SQLite has a single tenant per database file so the
// only valid org_id for any row is runmode.LocalDefaultOrgID; we
// still gate on the row's existence so a stale projectID surfaces as
// ("", nil) rather than confidently claiming the sentinel org.
func (s *projectStore) ResolveOrgSystem(ctx context.Context, projectID string) (string, error) {
	var orgID string
	err := s.q.QueryRowContext(ctx, `SELECT org_id FROM projects WHERE id = ?`, projectID).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return orgID, nil
}

func (s *projectStore) Delete(ctx context.Context, orgID, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
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
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx,
		`UPDATE projects SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		id,
	)
	return err
}

// scanSqliteProjectRow reads a SELECT … FROM projects row into a
// *domain.Project. (nil, nil) on sql.ErrNoRows so callers can map
// missing rows to a 404 without a sentinel comparison.
func scanSqliteProjectRow(row interface {
	Scan(dest ...any) error
}) (*domain.Project, error) {
	var (
		p               domain.Project
		jiraKey         sql.NullString
		linearKey       sql.NullString
		specBlueprintID sql.NullString
		teamID          sql.NullString
		createdAt       time.Time
		updatedAt       time.Time
	)
	err := row.Scan(
		&p.ID, &p.Name, &p.Description,
		&jiraKey, &linearKey, &specBlueprintID,
		&teamID, &p.Visibility, &p.CreatorUserID, &createdAt, &updatedAt,
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
	p.CreatedAt = createdAt
	p.UpdatedAt = updatedAt
	p.PinnedRepos = []string{}
	return &p, nil
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
func replacePinnedRepos(ctx context.Context, q queryer, projectID string, slugs []string) error {
	if _, err := q.ExecContext(ctx,
		`DELETE FROM project_pinned_repos WHERE project_id = ?`, projectID,
	); err != nil {
		return fmt.Errorf("clear pinned repos for project %s: %w", projectID, err)
	}
	for i, slug := range slugs {
		owner, repo := splitRepoSlug(slug)
		if owner == "" || repo == "" {
			continue // malformed entry, skipped like every other slug parse here
		}
		repositoryID, err := getOrCreateRepositoryID(ctx, q, domain.RepoRef{Owner: owner, Repo: repo})
		if err != nil {
			return fmt.Errorf("resolve pinned repo %s: %w", slug, err)
		}
		if _, err := q.ExecContext(ctx, `
			INSERT INTO project_pinned_repos (project_id, repository_id, position)
			VALUES (?, ?, ?)
			ON CONFLICT(project_id, repository_id) DO NOTHING
		`, projectID, repositoryID, i); err != nil {
			return fmt.Errorf("pin repo %s to project %s: %w", slug, projectID, err)
		}
	}
	return nil
}

// attachPinnedRepos fills PinnedRepos on each project from the join table. One
// query for the whole page rather than one per project — the list read is the
// hot one, and the alternative is an aggregate subquery whose element order
// SQLite does not promise.
func attachPinnedRepos(ctx context.Context, q queryer, projects []*domain.Project) error {
	if len(projects) == 0 {
		return nil
	}
	byID := make(map[string]*domain.Project, len(projects))
	placeholders := make([]string, 0, len(projects))
	args := make([]any, 0, len(projects))
	for _, p := range projects {
		byID[p.ID] = p
		placeholders = append(placeholders, "?")
		args = append(args, p.ID)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT pr.project_id, r.owner || '/' || r.repo
		  FROM project_pinned_repos pr
		  JOIN repositories r ON r.id = pr.repository_id
		 WHERE pr.project_id IN (`+strings.Join(placeholders, ", ")+`)
		 ORDER BY pr.project_id, pr.position
	`, args...)
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
