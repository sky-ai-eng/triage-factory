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
	set, err := scanTeamSettings(q.QueryRowContext(ctx, `
		SELECT `+teamSettingsColumns+`
		FROM team_settings WHERE team_id = ?
	`, teamID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		// See OrgsStore for the rationale: defaults here are the
		// belt-and-suspenders fallback for test fixtures or any
		// caller that beats the provisioning path. Matches the
		// schema DEFAULT clauses on team_settings.
		return domain.DefaultTeamSettingsFor(false), nil
	}
	if err != nil {
		return domain.TeamSettings{}, fmt.Errorf("read team_settings: %w", err)
	}
	return set, nil
}

// teamSettingsColumns is the canonical projection of a team_settings row, in
// the order scanTeamSettings reads them. Both reads SELECT it and both writes
// RETURN it, so the write shape cannot drift from the read shape.
const teamSettingsColumns = `jira_projects, ai_reprioritize_threshold, ai_preference_update_interval,
		       default_model, auto_delegate_enabled, auto_mode_enabled,
		       permission_absent_grace_ms, permission_absent_autodeny_enabled,
		       max_daily_cost_usd, branch_template, review_posture,
		       base_branch_push_policy, enabled_models, linear_teams`

// scanTeamSettings decodes one team_settings row in teamSettingsColumns order.
func scanTeamSettings(scan func(...any) error) (domain.TeamSettings, error) {
	var (
		projectsJSON            string
		aiThreshold, aiInterval int
		defaultModel            string
		autoDelegate            bool
		autoMode                bool
		permAbsentGraceMS       int
		permAbsentAutodeny      bool
		maxDailyCost            sql.NullFloat64
		branchTemplate          string
		reviewPosture           string
		basePushPolicy          string
		enabledModels           sql.NullString
		linearTeamsJSON         string
	)
	if err := scan(
		&projectsJSON, &aiThreshold, &aiInterval,
		&defaultModel, &autoDelegate, &autoMode,
		&permAbsentGraceMS, &permAbsentAutodeny,
		&maxDailyCost, &branchTemplate, &reviewPosture, &basePushPolicy,
		&enabledModels, &linearTeamsJSON,
	); err != nil {
		return domain.TeamSettings{}, err
	}
	projects := []string{}
	if projectsJSON != "" {
		if err := json.Unmarshal([]byte(projectsJSON), &projects); err != nil {
			return domain.TeamSettings{}, fmt.Errorf("unmarshal team_settings.jira_projects: %w", err)
		}
	}
	enabled, err := db.UnmarshalModelSetColumn(enabledModels, "team_settings.enabled_models")
	if err != nil {
		return domain.TeamSettings{}, err
	}
	linearTeams := []string{}
	if linearTeamsJSON != "" {
		if err := json.Unmarshal([]byte(linearTeamsJSON), &linearTeams); err != nil {
			return domain.TeamSettings{}, fmt.Errorf("unmarshal team_settings.linear_teams: %w", err)
		}
	}
	return domain.TeamSettings{
		JiraProjects:                    projects,
		LinearTeams:                     linearTeams,
		AIReprioritizeThreshold:         aiThreshold,
		AIPreferenceUpdateInterval:      aiInterval,
		DefaultModel:                    defaultModel,
		AutoDelegateEnabled:             autoDelegate,
		AutoModeEnabled:                 autoMode,
		PermissionAbsentGraceMS:         permAbsentGraceMS,
		PermissionAbsentAutodenyEnabled: permAbsentAutodeny,
		MaxDailyCostUSD:                 maxDailyCost.Float64, // NULL → 0 (no cap)
		BranchTemplate:                  branchTemplate,
		ReviewPosture:                   reviewPosture,
		BaseBranchPushPolicy:            basePushPolicy,
		EnabledModels:                   enabled,
	}, nil
}

func (s *teamsStore) ListForUser(ctx context.Context, orgID string, opts db.ListOpts) ([]domain.Team, int, error) {
	// SQLite is N=1: one synthetic user, so "the user's teams" is just
	// "every team in the org" — no memberships join needed, because the sole
	// user is the only person any team could belong to. The ordering
	// matches GetDefaultForOrg so teams[0] is the default team. Role is a
	// constant 'admin': the sole local user owns every team (mirrors the
	// local-mode short-circuit in requireTeamAdmin), so the settings
	// surface's admin gate reads true for all of them.
	// deleted_at IS NULL hides archived teams from the selectors (TFAC-448) —
	// the request-facing read filter, mirrored from the Postgres impl.
	var total int
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM teams WHERE org_id = ? AND deleted_at IS NULL
	`, orgID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count teams for user: %w", err)
	}
	if opts.CountOnly {
		return []domain.Team{}, total, nil
	}

	query := `
		SELECT id, org_id, slug, name, COALESCE(description, ''), created_at, 'admin' AS role
		FROM teams
		WHERE org_id = ? AND deleted_at IS NULL
		ORDER BY created_at ASC, id ASC`
	args := []any{orgID}
	if opts.Limit > 0 {
		query += `
		LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list teams for user: %w", err)
	}
	defer rows.Close()
	out := []domain.Team{}
	for rows.Next() {
		var t domain.Team
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.Description, &t.CreatedAt, &t.Role); err != nil {
			return nil, 0, fmt.Errorf("scan team: %w", err)
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// Get is the canonical single read. Local is N=1, so the sole user admins
// every team — the constant role matches ListForUser's.
// teamRowColumns is the canonical projection of a teams ROW, in the order
// scanTeamRow reads them. Get SELECTs it alongside the caller's membership
// role, and the archive/restore writes RETURN it — role is not in the list
// because it is not a column of this table: it is the caller's own membership,
// which Get resolves separately and a write has no view on.
const teamRowColumns = `id, org_id, slug, name, COALESCE(description, ''), created_at, deleted_at`

func scanTeamRow(scan func(...any) error) (domain.Team, error) {
	var (
		t       domain.Team
		deleted sql.NullTime
	)
	if err := scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.Description, &t.CreatedAt, &deleted); err != nil {
		return domain.Team{}, err
	}
	if deleted.Valid {
		t.DeletedAt = &deleted.Time
	}
	return t, nil
}

func (s *teamsStore) Get(ctx context.Context, orgID, teamID string) (*domain.Team, error) {
	t, err := scanTeamRow(s.q.QueryRowContext(ctx, `
		SELECT `+teamRowColumns+`
		FROM teams
		WHERE id = ? AND org_id = ?
	`, teamID, orgID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get team: %w", err)
	}
	// SQLite is N=1: the sole local user owns every team.
	t.Role = "admin"
	return &t, nil
}

// MemberIDsSystem — identical query to the Postgres impl so both backends pin
// to one result. Local mode never emits the per-user fan-out this feeds (N=1
// broadcasts org-wide), but the read is real rather than a stub so the shared
// conformance suite pins one answer.
func (s *teamsStore) MemberIDsSystem(ctx context.Context, orgID, teamID string) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT m.user_id
		FROM memberships m
		JOIN teams t ON t.id = m.team_id
		WHERE t.org_id = ? AND m.team_id = ?
		ORDER BY 1 ASC
	`, orgID, teamID)
	if err != nil {
		return nil, fmt.Errorf("list team member ids: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan member user_id: %w", err)
		}
		out = append(out, id)
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

func (s *teamsStore) Archive(ctx context.Context, teamID string) (domain.Team, error) {
	return archiveTeam(ctx, s.q, teamID)
}

func (s *teamsStore) Restore(ctx context.Context, teamID string) (domain.Team, error) {
	return restoreTeam(ctx, s.q, teamID)
}

// archiveTeam stamps CURRENT_TIMESTAMP on an active team's deleted_at unless no
// other active team would be left in its org, mirroring the Postgres impl. One
// statement carries the guard, and SQLite serializes writers, so there is no
// concurrent archive to lock out.
func archiveTeam(ctx context.Context, q queryer, teamID string) (domain.Team, error) {
	t, err := scanTeamRow(q.QueryRowContext(ctx, `
		UPDATE teams SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND deleted_at IS NULL
		   AND EXISTS (
		       SELECT 1 FROM teams o
		        WHERE o.org_id = teams.org_id AND o.id <> teams.id AND o.deleted_at IS NULL)
		RETURNING `+teamRowColumns, teamID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing archived: a team still active is its org's last active team;
		// otherwise it is archived already or missing.
		var active bool
		if err := q.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM teams WHERE id = ? AND deleted_at IS NULL)`, teamID,
		).Scan(&active); err != nil {
			return domain.Team{}, fmt.Errorf("read team state after refused archive: %w", err)
		}
		if active {
			return domain.Team{}, db.ErrLastActiveTeam
		}
		return domain.Team{}, db.ErrTeamNotFound
	}
	if err != nil {
		return domain.Team{}, fmt.Errorf("archive team: %w", err)
	}
	return t, nil
}

// restoreTeam clears an archived team's deleted_at. The state-guarded WHERE
// makes restoring a live team match nothing, which is db.ErrTeamNotFound, as is
// a missing team.
func restoreTeam(ctx context.Context, q queryer, teamID string) (domain.Team, error) {
	t, err := scanTeamRow(q.QueryRowContext(ctx, `
		UPDATE teams SET deleted_at = NULL, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND deleted_at IS NOT NULL
		RETURNING `+teamRowColumns, teamID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Team{}, db.ErrTeamNotFound
	}
	if err != nil {
		return domain.Team{}, fmt.Errorf("restore team: %w", err)
	}
	return t, nil
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

func (s *teamsStore) ListArchivedForOrgSystem(ctx context.Context, orgID string, opts db.ListOpts) ([]domain.Team, int, error) {
	var total int
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM teams WHERE org_id = ? AND deleted_at IS NOT NULL
	`, orgID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count archived teams: %w", err)
	}
	if opts.CountOnly {
		return []domain.Team{}, total, nil
	}

	query := `
		SELECT id, org_id, slug, name, COALESCE(description, ''), created_at, deleted_at
		FROM teams
		WHERE org_id = ? AND deleted_at IS NOT NULL
		ORDER BY deleted_at DESC, id ASC`
	args := []any{orgID}
	if opts.Limit > 0 {
		query += `
		LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list archived teams: %w", err)
	}
	defer rows.Close()
	out := []domain.Team{}
	for rows.Next() {
		var (
			t       domain.Team
			deleted sql.NullTime
		)
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.Description, &t.CreatedAt, &deleted); err != nil {
			return nil, 0, fmt.Errorf("scan archived team: %w", err)
		}
		if deleted.Valid {
			t.DeletedAt = &deleted.Time
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
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

func (s *teamsStore) ListActiveCapsForOrgSystem(ctx context.Context, orgID string, opts db.ListOpts) ([]domain.TeamCap, int, error) {
	var total int
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM teams WHERE org_id = ? AND deleted_at IS NULL
	`, orgID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count active teams: %w", err)
	}
	if opts.CountOnly {
		return []domain.TeamCap{}, total, nil
	}
	// LEFT JOIN, not JOIN: a team with no settings row has no cap, and an
	// inner join would silently drop it from the editor that exists to give
	// it one.
	query := `
		SELECT t.id, t.name, ts.max_daily_cost_usd
		FROM teams t
		LEFT JOIN team_settings ts ON ts.team_id = t.id
		WHERE t.org_id = ? AND t.deleted_at IS NULL
		ORDER BY t.name ASC, t.id ASC`
	args := []any{orgID}
	if opts.Limit > 0 {
		query += `
		LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list team caps: %w", err)
	}
	defer rows.Close()
	out := []domain.TeamCap{}
	for rows.Next() {
		var (
			c   domain.TeamCap
			cap sql.NullFloat64
		)
		if err := rows.Scan(&c.TeamID, &c.TeamName, &cap); err != nil {
			return nil, 0, fmt.Errorf("scan team cap: %w", err)
		}
		if cap.Valid && cap.Float64 > 0 {
			v := cap.Float64
			c.Cap = &v
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
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

func (s *teamsStore) UpdateSettings(ctx context.Context, teamID string, u domain.TeamSettings) (domain.TeamSettings, error) {
	// jira_projects, linear_teams and max_daily_cost_usd are absent from both
	// arms: they belong to other writers (see the interface), and an insert
	// takes their schema defaults.
	stored, err := scanTeamSettings(s.q.QueryRowContext(ctx, `
		INSERT INTO team_settings (
			team_id, ai_reprioritize_threshold,
			ai_preference_update_interval, default_model, auto_delegate_enabled,
			auto_mode_enabled,
			permission_absent_grace_ms, permission_absent_autodeny_enabled,
			branch_template, review_posture,
			base_branch_push_policy, enabled_models, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(team_id) DO UPDATE SET
			ai_reprioritize_threshold = excluded.ai_reprioritize_threshold,
			ai_preference_update_interval = excluded.ai_preference_update_interval,
			default_model = excluded.default_model,
			auto_delegate_enabled = excluded.auto_delegate_enabled,
			auto_mode_enabled = excluded.auto_mode_enabled,
			permission_absent_grace_ms = excluded.permission_absent_grace_ms,
			permission_absent_autodeny_enabled = excluded.permission_absent_autodeny_enabled,
			branch_template = excluded.branch_template,
			review_posture = excluded.review_posture,
			base_branch_push_policy = excluded.base_branch_push_policy,
			enabled_models = excluded.enabled_models,
			updated_at = CURRENT_TIMESTAMP
		RETURNING `+teamSettingsColumns,
		teamID, u.AIReprioritizeThreshold,
		u.AIPreferenceUpdateInterval, u.DefaultModel, u.AutoDelegateEnabled,
		u.AutoModeEnabled,
		u.PermissionAbsentGraceMS, u.PermissionAbsentAutodenyEnabled,
		u.BranchTemplate, u.ReviewPosture,
		u.BaseBranchPushPolicy, db.ModelSetColumnValue(u.EnabledModels),
	).Scan)
	if err != nil {
		return domain.TeamSettings{}, fmt.Errorf("upsert team_settings: %w", err)
	}
	return stored, nil
}

func (s *teamsStore) SetJiraProjectOrder(ctx context.Context, teamID string, jiraProjectIDs []string) (domain.TeamSettings, error) {
	return setTeamOrderColumn(ctx, s.q, teamID, "jira_projects", jiraProjectIDs)
}

func (s *teamsStore) SetLinearTeamOrder(ctx context.Context, teamID string, linearTeamIDs []string) (domain.TeamSettings, error) {
	return setTeamOrderColumn(ctx, s.q, teamID, "linear_teams", linearTeamIDs)
}

// setTeamOrderColumn upserts one of team_settings' two display-order columns,
// named by its two callers, as a JSON array, and returns the whole row: an
// insert takes every other column's schema default, so the row it lands in is
// one the caller has never seen.
func setTeamOrderColumn(ctx context.Context, q queryer, teamID, column string, ids []string) (domain.TeamSettings, error) {
	idsJSON, err := marshalJSONArray(ids)
	if err != nil {
		return domain.TeamSettings{}, fmt.Errorf("marshal team_settings.%s: %w", column, err)
	}
	stored, err := scanTeamSettings(q.QueryRowContext(ctx, `
		INSERT INTO team_settings (team_id, `+column+`, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(team_id) DO UPDATE SET
			`+column+` = excluded.`+column+`,
			updated_at = CURRENT_TIMESTAMP
		RETURNING `+teamSettingsColumns,
		teamID, idsJSON).Scan)
	if err != nil {
		return domain.TeamSettings{}, fmt.Errorf("upsert team_settings.%s: %w", column, err)
	}
	return stored, nil
}

// LockSettings takes nothing inside a transaction: the handle begins every
// write transaction IMMEDIATE, so a writer already holds the database's write
// lock from BEGIN to commit, which serializes it against every other. Outside
// one nothing would serialize the caller's read and write, so it refuses, as
// the Postgres impl does.
func (s *teamsStore) LockSettings(context.Context, string) error {
	if _, ok := s.q.(*sql.Tx); !ok {
		return errors.New("sqlite: LockSettings needs a transaction's stores, whose transaction serializes the caller")
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
func (s *teamsStore) SetDailyCostCapSystem(ctx context.Context, teamID string, capUSD float64) (domain.TeamSettings, error) {
	// ≤ 0 → SQL NULL ("no cap"); any positive value is the cap.
	var capArg any
	if capUSD > 0 {
		capArg = capUSD
	}
	// RETURNING projects the whole settings row: the insert arm fills every
	// other column from schema defaults, so the row this lands in is one the
	// caller has never seen.
	stored, err := scanTeamSettings(s.q.QueryRowContext(ctx, `
		INSERT INTO team_settings (team_id, max_daily_cost_usd, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(team_id) DO UPDATE SET
			max_daily_cost_usd = excluded.max_daily_cost_usd,
			updated_at = CURRENT_TIMESTAMP
		RETURNING `+teamSettingsColumns,
		teamID, capArg).Scan)
	if err != nil {
		return domain.TeamSettings{}, fmt.Errorf("set team daily cost cap: %w", err)
	}
	return stored, nil
}

// ListMembers answers the same roster read the Postgres impl does, off the
// same tables. Local mode is N=1 — the tenant seed enrolls the one implicit
// user on the sole team with role 'admin' — so this returns exactly that
// single member, with whatever GitHub / Jira binding they hold on the org's
// hosts and Linear binding they hold in its workspace. It is a real query
// rather than a synthesized row because the membership row genuinely exists:
// TeamIDsForUserInOrgSystem already resolves the local user through it.
//
// The roster's consumers run here too — the assignee picker, the predicate
// editor's variant choice — so this is a read local mode needs answered, not
// a stub for interface parity. The WRITES below are the local refusals:
// enrolling a second member is the thing N=1 has no concept of.
func (s *teamsStore) ListMembers(ctx context.Context, teamID, githubBaseURL, jiraBaseURL, linearWorkspaceID string, opts db.ListOpts) ([]domain.TeamMember, int, error) {
	// The filtered total, on the same FROM as the page below so the count
	// can't describe a different set than the rows it accompanies.
	var total int
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.team_id = ?
	`, teamID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count team members: %w", err)
	}
	if opts.CountOnly {
		return []domain.TeamMember{}, total, nil
	}

	// display_name is nullable, so COALESCE renders the empty string (matching
	// GetDisplayName's ""). The ORDER BY ends in m.user_id so offset paging
	// has a total order to walk — mirroring the Postgres impl, which is what
	// the shared conformance suite pins.
	query := `
		SELECT m.user_id, COALESCE(u.display_name, ''), m.role
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.team_id = ?
		ORDER BY COALESCE(u.display_name, ''), m.user_id`
	args := []any{teamID}
	if opts.Limit > 0 {
		query += `
		LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list team members: %w", err)
	}
	defer rows.Close()
	out := []domain.TeamMember{}
	for rows.Next() {
		var m domain.TeamMember
		if err := rows.Scan(&m.UserID, &m.DisplayName, &m.Role); err != nil {
			return nil, 0, fmt.Errorf("scan team member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate team members: %w", err)
	}

	// Identity enrichment, scoped to the team by the same membership join the
	// Postgres impl uses. Host resolution is the interface's, not the raw
	// setting's: the canonical form the capture paths bind under, and an
	// unset github_base_url or jira_base_url is no host and matches nothing.
	// The Linear workspace is matched verbatim, so an org with none matches
	// nothing too.
	ghMap, err := queryIdentityMap(ctx, s.q, `
		SELECT gh.user_id, gh.login
		FROM user_github_identities gh
		JOIN memberships m ON m.user_id = gh.user_id AND m.team_id = ?
		WHERE gh.github_base_url = ?
	`, teamID, db.EffectiveGitHubHost(githubBaseURL))
	if err != nil {
		return nil, 0, fmt.Errorf("list team member github identities: %w", err)
	}
	ghIDMap, err := queryIdentityMap(ctx, s.q, `
		SELECT gh.user_id, gh.github_user_id
		FROM user_github_identities gh
		JOIN memberships m ON m.user_id = gh.user_id AND m.team_id = ?
		WHERE gh.github_base_url = ? AND gh.github_user_id IS NOT NULL
	`, teamID, db.EffectiveGitHubHost(githubBaseURL))
	if err != nil {
		return nil, 0, fmt.Errorf("list team member github account ids: %w", err)
	}
	jiraMap, err := queryIdentityMap(ctx, s.q, `
		SELECT j.user_id, j.account_id
		FROM user_jira_identities j
		JOIN memberships m ON m.user_id = j.user_id AND m.team_id = ?
		WHERE j.jira_base_url = ?
	`, teamID, db.NormalizeJiraHost(jiraBaseURL))
	if err != nil {
		return nil, 0, fmt.Errorf("list team member jira identities: %w", err)
	}
	linearMap, err := queryIdentityMap(ctx, s.q, `
		SELECT l.user_id, l.linear_user_id
		FROM user_linear_identities l
		JOIN memberships m ON m.user_id = l.user_id AND m.team_id = ?
		WHERE l.workspace_id = ?
	`, teamID, linearWorkspaceID)
	if err != nil {
		return nil, 0, fmt.Errorf("list team member linear identities: %w", err)
	}
	linearNameMap, err := queryIdentityMap(ctx, s.q, `
		SELECT l.user_id, l.display_name
		FROM user_linear_identities l
		JOIN memberships m ON m.user_id = l.user_id AND m.team_id = ?
		WHERE l.workspace_id = ? AND l.display_name IS NOT NULL
	`, teamID, linearWorkspaceID)
	if err != nil {
		return nil, 0, fmt.Errorf("list team member linear names: %w", err)
	}
	// An absent (or empty) binding leaves the pointer nil — the "Not
	// connected" state.
	for i := range out {
		if login, ok := ghMap[out[i].UserID]; ok {
			out[i].GitHubUsername = &login
		}
		if id, ok := ghIDMap[out[i].UserID]; ok {
			out[i].GitHubUserID = &id
		}
		if acct, ok := jiraMap[out[i].UserID]; ok {
			out[i].JiraAccountID = &acct
		}
		if id, ok := linearMap[out[i].UserID]; ok {
			out[i].LinearUserID = &id
		}
		if name, ok := linearNameMap[out[i].UserID]; ok {
			out[i].LinearDisplayName = &name
		}
	}
	return out, total, nil
}

// queryIdentityMap runs a (user_id, value) query and returns the user_id →
// value map, dropping empty values so an "" binding reads as nil ("not
// connected") rather than a present-but-blank handle. Twin of the Postgres
// helper of the same name, which is what keeps the two rosters answering
// identically.
func queryIdentityMap(ctx context.Context, q queryer, query string, args ...any) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var userID, value string
		if err := rows.Scan(&userID, &value); err != nil {
			return nil, err
		}
		if value != "" {
			out[userID] = value
		}
	}
	return out, rows.Err()
}

// The roster WRITES are a multi-mode (Postgres) surface: the mutating
// team-roster handlers 404 in local, where there is no second person to
// enrol, promote or remove. These stubs exist only so the local store bundle
// satisfies the interface; reaching them at N=1 is a runmode-gate escape, and
// a clear error beats a write that looks like it landed.
func (s *teamsStore) AddMember(_ context.Context, _, _, _ string) error {
	return db.ErrNotApplicableInLocal
}

func (s *teamsStore) ChangeMemberRole(_ context.Context, _, _, _ string) (string, error) {
	return "", db.ErrNotApplicableInLocal
}

func (s *teamsStore) RemoveMember(_ context.Context, _, _ string) error {
	return db.ErrNotApplicableInLocal
}
