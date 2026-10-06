package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestLinearTeamRulesStore_SQLite runs the shared conformance suite against
// the SQLite impl. Local mode is one org; the second team the suite needs is
// inserted straight into teams beside the seeded default team.
func TestLinearTeamRulesStore_SQLite(t *testing.T) {
	dbtest.RunLinearTeamRulesConformance(t, func(t *testing.T) dbtest.LinearTeamRulesFixture {
		t.Helper()
		conn := openSQLiteForTest(t)
		const otherTeamID = "00000000-0000-4000-8000-0000000000b2"
		if _, err := conn.Exec(
			`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
			otherTeamID, runmode.LocalDefaultOrgID, "other", "Other",
		); err != nil {
			t.Fatalf("seed second team: %v", err)
		}
		return dbtest.LinearTeamRulesFixture{
			Store:       sqlitestore.New(conn).LinearTeamRules,
			OrgID:       runmode.LocalDefaultOrgID,
			TeamID:      runmode.LocalDefaultTeamID,
			OtherTeamID: otherTeamID,
		}
	})
}
