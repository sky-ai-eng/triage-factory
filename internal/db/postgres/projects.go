package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
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
	_, err = s.q.ExecContext(ctx, `
		INSERT INTO projects
		  (id, org_id, creator_user_id, team_id, visibility, name, description,
		   curator_session_id, pinned_repos,
		   jira_project_key, linear_project_key, spec_authorship_blueprint_id)
		VALUES
		  ($1, $2,
		   COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
		   NULLIF($3, '')::uuid,
		   $4,
		   $5, $6, NULLIF($7, ''), $8::jsonb,
		   NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, ''))
	`,
		id, orgID, teamBind, visibility,
		p.Name, p.Description,
		p.CuratorSessionID, string(pinnedJSON),
		p.JiraProjectKey, p.LinearProjectKey, p.SpecAuthorshipBlueprintID,
	)
	if err != nil {
		return "", translateVisibilityErr(err)
	}
	return id, nil
}

func (s *projectStore) Get(ctx context.Context, orgID, id string) (*domain.Project, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT id, name, description, curator_session_id, pinned_repos,
		       jira_project_key, linear_project_key, spec_authorship_blueprint_id,
		       team_id, visibility, creator_user_id, created_at, updated_at
		FROM projects
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	return scanProjectRow(row)
}

// GetSystem is the admin-pool (BYPASSRLS) variant of Get — a JWT-less
// background job resolving one project by id under an explicit orgID. See the
// interface doc.
func (s *projectStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Project, error) {
	row := s.admin.QueryRowContext(ctx, `
		SELECT id, name, description, curator_session_id, pinned_repos,
		       jira_project_key, linear_project_key, spec_authorship_blueprint_id,
		       team_id, visibility, creator_user_id, created_at, updated_at
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
		       team_id, visibility, creator_user_id, created_at, updated_at
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
		    visibility = $8,
		    updated_at = now()
		WHERE org_id = $9 AND id = $10
	`,
		p.Name, p.Description,
		p.CuratorSessionID, string(pinnedJSON),
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
		&teamID, &p.Visibility, &p.CreatorUserID, &p.CreatedAt, &p.UpdatedAt,
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
