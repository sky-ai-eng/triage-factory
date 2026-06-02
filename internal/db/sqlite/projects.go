package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
// Postgres impl (SKY-297), but SQLite has one connection — both
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
	pinned := p.PinnedRepos
	if pinned == nil {
		pinned = []string{}
	}
	pinnedJSON, err := json.Marshal(pinned)
	if err != nil {
		return "", fmt.Errorf("marshal pinned_repos: %w", err)
	}
	now := time.Now().UTC()
	_, err = s.q.ExecContext(ctx, `
		INSERT INTO projects (id, name, description, curator_session_id, pinned_repos, jira_project_key, linear_project_key, spec_authorship_blueprint_id, team_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		id, p.Name, p.Description,
		nullIfEmpty(p.CuratorSessionID), string(pinnedJSON),
		nullIfEmpty(p.JiraProjectKey), nullIfEmpty(p.LinearProjectKey),
		nullIfEmpty(p.SpecAuthorshipBlueprintID),
		runmode.LocalDefaultTeamID,
		now, now,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *projectStore) Get(ctx context.Context, orgID, id string) (*domain.Project, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT id, name, description, curator_session_id, pinned_repos,
		       jira_project_key, linear_project_key, spec_authorship_blueprint_id,
		       team_id, created_at, updated_at
		FROM projects WHERE id = ?
	`, id)
	return scanSqliteProjectRow(row)
}

func (s *projectStore) List(ctx context.Context, orgID string) ([]domain.Project, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, name, description, curator_session_id, pinned_repos,
		       jira_project_key, linear_project_key, spec_authorship_blueprint_id,
		       team_id, created_at, updated_at
		FROM projects ORDER BY LOWER(name) ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Project{}
	for rows.Next() {
		p, err := scanSqliteProjectRow(rows)
		if err != nil {
			return nil, err
		}
		if p != nil {
			out = append(out, *p)
		}
	}
	return out, rows.Err()
}

func (s *projectStore) Update(ctx context.Context, orgID string, p domain.Project) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	pinned := p.PinnedRepos
	if pinned == nil {
		pinned = []string{}
	}
	pinnedJSON, err := json.Marshal(pinned)
	if err != nil {
		return fmt.Errorf("marshal pinned_repos: %w", err)
	}
	now := time.Now().UTC()
	res, err := s.q.ExecContext(ctx, `
		UPDATE projects
		SET name = ?, description = ?,
		    curator_session_id = ?, pinned_repos = ?,
		    jira_project_key = ?, linear_project_key = ?,
		    spec_authorship_blueprint_id = ?,
		    updated_at = ?
		WHERE id = ?
	`,
		p.Name, p.Description,
		nullIfEmpty(p.CuratorSessionID), string(pinnedJSON),
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
	return nil
}

// ListSystem mirrors List. SKY-297: the project classifier consumes
// this through the admin pool in Postgres; SQLite has one connection,
// so this delegates straight through with the same assertLocalOrg
// gate.
func (s *projectStore) ListSystem(ctx context.Context, orgID string) ([]domain.Project, error) {
	return s.List(ctx, orgID)
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

func (s *projectStore) SetCuratorSessionID(ctx context.Context, orgID, projectID, sessionID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE projects SET curator_session_id = ?, updated_at = ?
		WHERE id = ?
	`, nullIfEmpty(sessionID), time.Now().UTC(), projectID)
	return err
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
		sessionID       sql.NullString
		jiraKey         sql.NullString
		linearKey       sql.NullString
		specBlueprintID sql.NullString
		teamID          sql.NullString
		pinnedJSON      string
		createdAt       time.Time
		updatedAt       time.Time
	)
	err := row.Scan(
		&p.ID, &p.Name, &p.Description, &sessionID, &pinnedJSON,
		&jiraKey, &linearKey, &specBlueprintID,
		&teamID, &createdAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.CuratorSessionID = sessionID.String
	p.JiraProjectKey = jiraKey.String
	p.LinearProjectKey = linearKey.String
	p.SpecAuthorshipBlueprintID = specBlueprintID.String
	p.TeamID = teamID.String
	p.CreatedAt = createdAt
	p.UpdatedAt = updatedAt
	if pinnedJSON == "" {
		p.PinnedRepos = []string{}
	} else if err := json.Unmarshal([]byte(pinnedJSON), &p.PinnedRepos); err != nil {
		return nil, fmt.Errorf("unmarshal pinned_repos: %w", err)
	}
	if p.PinnedRepos == nil {
		p.PinnedRepos = []string{}
	}
	return &p, nil
}
