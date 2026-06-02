package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// projectStore is the Postgres impl of db.ProjectStore. Holds two
// pools (SKY-297):
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
//     RepoStore / AgentRunStore use.
//
// pinned_repos is jsonb in Postgres vs text-JSON in SQLite. The
// jsonb cast happens at the placeholder level ($N::jsonb) — callers
// always pass a marshalled string, and the impl coerces.
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
	pinned := p.PinnedRepos
	if pinned == nil {
		pinned = []string{}
	}
	pinnedJSON, err := json.Marshal(pinned)
	if err != nil {
		return "", fmt.Errorf("marshal pinned_repos: %w", err)
	}

	// teamID: required, no synthesis. Projects are user-driven writes
	// and the human picks which team owns the project at the Create
	// UI (SKY-294). A "first of caller's teams" or "any team in org"
	// fallback would either silently attach to the wrong team or
	// collide with projects_insert RLS (tf.user_in_team(team_id)).
	// The handler ships runmode.LocalDefaultTeamID today; the
	// sentinel filter converts that to empty, which then trips the
	// explicit guard below. Post-D9, the handler threads a real team
	// from request context.
	teamBind := teamID
	if teamBind == runmode.LocalDefaultTeamID {
		teamBind = ""
	}
	if teamBind == "" {
		return "", fmt.Errorf("project store: team_id required for Postgres Create (handler must thread the user-selected team from request context; the SQLite-only LocalDefaultTeamID sentinel does not satisfy the projects_insert RLS policy)")
	}

	// creator_user_id: pulled from tf.current_user_id() set by WithTx
	// claims — same pattern every other app-pool store uses
	// (event_handlers, swipes, chains, tasks, prompts). Org-owner
	// fallback covers the pgtest admin-pool path (BYPASSRLS, no
	// claims set); in production multi-mode under tf_app, claims are
	// always set and the COALESCE stops at the first branch.
	_, err = s.q.ExecContext(ctx, `
		INSERT INTO projects
		  (id, org_id, creator_user_id, team_id, name, description,
		   curator_session_id, pinned_repos,
		   jira_project_key, linear_project_key, spec_authorship_blueprint_id)
		VALUES
		  ($1, $2,
		   COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
		   $3::uuid,
		   $4, $5, NULLIF($6, ''), $7::jsonb,
		   NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''))
	`,
		id, orgID, teamBind,
		p.Name, p.Description,
		p.CuratorSessionID, string(pinnedJSON),
		p.JiraProjectKey, p.LinearProjectKey, p.SpecAuthorshipBlueprintID,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *projectStore) Get(ctx context.Context, orgID, id string) (*domain.Project, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT id, name, description, curator_session_id, pinned_repos,
		       jira_project_key, linear_project_key, spec_authorship_blueprint_id,
		       team_id, created_at, updated_at
		FROM projects
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	return scanProjectRow(row)
}

func (s *projectStore) List(ctx context.Context, orgID string) ([]domain.Project, error) {
	return listProjects(ctx, s.q, orgID)
}

func (s *projectStore) ListSystem(ctx context.Context, orgID string) ([]domain.Project, error) {
	return listProjects(ctx, s.admin, orgID)
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

func listProjects(ctx context.Context, q queryer, orgID string) ([]domain.Project, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, name, description, curator_session_id, pinned_repos,
		       jira_project_key, linear_project_key, spec_authorship_blueprint_id,
		       team_id, created_at, updated_at
		FROM projects
		WHERE org_id = $1
		ORDER BY LOWER(name) ASC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Project{}
	for rows.Next() {
		p, err := scanProjectRow(rows)
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
	pinned := p.PinnedRepos
	if pinned == nil {
		pinned = []string{}
	}
	pinnedJSON, err := json.Marshal(pinned)
	if err != nil {
		return fmt.Errorf("marshal pinned_repos: %w", err)
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE projects
		SET name = $1, description = $2,
		    curator_session_id = NULLIF($3, ''),
		    pinned_repos = $4::jsonb,
		    jira_project_key = NULLIF($5, ''),
		    linear_project_key = NULLIF($6, ''),
		    spec_authorship_blueprint_id = NULLIF($7, ''),
		    updated_at = now()
		WHERE org_id = $8 AND id = $9
	`,
		p.Name, p.Description,
		p.CuratorSessionID, string(pinnedJSON),
		p.JiraProjectKey, p.LinearProjectKey, p.SpecAuthorshipBlueprintID,
		orgID, p.ID,
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

func (s *projectStore) SetCuratorSessionID(ctx context.Context, orgID, projectID, sessionID string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE projects
		SET curator_session_id = NULLIF($1, ''), updated_at = now()
		WHERE org_id = $2 AND id = $3
	`, sessionID, orgID, projectID)
	return err
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
		sessionID       sql.NullString
		jiraKey         sql.NullString
		linearKey       sql.NullString
		specBlueprintID sql.NullString
		teamID          sql.NullString
		pinnedJSON      []byte
	)
	err := row.Scan(
		&p.ID, &p.Name, &p.Description, &sessionID, &pinnedJSON,
		&jiraKey, &linearKey, &specBlueprintID,
		&teamID, &p.CreatedAt, &p.UpdatedAt,
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
	if len(pinnedJSON) == 0 {
		p.PinnedRepos = []string{}
	} else if err := json.Unmarshal(pinnedJSON, &p.PinnedRepos); err != nil {
		return nil, fmt.Errorf("unmarshal pinned_repos: %w", err)
	}
	if p.PinnedRepos == nil {
		p.PinnedRepos = []string{}
	}
	return &p, nil
}
