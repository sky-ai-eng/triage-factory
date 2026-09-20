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
