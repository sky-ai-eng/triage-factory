package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestShippedSync_SQLite runs the shared shipped-defaults sync suite against the
// SQLite impl. The local sentinel org+team are provisioned (the sync reads
// teams.shipped_defaults_backfilled_at, so the team row must exist); resetBackfill
// clears that marker so the grandfather backfill is exercisable.
func TestShippedSync_SQLite(t *testing.T) {
	factory := func(t *testing.T) (db.Stores, string, string, func(*testing.T)) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		if err := stores.Orgs.CreateLocalTenant(t.Context()); err != nil {
			t.Fatalf("provision local tenant: %v", err)
		}
		teamID := runmode.LocalDefaultTeamID
		reset := func(t *testing.T) {
			t.Helper()
			if _, err := conn.Exec(`UPDATE teams SET shipped_defaults_backfilled_at = NULL WHERE id = ?`, teamID); err != nil {
				t.Fatalf("reset backfill marker: %v", err)
			}
		}
		return stores, runmode.LocalDefaultOrgID, teamID, reset
	}
	dbtest.RunShippedSyncConformance(t, factory)
	dbtest.RunShippedHandlerSyncConformance(t, factory)
}
