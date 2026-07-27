package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	// deleted_at IS NULL: an archived team is never the org's default (TFAC-448).
	// Harmless at N=1 (local mode never archives its sole team), kept for parity
	// with the Postgres impl.
	err := q.QueryRowContext(ctx, `
		SELECT id FROM teams
		WHERE org_id = ? AND deleted_at IS NULL
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
		maxDailyCost            sql.NullFloat64
		branchTemplate          string
		reviewPosture           string
		basePushPolicy          string
	)
	err := q.QueryRowContext(ctx, `
		SELECT jira_projects, ai_reprioritize_threshold, ai_preference_update_interval,
		       default_model, auto_delegate_enabled,
		       permission_absent_grace_ms, permission_absent_autodeny_enabled,
		       max_daily_cost_usd, branch_template, review_posture,
		       base_branch_push_policy
		FROM team_settings WHERE team_id = ?
	`, teamID).Scan(
		&projectsJSON, &aiThreshold, &aiInterval,
		&defaultModel, &autoDelegate,
		&permAbsentGraceMS, &permAbsentAutodeny,
		&maxDailyCost, &branchTemplate, &reviewPosture, &basePushPolicy,
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
		MaxDailyCostUSD:                 maxDailyCost.Float64, // NULL → 0 (no cap)
		BranchTemplate:                  branchTemplate,
		ReviewPosture:                   reviewPosture,
		BaseBranchPushPolicy:            basePushPolicy,
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
	// deleted_at IS NULL hides archived teams from the selectors (TFAC-448) —
	// the request-facing read filter, mirrored from the Postgres impl.
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id, slug, name, created_at, 'admin' AS role
		FROM teams
		WHERE org_id = ? AND deleted_at IS NULL
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
	// t.deleted_at IS NULL excludes archived teams (TFAC-448) — parity with the
	// Postgres impl; the router must not route new tasks to an archived team.
	rows, err := s.q.QueryContext(ctx, `
		SELECT t.id
		FROM memberships m
		JOIN teams t ON t.id = m.team_id
		WHERE t.org_id = ? AND m.user_id = ? AND t.deleted_at IS NULL
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

func (s *teamsStore) Update(ctx context.Context, teamID string, name, slug, description *string) (domain.Team, error) {
	// COALESCE applies each arg only when non-nil (the partial-PATCH
	// contract); an empty-string description clears the blurb. Mirrors the
	// Postgres impl, but reads the row back with a follow-up SELECT rather
	// than RETURNING to match the Create pattern in this file. A zero-row
	// UPDATE means no such team → ErrTeamNotFound (a clean 404).
	res, err := s.q.ExecContext(ctx, `
		UPDATE teams SET
			name = COALESCE(?, name),
			slug = COALESCE(?, slug),
			description = COALESCE(?, description),
			updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, name, slug, description, teamID)
	if err != nil {
		// UNIQUE (org_id, slug) collision passes through verbatim — the
		// handler maps "UNIQUE constraint" to a 409, the same as Create.
		return domain.Team{}, fmt.Errorf("update team: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return domain.Team{}, fmt.Errorf("update team: rows affected: %w", err)
	}
	if n == 0 {
		return domain.Team{}, db.ErrTeamNotFound
	}
	var t domain.Team
	if err := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, slug, name, COALESCE(description, ''), created_at
		FROM teams WHERE id = ?
	`, teamID).Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.Description, &t.CreatedAt); err != nil {
		return domain.Team{}, fmt.Errorf("read updated team: %w", err)
	}
	return t, nil
}

func (s *teamsStore) Archive(ctx context.Context, teamID string) error {
	return setTeamArchived(ctx, s.q, teamID, true)
}

func (s *teamsStore) Restore(ctx context.Context, teamID string) error {
	return setTeamArchived(ctx, s.q, teamID, false)
}

// setTeamArchived flips teams.deleted_at, mirroring the Postgres impl: archive
// stamps CURRENT_TIMESTAMP WHERE deleted_at IS NULL, restore clears it WHERE
// deleted_at IS NOT NULL. The state-guarded WHERE makes a no-op flip affect zero
// rows → ErrTeamNotFound (already archived / already active / missing).
func setTeamArchived(ctx context.Context, q queryer, teamID string, archive bool) error {
	var (
		res sql.Result
		err error
	)
	if archive {
		res, err = q.ExecContext(ctx, `
			UPDATE teams SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND deleted_at IS NULL
		`, teamID)
	} else {
		res, err = q.ExecContext(ctx, `
			UPDATE teams SET deleted_at = NULL, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND deleted_at IS NOT NULL
		`, teamID)
	}
	if err != nil {
		return fmt.Errorf("set team archived=%v: %w", archive, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set team archived: rows affected: %w", err)
	}
	if n == 0 {
		return db.ErrTeamNotFound
	}
	return nil
}

func (s *teamsStore) GetSystem(ctx context.Context, orgID, teamID string) (*domain.Team, error) {
	var (
		t       domain.Team
		deleted sql.NullTime
	)
	err := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, slug, name, COALESCE(description, ''), created_at, deleted_at
		FROM teams WHERE id = ? AND org_id = ?
	`, teamID, orgID).Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.Description, &t.CreatedAt, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get team system: %w", err)
	}
	if deleted.Valid {
		t.DeletedAt = &deleted.Time
	}
	return &t, nil
}

func (s *teamsStore) ListArchivedForOrgSystem(ctx context.Context, orgID string) ([]domain.Team, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id, slug, name, COALESCE(description, ''), created_at, deleted_at
		FROM teams
		WHERE org_id = ? AND deleted_at IS NOT NULL
		ORDER BY deleted_at DESC, id ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list archived teams: %w", err)
	}
	defer rows.Close()
	out := []domain.Team{}
	for rows.Next() {
		var (
			t       domain.Team
			deleted sql.NullTime
		)
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.Description, &t.CreatedAt, &deleted); err != nil {
			return nil, fmt.Errorf("scan archived team: %w", err)
		}
		if deleted.Valid {
			t.DeletedAt = &deleted.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *teamsStore) ListActiveForOrgSystem(ctx context.Context, orgID string) ([]domain.Team, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id, slug, name, COALESCE(description, ''), created_at
		FROM teams
		WHERE org_id = ? AND deleted_at IS NULL
		ORDER BY name ASC, id ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list active teams: %w", err)
	}
	defer rows.Close()
	out := []domain.Team{}
	for rows.Next() {
		var t domain.Team
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.Description, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan active team: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *teamsStore) NamesForIDsSystem(ctx context.Context, orgID string, teamIDs []string) (map[string]string, error) {
	seen := make(map[string]bool, len(teamIDs))
	ids := make([]string, 0, len(teamIDs))
	for _, id := range teamIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, orgID)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, name FROM teams WHERE org_id = ? AND id IN (`+strings.Join(placeholders, ",")+`)
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("names for ids teams: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan team name: %w", err)
		}
		out[id] = name
	}
	return out, rows.Err()
}

func (s *teamsStore) UpdateSettings(ctx context.Context, teamID string, u domain.TeamSettings) error {
	projectsJSON, err := marshalJSONArray(u.JiraProjects)
	if err != nil {
		return fmt.Errorf("marshal team_settings.jira_projects: %w", err)
	}
	// max_daily_cost_usd rides along (0 → NULL via nullFloatValue) so a read-
	// modify-write team-settings save round-trips the org-admin-set cap untouched.
	// The team-settings handler never populates this field from its request body
	// (a team admin cannot set their own cap), so its GetSettings→UpdateSettings
	// flow always writes back exactly what it read. The cap is *changed* only by
	// the org-admin SetDailyCostCapSystem path.
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO team_settings (
			team_id, jira_projects, ai_reprioritize_threshold,
			ai_preference_update_interval, default_model, auto_delegate_enabled,
			permission_absent_grace_ms, permission_absent_autodeny_enabled,
			max_daily_cost_usd, branch_template, review_posture,
			base_branch_push_policy, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(team_id) DO UPDATE SET
			jira_projects = excluded.jira_projects,
			ai_reprioritize_threshold = excluded.ai_reprioritize_threshold,
			ai_preference_update_interval = excluded.ai_preference_update_interval,
			default_model = excluded.default_model,
			auto_delegate_enabled = excluded.auto_delegate_enabled,
			permission_absent_grace_ms = excluded.permission_absent_grace_ms,
			permission_absent_autodeny_enabled = excluded.permission_absent_autodeny_enabled,
			max_daily_cost_usd = excluded.max_daily_cost_usd,
			branch_template = excluded.branch_template,
			review_posture = excluded.review_posture,
			base_branch_push_policy = excluded.base_branch_push_policy,
			updated_at = CURRENT_TIMESTAMP
	`,
		teamID, projectsJSON, u.AIReprioritizeThreshold,
		u.AIPreferenceUpdateInterval, u.DefaultModel, u.AutoDelegateEnabled,
		u.PermissionAbsentGraceMS, u.PermissionAbsentAutodenyEnabled,
		nullFloatValue(u.MaxDailyCostUSD), u.BranchTemplate, u.ReviewPosture,
		u.BaseBranchPushPolicy,
	); err != nil {
		return fmt.Errorf("upsert team_settings: %w", err)
	}
	return nil
}

// SetDailyCostCapSystem upserts ONLY team_settings.max_daily_cost_usd for teamID
// — the org-admin per-team daily spend cap (TFAC-482). SQLite is N=1 / no RLS so
// it's a plain write; the Postgres twin runs it on the admin pool because an org
// admin setting a team's cap may not be a member of that team. capUSD ≤ 0 stores
// SQL NULL ("no cap"). The partial INSERT relies on the schema DEFAULT clauses
// for every other team_settings column when no row exists yet, and ON CONFLICT
// touches only the cap so a team's other settings are never clobbered.
func (s *teamsStore) SetDailyCostCapSystem(ctx context.Context, teamID string, capUSD float64) error {
	// ≤ 0 → SQL NULL ("no cap"); any positive value is the cap.
	var capArg any
	if capUSD > 0 {
		capArg = capUSD
	}
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO team_settings (team_id, max_daily_cost_usd, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(team_id) DO UPDATE SET
			max_daily_cost_usd = excluded.max_daily_cost_usd,
			updated_at = CURRENT_TIMESTAMP
	`, teamID, capArg); err != nil {
		return fmt.Errorf("set team daily cost cap: %w", err)
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

func (s *teamsStore) ChangeMemberRole(_ context.Context, _, _, _ string) (string, error) {
	return "", db.ErrNotApplicableInLocal
}

func (s *teamsStore) RemoveMember(_ context.Context, _, _ string) error {
	return db.ErrNotApplicableInLocal
}
