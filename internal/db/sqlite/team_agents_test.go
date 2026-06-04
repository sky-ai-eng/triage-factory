package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTeamAgentStore_SQLite runs the shared TeamAgentStore conformance
// suite against the SQLite impl. The factory pre-creates the agents
// row so team_agents.agent_id FK is satisfied (SQLite enforces FK with
// the pragma set in openSQLiteForTest).
func TestTeamAgentStore_SQLite(t *testing.T) {
	dbtest.RunTeamAgentStoreConformance(t, func(t *testing.T) (db.TeamAgentStore, string, string, string) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		ctx := t.Context()
		agentID, err := stores.Agents.Create(ctx, runmode.LocalDefaultOrg, domain.Agent{})
		if err != nil {
			t.Fatalf("seed agent: %v", err)
		}
		return stores.TeamAgents, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, agentID
	})
}

// TestTeamAgentStore_SQLite_AssertsLocalOrg pins the local-org guard.
func TestTeamAgentStore_SQLite_AssertsLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	if _, err := stores.TeamAgents.GetForTeam(t.Context(), "some-org", "team", "agent"); err == nil {
		t.Fatal("GetForTeam accepted non-local orgID; should reject")
	}
}
