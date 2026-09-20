package dbtest

import (
	"database/sql"
	"testing"
)

// SetTeamAgentEnabledDirect flips a team_agents row's enabled bit via raw
// SQLite SQL. No production door writes this column: bootstrap's AddForTeam
// inserts the row default-enabled and nothing toggles it afterwards, while the
// router, the delegate handlers and the bootstrap re-run all READ it. A fixture
// whose subject is how those readers treat a disabled row therefore writes the
// shape directly — the same reason SeedBlueprintRun bypasses the blueprint
// door.
func SetTeamAgentEnabledDirect(tb testing.TB, database *sql.DB, teamID, agentID string, enabled bool) {
	tb.Helper()
	res, err := database.Exec(
		`UPDATE team_agents SET enabled = ? WHERE team_id = ? AND agent_id = ?`,
		enabled, teamID, agentID,
	)
	if err != nil {
		tb.Fatalf("SetTeamAgentEnabledDirect(%s, %s): %v", teamID, agentID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		tb.Fatalf("SetTeamAgentEnabledDirect(%s, %s): matched %d rows, want 1", teamID, agentID, n)
	}
}
