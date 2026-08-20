package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamAgentStore is the SQLite impl of db.TeamAgentStore. Local-mode
// shape mirrors the Postgres team_agents schema minus tenancy FKs
// (the teams table doesn't exist locally; team_id stores the synthetic
// LocalDefaultTeamID). assertLocalOrg pins identity at every entry.
type teamAgentStore struct{ q queryer }

func newTeamAgentStore(q queryer) db.TeamAgentStore { return &teamAgentStore{q: q} }

var _ db.TeamAgentStore = (*teamAgentStore)(nil)

const sqliteTeamAgentColumns = `team_id, agent_id, enabled, per_team_model,
       per_team_autonomy_suitability, added_at`

func (s *teamAgentStore) GetForTeamSystem(ctx context.Context, orgID, teamID, agentID string) (*domain.TeamAgent, error) {
	return s.GetForTeam(ctx, orgID, teamID, agentID)
}

func (s *teamAgentStore) GetForTeam(ctx context.Context, orgID, teamID, agentID string) (*domain.TeamAgent, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT `+sqliteTeamAgentColumns+`
		FROM team_agents
		WHERE team_id = ? AND agent_id = ?
	`, teamID, agentID)
	ta, err := scanTeamAgentRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ta, nil
}

func (s *teamAgentStore) AddForTeam(ctx context.Context, orgID, teamID, agentID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// INSERT OR IGNORE so re-runs don't reset the user's toggle / overrides.
	_, err := s.q.ExecContext(ctx, `
		INSERT OR IGNORE INTO team_agents (team_id, agent_id, enabled, added_at)
		VALUES (?, ?, 1, ?)
	`, teamID, agentID, time.Now().UTC())
	return err
}

// scanUpdatedTeamAgent decodes a key-addressed UPDATE … RETURNING. No row
// scanned means the (team, agent) pair named nothing, which is
// db.ErrNoSuchTeamAgent.
func scanUpdatedTeamAgent(row *sql.Row) (domain.TeamAgent, error) {
	ta, err := scanTeamAgentRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TeamAgent{}, db.ErrNoSuchTeamAgent
	}
	return ta, err
}

func (s *teamAgentStore) SetEnabled(ctx context.Context, orgID, teamID, agentID string, enabled bool) (domain.TeamAgent, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.TeamAgent{}, err
	}
	return scanUpdatedTeamAgent(s.q.QueryRowContext(ctx, `
		UPDATE team_agents SET enabled = ? WHERE team_id = ? AND agent_id = ?
		RETURNING `+sqliteTeamAgentColumns,
		enabled, teamID, agentID))
}

func (s *teamAgentStore) SetOverrides(ctx context.Context, orgID, teamID, agentID string, model *string, autonomy *float64) (domain.TeamAgent, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.TeamAgent{}, err
	}
	var modelArg any
	if model != nil && *model != "" {
		modelArg = *model
	}
	return scanUpdatedTeamAgent(s.q.QueryRowContext(ctx, `
		UPDATE team_agents
		SET per_team_model = ?,
		    per_team_autonomy_suitability = ?
		WHERE team_id = ? AND agent_id = ?
		RETURNING `+sqliteTeamAgentColumns,
		modelArg, autonomy, teamID, agentID))
}

func (s *teamAgentStore) Remove(ctx context.Context, orgID, teamID, agentID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `DELETE FROM team_agents WHERE team_id = ? AND agent_id = ?`,
		teamID, agentID)
	return err
}

func (s *teamAgentStore) ListForOrg(ctx context.Context, orgID, agentID string) ([]domain.TeamAgent, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteTeamAgentColumns+`
		FROM team_agents
		WHERE agent_id = ?
		ORDER BY added_at ASC
	`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.TeamAgent
	for rows.Next() {
		ta, err := scanTeamAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ta)
	}
	return out, rows.Err()
}

func scanTeamAgent(rows *sql.Rows) (domain.TeamAgent, error) {
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

func scanTeamAgentRow(row *sql.Row) (domain.TeamAgent, error) {
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
