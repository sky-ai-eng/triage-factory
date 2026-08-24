package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
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
	// deleted_at IS NULL: an archived team is never the org's default. Applies to
	// both pools — the default-team pick should skip a tombstoned team in every
	// context (TFAC-448).
	err := q.QueryRowContext(ctx, `
		SELECT id::text FROM teams
		WHERE org_id = $1 AND deleted_at IS NULL
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

// pgTeamSettingsColumns is the canonical projection of a team_settings row, in
// the order scanTeamSettings reads them. Both reads SELECT it and both writes
// RETURN it, so the write shape cannot drift from the read shape.
//
// array_to_json(...)::text round-trips text[] as a JSON literal.
// database/sql + pgx stdlib doesn't ship a scanner for *[]string, so the JSON
// detour is the portable shape — matches what the jira_project_status_rules
// reader does for its array columns.
const pgTeamSettingsColumns = `array_to_json(jira_projects)::text,
		       ai_reprioritize_threshold, ai_preference_update_interval,
		       default_model, auto_delegate_enabled, auto_mode_enabled,
		       permission_absent_grace_ms, permission_absent_autodeny_enabled,
		       max_daily_cost_usd, branch_template, review_posture,
		       base_branch_push_policy, enabled_models`

// scanTeamSettings decodes one team_settings row in pgTeamSettingsColumns order.
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
	)
	if err := scan(
		&projectsJSON, &aiThreshold, &aiInterval,
		&defaultModel, &autoDelegate, &autoMode,
		&permAbsentGraceMS, &permAbsentAutodeny,
		&maxDailyCost, &branchTemplate, &reviewPosture, &basePushPolicy,
		&enabledModels,
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
	return domain.TeamSettings{
		JiraProjects:                    projects,
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

func getTeamSettings(ctx context.Context, q queryer, teamID string) (domain.TeamSettings, error) {
	set, err := scanTeamSettings(q.QueryRowContext(ctx, `
		SELECT `+pgTeamSettingsColumns+`
		FROM team_settings WHERE team_id = $1
	`, teamID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		// See OrgsStore for the rationale. Matches team_settings'
		// schema DEFAULT clauses.
		return domain.DefaultTeamSettingsFor(true), nil
	}
	if err != nil {
		return domain.TeamSettings{}, fmt.Errorf("read team_settings: %w", err)
	}
	return set, nil
}

func (s *teamsStore) ListForUser(ctx context.Context, orgID string, opts db.ListOpts) ([]domain.Team, int, error) {
	// teams_select RLS gates on org access, not team membership, so it
	// returns every team in the org. The memberships join is what
	// narrows to the caller's own teams — memberships_select lets a user
	// read their own rows (user_id = current_user_id()), so the join is
	// RLS-safe and self-scoping without an explicit user_id parameter.
	// m.role rides along from the same membership join: it's the caller's
	// own role in each team (the settings surface gates the Team section +
	// its selector on role='admin'). The join already narrows to the
	// caller's rows, so this needs no extra predicate.
	// t.deleted_at IS NULL hides archived teams from the selectors (TFAC-448) —
	// the request-facing read filter mirrored on the SQLite impl; the lifecycle
	// paths read archived teams through the ...System variants instead.
	var total int
	if err := s.app.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM teams t
		JOIN memberships m ON m.team_id = t.id AND m.user_id = tf.current_user_id()
		WHERE t.org_id = $1 AND t.deleted_at IS NULL
	`, orgID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count teams for user: %w", err)
	}
	if opts.CountOnly {
		return []domain.Team{}, total, nil
	}

	query := `
		SELECT t.id::text, t.org_id::text, t.slug, t.name, COALESCE(t.description, ''), t.created_at, m.role::text
		FROM teams t
		JOIN memberships m ON m.team_id = t.id AND m.user_id = tf.current_user_id()
		WHERE t.org_id = $1 AND t.deleted_at IS NULL
		ORDER BY t.created_at ASC, t.id ASC`
	args := []any{orgID}
	if opts.Limit > 0 {
		query += `
		LIMIT $2 OFFSET $3`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.app.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list teams for user: %w", err)
	}
	defer rows.Close()
	teams, err := scanTeams(rows)
	return teams, total, err
}

// Get reads under the caller's claims: teams_select gates on org access, so a
// team in the caller's org resolves whether or not they are on it, and the
// LEFT JOIN reports their role as empty when they are not. A cross-org id is
// invisible to the policy and comes back nil — the not-found the disclosure
// rule asks for, produced by the database rather than by a handler predicate.
// pgTeamRowColumns is the canonical projection of a teams ROW, in the order
// scanTeamRow reads them. Get SELECTs it alongside the caller's membership
// role, and the archive/restore writes RETURN it — role is not in the list
// because it is not a column of this table: it is the caller's own membership,
// which Get resolves through a join and a write has no view on.
const pgTeamRowColumns = `id::text, org_id::text, slug, name, COALESCE(description, ''), created_at, deleted_at`

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
	var (
		t       domain.Team
		deleted sql.NullTime
		role    sql.NullString
	)
	err := s.app.QueryRowContext(ctx, `
		SELECT t.id::text, t.org_id::text, t.slug, t.name, COALESCE(t.description, ''),
		       t.created_at, t.deleted_at, m.role::text
		FROM teams t
		LEFT JOIN memberships m ON m.team_id = t.id AND m.user_id = tf.current_user_id()
		WHERE t.id = $1 AND t.org_id = $2
	`, teamID, orgID).Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.Description, &t.CreatedAt, &deleted, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get team: %w", err)
	}
	if deleted.Valid {
		t.DeletedAt = &deleted.Time
	}
	t.Role = role.String
	return &t, nil
}

func (s *teamsStore) Archive(ctx context.Context, teamID string) (domain.Team, error) {
	return setTeamArchived(ctx, s.app, teamID, true)
}

func (s *teamsStore) Restore(ctx context.Context, teamID string) (domain.Team, error) {
	return setTeamArchived(ctx, s.app, teamID, false)
}

// setTeamArchived flips teams.deleted_at: archive stamps now() WHERE deleted_at
// IS NULL, restore clears it WHERE deleted_at IS NOT NULL. The state-guarded
// WHERE makes both idempotent-safe — a no-op flip (re-archiving an archived
// team, restoring a live one) affects zero rows and surfaces as ErrTeamNotFound
// so the handler answers 404/409 rather than silently succeeding. App pool:
// teams_update RLS gates org-admin via the handler.
func setTeamArchived(ctx context.Context, q queryer, teamID string, archive bool) (domain.Team, error) {
	stmt := `
			UPDATE teams SET deleted_at = NULL, updated_at = now()
			WHERE id = $1 AND deleted_at IS NOT NULL
			RETURNING ` + pgTeamRowColumns
	if archive {
		stmt = `
			UPDATE teams SET deleted_at = now(), updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL
			RETURNING ` + pgTeamRowColumns
	}
	// No row scanned means the state-guarded WHERE matched nothing — already
	// archived, already active, missing, or invisible under RLS — which is
	// db.ErrTeamNotFound, the same answer the rows-affected probe here used to
	// synthesize.
	t, err := scanTeamRow(q.QueryRowContext(ctx, stmt, teamID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Team{}, db.ErrTeamNotFound
	}
	if err != nil {
		return domain.Team{}, fmt.Errorf("set team archived=%v: %w", archive, err)
	}
	return t, nil
}

func (s *teamsStore) GetSystem(ctx context.Context, orgID, teamID string) (*domain.Team, error) {
	var (
		t       domain.Team
		deleted sql.NullTime
	)
	err := s.admin.QueryRowContext(ctx, `
		SELECT id::text, org_id::text, slug, name, COALESCE(description, ''),
		       created_at, deleted_at
		FROM teams
		WHERE id = $1 AND org_id = $2
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
	if err := s.admin.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM teams WHERE org_id = $1 AND deleted_at IS NOT NULL
	`, orgID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count archived teams: %w", err)
	}
	if opts.CountOnly {
		return []domain.Team{}, total, nil
	}
	query := `
		SELECT id::text, org_id::text, slug, name, COALESCE(description, ''),
		       created_at, deleted_at
		FROM teams
		WHERE org_id = $1 AND deleted_at IS NOT NULL
		ORDER BY deleted_at DESC, id ASC`
	args := []any{orgID}
	if opts.Limit > 0 {
		query += `
		LIMIT $2 OFFSET $3`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.admin.QueryContext(ctx, query, args...)
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
	rows, err := s.admin.QueryContext(ctx, `
		SELECT id::text, org_id::text, slug, name, COALESCE(description, ''), created_at
		FROM teams
		WHERE org_id = $1 AND deleted_at IS NULL
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
	if err := s.admin.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM teams WHERE org_id = $1 AND deleted_at IS NULL
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
		SELECT t.id::text, t.name, ts.max_daily_cost_usd
		FROM teams t
		LEFT JOIN team_settings ts ON ts.team_id = t.id
		WHERE t.org_id = $1 AND t.deleted_at IS NULL
		ORDER BY t.name ASC, t.id ASC`
	args := []any{orgID}
	if opts.Limit > 0 {
		query += `
		LIMIT $2 OFFSET $3`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.admin.QueryContext(ctx, query, args...)
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
	ids := dedupeNonEmpty(teamIDs)
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT id::text, name FROM teams WHERE org_id = $1 AND id = ANY($2)
	`, orgID, pgUUIDArray(ids))
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

// dedupeNonEmpty drops blanks and duplicates from ids, preserving
// first-seen order.
func dedupeNonEmpty(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// MemberIDsSystem — identical query to the SQLite impl so both backends pin to
// one result. Admin pool: the emitter is a claims-free notifier, and the
// audience must not vary with who made the change.
func (s *teamsStore) MemberIDsSystem(ctx context.Context, orgID, teamID string) ([]string, error) {
	// memberships ⋈ teams, with the org in the WHERE clause as defense in
	// depth: memberships carries no org_id (it FKs to teams), so the join is
	// also what drops a dangling row whose team is gone. No deleted_at filter
	// — archiving hides nothing a member could already see, and their KB page
	// would otherwise go silently stale.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT DISTINCT m.user_id::text
		FROM memberships m
		JOIN teams t ON t.id = m.team_id
		WHERE t.org_id = $1 AND m.team_id = $2
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
	// memberships ⋈ teams, scoped to the org. The join to teams is what
	// applies the org filter (memberships carries no org_id — it FKs to
	// teams), and it also drops any dangling membership row whose team
	// has since been deleted. Admin pool: the router resolves an
	// arbitrary author/reviewer userID with no JWT claims, so this can't
	// lean on current_user_id() the way ListForUser does.
	// t.deleted_at IS NULL excludes archived teams (TFAC-448): the router resolves
	// an author/reviewer's teams to route new tasks to, and an archived team must
	// not receive new work (it's been force-stopped + write-blocked).
	rows, err := s.admin.QueryContext(ctx, `
		SELECT t.id::text
		FROM memberships m
		JOIN teams t ON t.id = m.team_id
		WHERE t.org_id = $1 AND m.user_id = $2 AND t.deleted_at IS NULL
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

func (s *teamsStore) Update(ctx context.Context, teamID string, name, slug, description *string) (domain.Team, error) {
	// App pool: teams_update RLS gates the write to team-admin-or-org-admin
	// (widened from org-admin-only). COALESCE applies each arg only when
	// non-nil, so a description-only PATCH leaves name/slug untouched and an
	// empty-string description clears the blurb (the empty string is non-NULL,
	// so COALESCE takes it). RETURNING reads the post-update row back under the
	// same statement; a zero-row result (id invisible to RLS, or deleted in a
	// race past the handler gate) surfaces as ErrTeamNotFound for a clean 404.
	var t domain.Team
	err := s.app.QueryRowContext(ctx, `
		UPDATE teams SET
			name = COALESCE($2, name),
			slug = COALESCE($3, slug),
			description = COALESCE($4, description),
			updated_at = now()
		WHERE id = $1
		RETURNING id::text, org_id::text, slug, name, COALESCE(description, ''), created_at
	`, teamID, name, slug, description).
		Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.Description, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Team{}, db.ErrTeamNotFound
	}
	if err != nil {
		// The UNIQUE (org_id, slug) collision passes through verbatim — the
		// handler maps "duplicate key" to a 409, the same as Create.
		return domain.Team{}, fmt.Errorf("update team: %w", err)
	}
	return t, nil
}

// scanTeams reads the ListForUser projection — identity columns plus the
// caller's per-team role. It's the sole consumer of that 6-column shape;
// the other team queries (Create, GetDefaultForOrg) scan their own rows.
func scanTeams(rows *sql.Rows) ([]domain.Team, error) {
	out := []domain.Team{}
	for rows.Next() {
		var t domain.Team
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.Description, &t.CreatedAt, &t.Role); err != nil {
			return nil, fmt.Errorf("scan team: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *teamsStore) UpdateSettings(ctx context.Context, teamID string, u domain.TeamSettings) (domain.TeamSettings, error) {
	projects := u.JiraProjects
	if projects == nil {
		projects = []string{}
	}
	// max_daily_cost_usd rides along (0 → NULL via nullFloat) so a
	// read-modify-write team-settings save round-trips the org-admin-set value
	// untouched. The team-settings handler never populates that field from its
	// request body — a team admin cannot set their own cap — so its
	// GetSettings→UpdateSettings flow writes back exactly what it read; it is
	// *changed* only by the org-admin SetDailyCostCapSystem path.
	stored, err := scanTeamSettings(s.app.QueryRowContext(ctx, `
		INSERT INTO team_settings (
			team_id, jira_projects, ai_reprioritize_threshold,
			ai_preference_update_interval, default_model, auto_delegate_enabled,
			auto_mode_enabled,
			permission_absent_grace_ms, permission_absent_autodeny_enabled,
			max_daily_cost_usd, branch_template, review_posture,
			base_branch_push_policy, enabled_models, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now())
		ON CONFLICT (team_id) DO UPDATE SET
			jira_projects = EXCLUDED.jira_projects,
			ai_reprioritize_threshold = EXCLUDED.ai_reprioritize_threshold,
			ai_preference_update_interval = EXCLUDED.ai_preference_update_interval,
			default_model = EXCLUDED.default_model,
			auto_delegate_enabled = EXCLUDED.auto_delegate_enabled,
			auto_mode_enabled = EXCLUDED.auto_mode_enabled,
			permission_absent_grace_ms = EXCLUDED.permission_absent_grace_ms,
			permission_absent_autodeny_enabled = EXCLUDED.permission_absent_autodeny_enabled,
			max_daily_cost_usd = EXCLUDED.max_daily_cost_usd,
			branch_template = EXCLUDED.branch_template,
			review_posture = EXCLUDED.review_posture,
			base_branch_push_policy = EXCLUDED.base_branch_push_policy,
			enabled_models = EXCLUDED.enabled_models,
			updated_at = now()
		RETURNING `+pgTeamSettingsColumns,
		teamID, projects, u.AIReprioritizeThreshold,
		u.AIPreferenceUpdateInterval, u.DefaultModel, u.AutoDelegateEnabled,
		u.AutoModeEnabled,
		u.PermissionAbsentGraceMS, u.PermissionAbsentAutodenyEnabled,
		nullFloat(u.MaxDailyCostUSD), u.BranchTemplate, u.ReviewPosture,
		u.BaseBranchPushPolicy, db.ModelSetColumnValue(u.EnabledModels),
	).Scan)
	if err != nil {
		return domain.TeamSettings{}, fmt.Errorf("upsert team_settings: %w", err)
	}
	return stored, nil
}

// SetDailyCostCapSystem upserts ONLY team_settings.max_daily_cost_usd for teamID
// — the org-admin per-team daily spend cap (TFAC-482). Admin pool (BYPASSRLS):
// an org admin setting a team's cap may not be a member of that team, so the
// app-pool team_settings_update RLS (team-admin-gated) would reject the write;
// the HTTP RequireOrgAdminRole gate is the authorization for this System write.
// capUSD ≤ 0 stores SQL NULL ("no cap"). The partial INSERT relies on the schema
// DEFAULT clauses for every other team_settings column when no row exists yet,
// and ON CONFLICT touches only the cap so the team's other settings are never
// clobbered. org_id isn't a column on team_settings (it FKs teams), so scoping
// is by the teamID the org-admin handler already verified is in the org.
func (s *teamsStore) SetDailyCostCapSystem(ctx context.Context, teamID string, capUSD float64) (domain.TeamSettings, error) {
	// ≤ 0 → SQL NULL ("no cap"); any positive value is the cap.
	var capArg any
	if capUSD > 0 {
		capArg = capUSD
	}
	// RETURNING projects the whole settings row: the insert arm fills every
	// other column from schema defaults, so the row this lands in is one the
	// caller has never seen.
	stored, err := scanTeamSettings(s.admin.QueryRowContext(ctx, `
		INSERT INTO team_settings (team_id, max_daily_cost_usd, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (team_id) DO UPDATE SET
			max_daily_cost_usd = EXCLUDED.max_daily_cost_usd,
			updated_at = now()
		RETURNING `+pgTeamSettingsColumns,
		teamID, capArg).Scan)
	if err != nil {
		return domain.TeamSettings{}, fmt.Errorf("set team daily cost cap: %w", err)
	}
	return stored, nil
}

func (s *teamsStore) ListMembers(ctx context.Context, teamID, githubBaseURL, jiraBaseURL string, opts db.ListOpts) ([]domain.TeamMember, int, error) {
	// 0. The filtered total, on the same pool and the same FROM as the page
	//    below — a count taken through a different join could disagree with
	//    the rows it is meant to describe.
	var total int
	if err := s.app.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.team_id = $1
	`, teamID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count team members: %w", err)
	}
	if opts.CountOnly {
		return []domain.TeamMember{}, total, nil
	}

	// 1. Core roster on the app pool (RLS-gated). memberships_select lets any
	//    org member read a team-in-org's roster; display_name is nullable, so
	//    COALESCE renders the empty string (matching GetDisplayName's "").
	//    The ORDER BY ends in m.user_id: two members sharing a display name
	//    would otherwise tie, and a tie under LIMIT/OFFSET is a row that
	//    repeats on one page and never appears on another.
	query := `
		SELECT m.user_id::text, COALESCE(u.display_name, ''), m.role::text
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.team_id = $1
		ORDER BY COALESCE(u.display_name, ''), m.user_id`
	args := []any{teamID}
	if opts.Limit > 0 {
		query += `
		LIMIT $2 OFFSET $3`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.app.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list team members: %w", err)
	}
	defer rows.Close()
	out := []domain.TeamMember{}
	ids := []string{}
	for rows.Next() {
		var m domain.TeamMember
		if err := rows.Scan(&m.UserID, &m.DisplayName, &m.Role); err != nil {
			return nil, 0, fmt.Errorf("scan team member: %w", err)
		}
		out = append(out, m)
		ids = append(ids, m.UserID)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate team members: %w", err)
	}
	if len(out) == 0 {
		return out, total, nil
	}

	// 2. Identity enrichment on the admin pool, scoped to this team's members
	//    by a membership join — the team-tier twin of
	//    OrgMembershipsStore.ListWithIdentity. The self-only identity-table
	//    RLS can't show a peer's binding, but the roster's whole purpose is to
	//    show every teammate's readiness; the membership join keeps the
	//    admin-pool read scoped to exactly the members the RLS roster already
	//    returned, and the id list narrows it further to the page in hand so
	//    the enrichment costs the window rather than the whole roster. Host
	//    resolution mirrors the org roster: an unset github_base_url resolves
	//    to github.com (EffectiveGitHubHost), an unset jira_base_url
	//    normalizes to "" and matches nothing.
	ghMap, err := queryIdentityMap(ctx, s.admin, `
		SELECT gh.user_id::text, gh.login
		FROM user_github_identities gh
		JOIN memberships m ON m.user_id = gh.user_id AND m.team_id = $1
		WHERE gh.github_base_url = $2 AND gh.user_id = ANY($3)
	`, teamID, db.EffectiveGitHubHost(githubBaseURL), pgUUIDArray(ids))
	if err != nil {
		return nil, 0, fmt.Errorf("list team member github identities: %w", err)
	}
	jiraMap, err := queryIdentityMap(ctx, s.admin, `
		SELECT j.user_id::text, j.account_id
		FROM user_jira_identities j
		JOIN memberships m ON m.user_id = j.user_id AND m.team_id = $1
		WHERE j.jira_base_url = $2 AND j.user_id = ANY($3)
	`, teamID, db.NormalizeJiraHost(jiraBaseURL), pgUUIDArray(ids))
	if err != nil {
		return nil, 0, fmt.Errorf("list team member jira identities: %w", err)
	}

	// 3. Merge. An absent (or empty) binding leaves the pointer nil — the
	//    "Not connected" state.
	for i := range out {
		if login, ok := ghMap[out[i].UserID]; ok {
			out[i].GitHubUsername = &login
		}
		if acct, ok := jiraMap[out[i].UserID]; ok {
			out[i].JiraAccountID = &acct
		}
	}
	return out, total, nil
}

// queryIdentityMap runs a (user_id, value) query on q and returns the
// user_id → value map, dropping empty values so an "" binding reads as nil
// ("not connected") rather than a present-but-blank handle. The team roster's
// identity enrichment uses it; the org roster has its own method-bound twin.
func queryIdentityMap(ctx context.Context, q queryer, query string, args ...any) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var id, val string
		if err := rows.Scan(&id, &val); err != nil {
			return nil, err
		}
		if val != "" {
			m[id] = val
		}
	}
	return m, rows.Err()
}

func (s *teamsStore) AddMember(ctx context.Context, teamID, userID, role string) error {
	// Cast the bound string to the enum explicitly — role is a
	// public.membership_role column; the handler already rejected any value
	// outside the enum, so this never trips a 22P02.
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO memberships (user_id, team_id, role)
		VALUES ($1, $2, $3::membership_role)
	`, userID, teamID, role)
	if err != nil {
		return translateTeamMemberInsert(fmt.Errorf("insert membership: %w", err))
	}
	return nil
}

func (s *teamsStore) ChangeMemberRole(ctx context.Context, teamID, userID, role string) (string, error) {
	// Capture and return the prior role via the prev CTE (snapshotted before the
	// UPDATE applies) so the governance audit log records the old→new
	// transition. A zero-row UPDATE — the member isn't on the team — yields no
	// RETURNING row → sql.ErrNoRows → ErrTeamMemberNotFound (the
	// assertOneTeamMemberRow contract, expressed through the query).
	var oldRole string
	err := s.app.QueryRowContext(ctx, `
		WITH prev AS (
			SELECT role FROM memberships
			WHERE team_id = $1 AND user_id = $2
		)
		UPDATE memberships m SET role = $3::membership_role
		FROM prev
		WHERE m.team_id = $1 AND m.user_id = $2
		RETURNING prev.role::text
	`, teamID, userID, role).Scan(&oldRole)
	if errors.Is(err, sql.ErrNoRows) {
		return "", db.ErrTeamMemberNotFound
	}
	if err != nil {
		return "", translateTeamAdminGuard(fmt.Errorf("update membership role: %w", err))
	}
	return oldRole, nil
}

func (s *teamsStore) RemoveMember(ctx context.Context, teamID, userID string) error {
	res, err := s.app.ExecContext(ctx, `
		DELETE FROM memberships WHERE team_id = $1 AND user_id = $2
	`, teamID, userID)
	if err != nil {
		return translateTeamAdminGuard(fmt.Errorf("delete membership: %w", err))
	}
	return assertOneTeamMemberRow(res, "delete membership")
}

// translateTeamAdminGuard maps the tf.guard_team_admins trigger's SQLSTATE
// 23514 to db.ErrLastTeamAdminGuard so the handler answers 409 without
// importing the pg driver. guard_team_admins is the only 23514 source on
// memberships (the table carries no CHECK constraints; an invalid role enum
// fails with 22P02, and callers validate role first), so the code→sentinel
// mapping is unambiguous. Any other error passes through. The org tier's
// translateOwnerGuard is its twin.
func translateTeamAdminGuard(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		return errors.Join(db.ErrLastTeamAdminGuard, err)
	}
	return err
}

// translateTeamMemberInsert maps the memberships PK collision (unique_violation
// 23505) to db.ErrTeamMemberExists for AddMember, so adding a user already on
// the team is a clean 409 rather than a raw driver error.
func translateTeamMemberInsert(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return errors.Join(db.ErrTeamMemberExists, err)
	}
	return err
}

// assertOneTeamMemberRow turns a zero-row UPDATE/DELETE result into
// db.ErrTeamMemberNotFound. The handlers gate team-admin (or self) before
// calling, so a row visible to the policy but absent means the target user
// simply isn't on the team — a 404, not a silent success.
func assertOneTeamMemberRow(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", op, err)
	}
	if n == 0 {
		return db.ErrTeamMemberNotFound
	}
	return nil
}
