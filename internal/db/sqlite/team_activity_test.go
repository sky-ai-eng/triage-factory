package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTeamActivityStore_SQLite runs the shared conformance suite against the
// SQLite impl, reusing the factory seeder — the two stores share their
// definition of a team's flow. teamID is the local default and ignored by
// the impl: local is N=1 and unscoped, the same posture as the belt.
func TestTeamActivityStore_SQLite(t *testing.T) {
	dbtest.RunTeamActivityConformance(t, func(t *testing.T) (db.TeamActivityStore, string, string, dbtest.FactorySeeder) {
		t.Helper()
		conn, err := sql.Open("sqlite", db.TestDSNMemory)
		if err != nil {
			t.Fatalf("open in-memory db: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		conn.SetMaxOpenConns(1)
		conn.SetMaxIdleConns(1)
		if err := db.BootstrapSchemaForTest(conn); err != nil {
			t.Fatalf("bootstrap schema: %v", err)
		}
		if _, err := conn.Exec(
			`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES (?, 'Factory Test', 'body', ?, ?)`,
			factoryTestPromptID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID,
		); err != nil {
			t.Fatalf("seed prompt: %v", err)
		}
		stores := sqlitestore.New(conn)
		return stores.TeamActivity, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, newSQLiteFactorySeeder(conn)
	})
}
