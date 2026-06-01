package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamsStore is the Postgres impl of db.TeamsStore. Holds both pools —
// see the TeamsStore interface comment for the pool-split rationale.
//
//   - admin: GetDefaultForOrgSystem, GetSettingsSystem. Boot-time
//     pollers/scorer/delegation spawner without a JWT-claims context.
//   - app: GetSettings, UpdateSettings. Request-handler reads/writes
//     gated by the team_settings_select / team_settings_update RLS
//     policies (team membership / team admin).
type teamsStore struct {
	app   queryer
	admin queryer
}

func newTeamsStore(app, admin queryer) db.TeamsStore {
	return &teamsStore{app: app, admin: admin}
}

var _ db.TeamsStore = (*teamsStore)(nil)

func (s *teamsStore) GetDefaultForOrg(ctx context.Context, orgID string) (string, error) {
	return queryDefaultTeam(ctx, s.app, orgID)
}

func (s *teamsStore) GetDefaultForOrgSystem(ctx context.Context, orgID string) (string, error) {
	return queryDefaultTeam(ctx, s.admin, orgID)
}

func queryDefaultTeam(ctx context.Context, q queryer, orgID string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT id::text FROM teams
		WHERE org_id = $1
		ORDER BY created_at ASC, id ASC
		LIMIT 1
	`, orgID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *teamsStore) GetSettings(ctx context.Context, teamID string) (domain.TeamSettings, error) {
	return getTeamSettings(ctx, s.app, teamID)
}

func (s *teamsStore) GetSettingsSystem(ctx context.Context, teamID string) (domain.TeamSettings, error) {
	return getTeamSettings(ctx, s.admin, teamID)
}

func getTeamSettings(ctx context.Context, q queryer, teamID string) (domain.TeamSettings, error) {
	var (
		projectsJSON            string
		aiThreshold, aiInterval int
		defaultModel            string
		autoDelegate            bool
	)
	// array_to_json(...)::text round-trips text[] as a JSON literal.
	// database/sql + pgx stdlib doesn't ship a scanner for *[]string,
	// so the JSON detour is the portable shape — matches what the
	// jira_project_status_rules reader does for its array columns.
	err := q.QueryRowContext(ctx, `
		SELECT array_to_json(jira_projects)::text,
		       ai_reprioritize_threshold, ai_preference_update_interval,
		       default_model, auto_delegate_enabled
		FROM team_settings WHERE team_id = $1
	`, teamID).Scan(
		&projectsJSON, &aiThreshold, &aiInterval,
		&defaultModel, &autoDelegate,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// See OrgsStore for the rationale. Matches team_settings'
		// schema DEFAULT clauses.
		return domain.DefaultTeamSettings(), nil
	}
	if err != nil {
		return domain.TeamSettings{}, fmt.Errorf("read team_settings: %w", err)
	}
	projects := []string{}
	if projectsJSON != "" {
		if err := json.Unmarshal([]byte(projectsJSON), &projects); err != nil {
			return domain.TeamSettings{}, fmt.Errorf("unmarshal team_settings.jira_projects: %w", err)
		}
		if projects == nil {
			projects = []string{}
		}
	}
	return domain.TeamSettings{
		JiraProjects:               projects,
		AIReprioritizeThreshold:    aiThreshold,
		AIPreferenceUpdateInterval: aiInterval,
		DefaultModel:               defaultModel,
		AutoDelegateEnabled:        autoDelegate,
	}, nil
}

func (s *teamsStore) ListForUser(ctx context.Context, orgID string) ([]domain.Team, error) {
	// teams_select RLS gates on org access, not team membership, so it
	// returns every team in the org. The memberships join is what
	// narrows to the caller's own teams — memberships_select lets a user
	// read their own rows (user_id = current_user_id()), so the join is
	// RLS-safe and self-scoping without an explicit user_id parameter.
	rows, err := s.app.QueryContext(ctx, `
		SELECT t.id::text, t.org_id::text, t.slug, t.name, t.created_at
		FROM teams t
		JOIN memberships m ON m.team_id = t.id AND m.user_id = tf.current_user_id()
		WHERE t.org_id = $1
		ORDER BY t.created_at ASC, t.id ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list teams for user: %w", err)
	}
	defer rows.Close()
	return scanTeams(rows)
}

func (s *teamsStore) TeamIDsForUserInOrgSystem(ctx context.Context, orgID, userID string) ([]string, error) {
	// memberships ⋈ teams, scoped to the org. The join to teams is what
	// applies the org filter (memberships carries no org_id — it FKs to
	// teams), and it also drops any dangling membership row whose team
	// has since been deleted. Admin pool: the router resolves an
	// arbitrary author/reviewer userID with no JWT claims, so this can't
	// lean on current_user_id() the way ListForUser does.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT t.id::text
		FROM memberships m
		JOIN teams t ON t.id = m.team_id
		WHERE t.org_id = $1 AND m.user_id = $2
		ORDER BY t.id ASC
	`, orgID, userID)
	if err != nil {
		return nil, fmt.Errorf("list teams for user in org: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan team_id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *teamsStore) Create(ctx context.Context, orgID, name, slug, creatorUserID string) (domain.Team, error) {
	var t domain.Team
	err := s.app.QueryRowContext(ctx, `
		INSERT INTO teams (org_id, slug, name)
		VALUES ($1, $2, $3)
		RETURNING id::text, org_id::text, slug, name, created_at
	`, orgID, slug, name).Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.CreatedAt)
	if err != nil {
		return domain.Team{}, fmt.Errorf("insert team: %w", err)
	}
	// Enroll the creator so the new team shows up in their ListForUser
	// set (and so they can write to it). Admin role mirrors the founder
	// role org provisioning grants on the default team.
	if _, err := s.app.ExecContext(ctx, `
		INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')
	`, creatorUserID, t.ID); err != nil {
		return domain.Team{}, fmt.Errorf("enroll team creator: %w", err)
	}
	return t, nil
}

func scanTeams(rows *sql.Rows) ([]domain.Team, error) {
	out := []domain.Team{}
	for rows.Next() {
		var t domain.Team
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan team: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *teamsStore) UpdateSettings(ctx context.Context, teamID string, u domain.TeamSettings) error {
	projects := u.JiraProjects
	if projects == nil {
		projects = []string{}
	}
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO team_settings (
			team_id, jira_projects, ai_reprioritize_threshold,
			ai_preference_update_interval, default_model, auto_delegate_enabled,
			updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (team_id) DO UPDATE SET
			jira_projects = EXCLUDED.jira_projects,
			ai_reprioritize_threshold = EXCLUDED.ai_reprioritize_threshold,
			ai_preference_update_interval = EXCLUDED.ai_preference_update_interval,
			default_model = EXCLUDED.default_model,
			auto_delegate_enabled = EXCLUDED.auto_delegate_enabled,
			updated_at = now()
	`,
		teamID, projects, u.AIReprioritizeThreshold,
		u.AIPreferenceUpdateInterval, u.DefaultModel, u.AutoDelegateEnabled,
	)
	if err != nil {
		return fmt.Errorf("upsert team_settings: %w", err)
	}
	return nil
}
