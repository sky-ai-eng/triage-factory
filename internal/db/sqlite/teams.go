package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamsStore is the SQLite impl of db.TeamsStore. Local mode seeds a
// single "default" team for runmode.LocalDefaultOrgID via the
// baseline migration; GetDefaultForOrgSystem returns its id by the
// "oldest row wins" rule. The settings methods read/write
// team_settings; SQLite stores jira_projects as a JSON text blob
// (no native array type).
//
// teamsStore — SQLite impl. The constructor accepts two queryers for
// signature parity with the Postgres impl; SQLite has one connection
// so both collapse to the same queryer. The `...System` variants
// delegate to their non-System counterparts.
type teamsStore struct{ q queryer }

func newTeamsStore(q, _ queryer) db.TeamsStore { return &teamsStore{q: q} }

var _ db.TeamsStore = (*teamsStore)(nil)

func (s *teamsStore) GetDefaultForOrg(ctx context.Context, orgID string) (string, error) {
	return getDefaultTeamForOrg(ctx, s.q, orgID)
}

func (s *teamsStore) GetDefaultForOrgSystem(ctx context.Context, orgID string) (string, error) {
	return getDefaultTeamForOrg(ctx, s.q, orgID)
}

func getDefaultTeamForOrg(ctx context.Context, q queryer, orgID string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT id FROM teams
		WHERE org_id = ?
		ORDER BY created_at ASC, id ASC
		LIMIT 1
	`, orgID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *teamsStore) GetSettings(ctx context.Context, teamID string) (domain.TeamSettings, error) {
	return getTeamSettings(ctx, s.q, teamID)
}

func (s *teamsStore) GetSettingsSystem(ctx context.Context, teamID string) (domain.TeamSettings, error) {
	return getTeamSettings(ctx, s.q, teamID)
}

func getTeamSettings(ctx context.Context, q queryer, teamID string) (domain.TeamSettings, error) {
	var (
		projectsJSON            string
		aiThreshold, aiInterval int
		defaultModel            string
		autoDelegate            bool
		permAbsentGraceMS       int
		permAbsentAutodeny      bool
	)
	err := q.QueryRowContext(ctx, `
		SELECT jira_projects, ai_reprioritize_threshold, ai_preference_update_interval,
		       default_model, auto_delegate_enabled,
		       permission_absent_grace_ms, permission_absent_autodeny_enabled
		FROM team_settings WHERE team_id = ?
	`, teamID).Scan(
		&projectsJSON, &aiThreshold, &aiInterval,
		&defaultModel, &autoDelegate,
		&permAbsentGraceMS, &permAbsentAutodeny,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// See OrgsStore for the rationale: defaults here are the
		// belt-and-suspenders fallback for test fixtures or any
		// caller that beats the provisioning path. Matches the
		// schema DEFAULT clauses on team_settings.
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
	}
	return domain.TeamSettings{
		JiraProjects:                    projects,
		AIReprioritizeThreshold:         aiThreshold,
		AIPreferenceUpdateInterval:      aiInterval,
		DefaultModel:                    defaultModel,
		AutoDelegateEnabled:             autoDelegate,
		PermissionAbsentGraceMS:         permAbsentGraceMS,
		PermissionAbsentAutodenyEnabled: permAbsentAutodeny,
	}, nil
}

func (s *teamsStore) ListForUser(ctx context.Context, orgID string) ([]domain.Team, error) {
	// SQLite is N=1: one synthetic user, so "the user's teams" is just
	// "every team in the org" — no memberships join needed (and the
	// local sentinel user isn't enrolled via memberships). The ordering
	// matches GetDefaultForOrg so teams[0] is the default team. Role is a
	// constant 'admin': the sole local user owns every team (mirrors the
	// local-mode short-circuit in requireTeamAdmin), so the settings
	// surface's admin gate reads true for all of them.
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id, slug, name, created_at, 'admin' AS role
		FROM teams
		WHERE org_id = ?
		ORDER BY created_at ASC, id ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list teams for user: %w", err)
	}
	defer rows.Close()
	out := []domain.Team{}
	for rows.Next() {
		var t domain.Team
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.CreatedAt, &t.Role); err != nil {
			return nil, fmt.Errorf("scan team: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *teamsStore) TeamIDsForUserInOrgSystem(ctx context.Context, orgID, userID string) ([]string, error) {
	// memberships ⋈ teams, scoped to the org — identical query to the
	// Postgres impl so both backends pin to one result. Unlike
	// ListForUser (which short-circuits the join because SQLite is N=1),
	// this takes an explicit userID and honors the membership rows, so
	// the baseline's seeded local-user membership is what
	// resolves the one synthetic user to its team in N=1.
	rows, err := s.q.QueryContext(ctx, `
		SELECT t.id
		FROM memberships m
		JOIN teams t ON t.id = m.team_id
		WHERE t.org_id = ? AND m.user_id = ?
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
	// Local mode never calls this in practice (the "add team" handler is
	// multi-mode-gated), but the impl exists for store-conformance parity.
	// SQLite teams.id has no DEFAULT, so generate it app-side.
	id := uuid.New().String()
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)
	`, id, orgID, slug, name); err != nil {
		return domain.Team{}, fmt.Errorf("insert team: %w", err)
	}
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'admin')
	`, creatorUserID, id); err != nil {
		return domain.Team{}, fmt.Errorf("enroll team creator: %w", err)
	}
	var t domain.Team
	if err := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, slug, name, created_at FROM teams WHERE id = ?
	`, id).Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.CreatedAt); err != nil {
		return domain.Team{}, fmt.Errorf("read created team: %w", err)
	}
	return t, nil
}

func (s *teamsStore) UpdateSettings(ctx context.Context, teamID string, u domain.TeamSettings) error {
	projectsJSON, err := marshalJSONArray(u.JiraProjects)
	if err != nil {
		return fmt.Errorf("marshal team_settings.jira_projects: %w", err)
	}
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO team_settings (
			team_id, jira_projects, ai_reprioritize_threshold,
			ai_preference_update_interval, default_model, auto_delegate_enabled,
			permission_absent_grace_ms, permission_absent_autodeny_enabled,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(team_id) DO UPDATE SET
			jira_projects = excluded.jira_projects,
			ai_reprioritize_threshold = excluded.ai_reprioritize_threshold,
			ai_preference_update_interval = excluded.ai_preference_update_interval,
			default_model = excluded.default_model,
			auto_delegate_enabled = excluded.auto_delegate_enabled,
			permission_absent_grace_ms = excluded.permission_absent_grace_ms,
			permission_absent_autodeny_enabled = excluded.permission_absent_autodeny_enabled,
			updated_at = CURRENT_TIMESTAMP
	`,
		teamID, projectsJSON, u.AIReprioritizeThreshold,
		u.AIPreferenceUpdateInterval, u.DefaultModel, u.AutoDelegateEnabled,
		u.PermissionAbsentGraceMS, u.PermissionAbsentAutodenyEnabled,
	); err != nil {
		return fmt.Errorf("upsert team_settings: %w", err)
	}
	return nil
}

// The roster methods are a multi-mode (Postgres) surface: the team-roster
// handlers 404 in local, and local mode keeps the synthetic single-member
// behavior in handleTeamMembers. These stubs exist only so the local store
// bundle satisfies the interface; reaching them at N=1 is a runmode-gate
// escape, and a clear error beats a misleading empty roster.
func (s *teamsStore) ListMembers(_ context.Context, _, _, _ string) ([]domain.TeamMember, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *teamsStore) AddMember(_ context.Context, _, _, _ string) error {
	return db.ErrNotApplicableInLocal
}

func (s *teamsStore) ChangeMemberRole(_ context.Context, _, _, _ string) error {
	return db.ErrNotApplicableInLocal
}

func (s *teamsStore) RemoveMember(_ context.Context, _, _ string) error {
	return db.ErrNotApplicableInLocal
}
