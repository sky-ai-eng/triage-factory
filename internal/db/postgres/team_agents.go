package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamAgentStore is the Postgres impl of db.TeamAgentStore.
//
// # Pool split
//
//   - app   — tf_app, RLS-active. GetForTeam, SetEnabled, SetOverrides,
//     Remove, ListForOrg. team_agents_select plus the write policies
//     (team_agents_insert/team_agents_update/team_agents_delete) all
//     gate on tf.user_in_team(team_id), so team members read/write
//     their own team's row but not other teams'. The insert/update
//     policies additionally enforce agents.org_id = teams.org_id at
//     write time (migration 202605120004) — without that, a team
//     member who guessed another org's agent UUID could create a
//     cross-org reference.
//   - admin — supabase_admin, BYPASSRLS. AddForTeam only. Same
//     reasoning as AgentStore.Create: bootstrap runs without claims
//     (org-create + team-create handlers, internal/db/bootstrap.go).
//
// Note: orgID is accepted for API symmetry with the SQLite impl and to
// keep call sites uniform, but the Postgres impl doesn't filter on it
// — team_id is globally unique (UUID PK on teams) and the RLS policy
// already constrains cross-team access. The orgID is verified upstream
// at the router / handler layer where the request-scoped org context
// is canonical.
type teamAgentStore struct {
	app   queryer
	admin queryer
	inTx  bool
}

func newTeamAgentStore(app, admin queryer) db.TeamAgentStore {
	return &teamAgentStore{app: app, admin: admin}
}

func newTxTeamAgentStore(tx queryer) db.TeamAgentStore {
	return &teamAgentStore{app: tx, admin: tx, inTx: true}
}

var _ db.TeamAgentStore = (*teamAgentStore)(nil)

const pgTeamAgentColumns = `team_id, agent_id, enabled, per_team_model,
       per_team_autonomy_suitability, added_at`

func (s *teamAgentStore) GetForTeam(ctx context.Context, orgID, teamID, agentID string) (*domain.TeamAgent, error) {
	ta, err := getTeamAgent(ctx, s.app, teamID, agentID)
	return ta, wrapAppPoolPermErr(err, "team_agents.GetForTeam")
}

func (s *teamAgentStore) GetForTeamSystem(ctx context.Context, orgID, teamID, agentID string) (*domain.TeamAgent, error) {
	return getTeamAgent(ctx, s.admin, teamID, agentID)
}

func getTeamAgent(ctx context.Context, q queryer, teamID, agentID string) (*domain.TeamAgent, error) {
	if !isValidUUID(teamID) || !isValidUUID(agentID) {
		return nil, nil
	}
	row := q.QueryRowContext(ctx, `
		SELECT `+pgTeamAgentColumns+`
		FROM team_agents
		WHERE team_id = $1 AND agent_id = $2
	`, teamID, agentID)
	ta, err := scanTeamAgentRowPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ta, nil
}

func (s *teamAgentStore) AddForTeam(ctx context.Context, orgID, teamID, agentID string) error {
	if s.inTx {
		// Same justification as agentStore.Create — admin-pool routing
		// from inside a caller's tx breaks tx scope. Production
		// bootstrap runs outside any user tx.
		return errors.New("postgres team_agents: AddForTeam must not be called inside WithTx; call stores.TeamAgents.AddForTeam directly")
	}
	// Match the no-op-on-invalid-UUID convention the rest of this store
	// uses (GetForTeam, SetEnabled, SetOverrides, Remove). Without this
	// guard the bare ExecContext below would raise Postgres 22P02 on
	// malformed input, which leaks the parse layer into callers and
	// makes the API inconsistent with its peers in the same interface.
	if !isValidUUID(teamID) || !isValidUUID(agentID) {
		return nil
	}
	// ON CONFLICT preserves existing rows verbatim — re-runs of
	// bootstrap don't flip Enabled back to TRUE if a team has
	// already disabled the bot.
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO team_agents (team_id, agent_id, enabled)
		VALUES ($1, $2, TRUE)
		ON CONFLICT (team_id, agent_id) DO NOTHING
	`, teamID, agentID)
	return err
}

// scanUpdatedTeamAgent decodes a key-addressed UPDATE … RETURNING. No row
// scanned means the (team, agent) pair named nothing — or that RLS hides
// whatever does — and both are db.ErrNoSuchTeamAgent.
func scanUpdatedTeamAgent(row *sql.Row) (domain.TeamAgent, error) {
	ta, err := scanTeamAgentRowPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TeamAgent{}, db.ErrNoSuchTeamAgent
	}
	return ta, err
}

func (s *teamAgentStore) SetEnabled(ctx context.Context, orgID, teamID, agentID string, enabled bool) (domain.TeamAgent, error) {
	if !isValidUUID(teamID) || !isValidUUID(agentID) {
		return domain.TeamAgent{}, db.ErrNoSuchTeamAgent
	}
	return scanUpdatedTeamAgent(s.app.QueryRowContext(ctx, `
		UPDATE team_agents SET enabled = $1 WHERE team_id = $2 AND agent_id = $3
		RETURNING `+pgTeamAgentColumns,
		enabled, teamID, agentID))
}

func (s *teamAgentStore) SetOverrides(ctx context.Context, orgID, teamID, agentID string, model *string, autonomy *float64) (domain.TeamAgent, error) {
	if !isValidUUID(teamID) || !isValidUUID(agentID) {
		return domain.TeamAgent{}, db.ErrNoSuchTeamAgent
	}
	var modelArg any
	if model != nil && *model != "" {
		modelArg = *model
	}
	return scanUpdatedTeamAgent(s.app.QueryRowContext(ctx, `
		UPDATE team_agents
		SET per_team_model = $1,
		    per_team_autonomy_suitability = $2
		WHERE team_id = $3 AND agent_id = $4
		RETURNING `+pgTeamAgentColumns,
		modelArg, autonomy, teamID, agentID))
}

func (s *teamAgentStore) Remove(ctx context.Context, orgID, teamID, agentID string) error {
	if !isValidUUID(teamID) || !isValidUUID(agentID) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `
		DELETE FROM team_agents WHERE team_id = $1 AND agent_id = $2
	`, teamID, agentID)
	return err
}

func (s *teamAgentStore) ListForOrg(ctx context.Context, orgID, agentID string) ([]domain.TeamAgent, error) {
	if !isValidUUID(agentID) {
		return nil, nil
	}
	rows, err := s.app.QueryContext(ctx, `
		SELECT `+pgTeamAgentColumns+`
		FROM team_agents
		WHERE agent_id = $1
		ORDER BY added_at ASC
	`, agentID)
	if err != nil {
		return nil, wrapAppPoolPermErr(err, "team_agents.ListForOrg")
	}
	defer rows.Close()

	var out []domain.TeamAgent
	for rows.Next() {
		ta, err := scanTeamAgentPG(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ta)
	}
	return out, rows.Err()
}

func scanTeamAgentPG(rows *sql.Rows) (domain.TeamAgent, error) {
	var ta domain.TeamAgent
	var model sql.NullString
	var autonomy sql.NullFloat64
	if err := rows.Scan(&ta.TeamID, &ta.AgentID, &ta.Enabled, &model, &autonomy, &ta.AddedAt); err != nil {
		return ta, err
	}
	ta.PerTeamModel = model.String
	if autonomy.Valid {
		v := autonomy.Float64
		ta.PerTeamAutonomySuitability = &v
	}
	return ta, nil
}

func scanTeamAgentRowPG(row *sql.Row) (domain.TeamAgent, error) {
	var ta domain.TeamAgent
	var model sql.NullString
	var autonomy sql.NullFloat64
	if err := row.Scan(&ta.TeamID, &ta.AgentID, &ta.Enabled, &model, &autonomy, &ta.AddedAt); err != nil {
		return ta, err
	}
	ta.PerTeamModel = model.String
	if autonomy.Valid {
		v := autonomy.Float64
		ta.PerTeamAutonomySuitability = &v
	}
	return ta, nil
}
