package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// TestTeamListForOrg_SQLite runs the shared ListForOrg conformance suite
// against the SQLite impl, on a fresh in-memory DB whose orgs and teams the
// seeder inserts through raw SQL.
func TestTeamListForOrg_SQLite(t *testing.T) {
	dbtest.RunTeamListForOrgConformance(t, func(t *testing.T) (db.TeamsStore, dbtest.TeamListSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		seed := dbtest.TeamListSeeder{
			Org: func(t *testing.T) string {
				t.Helper()
				id := uuid.NewString()
				if _, err := conn.Exec(`INSERT INTO orgs (id, slug, name) VALUES (?, ?, ?)`, id, id, "org-"+id[:8]); err != nil {
					t.Fatalf("seed org: %v", err)
				}
				return id
			},
			Team: func(t *testing.T, orgID string) string {
				t.Helper()
				id := uuid.NewString()
				if _, err := conn.Exec(`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`, id, orgID, id, "team-"+id[:8]); err != nil {
					t.Fatalf("seed team: %v", err)
				}
				return id
			},
		}
		return sqlitestore.New(conn).Teams, seed
	})
}
