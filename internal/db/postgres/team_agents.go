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
//   - app   — tf_app, RLS-active. GetForTeam. team_agents_select plus
//     the write policies (team_agents_insert/team_agents_update/
//     team_agents_delete) all gate on tf.user_in_team(team_id), so team
//     members read their own team's row but not other teams'. The
//     insert/update policies additionally enforce agents.org_id =
//     teams.org_id at write time (migration 202605120004) — without
//     that, a team member who guessed another org's agent UUID could
//     create a cross-org reference.
//   - admin — supabase_admin, BYPASSRLS. AddForTeam and GetForTeamSystem.
//     Same reasoning as AgentStore.Create: bootstrap and the router run
//     without claims (org-create + team-create handlers,
//     internal/db/bootstrap.go; internal/routing).
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
	// Match the no-op-on-invalid-UUID convention the reads in this store
	// use. Without this guard the bare ExecContext below would raise
	// Postgres 22P02 on malformed input, which leaks the parse layer into
	// callers and makes the API inconsistent with its peers in the same
	// interface.
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
