package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestShippedSync_Postgres runs the shared shipped-defaults sync suite against
// the Postgres impl. Both pools wire to AdminDB (the sync is claims-less admin
// work); each subtest gets a fresh org + default team via h.Reset. resetBackfill
// clears teams.shipped_defaults_backfilled_at so the grandfather backfill is
// exercisable.
func TestShippedSync_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	factory := func(t *testing.T) (db.Stores, string, string, func(*testing.T)) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgOrgForBlueprints(t, h)
		seedPgDefaultTeam(t, h, orgID, userID)
		teamID := firstTeamForOrg(t, h, orgID)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		reset := func(t *testing.T) {
			t.Helper()
			if _, err := h.AdminDB.Exec(
				`UPDATE teams SET shipped_defaults_backfilled_at = NULL WHERE id = $1::uuid`, teamID,
			); err != nil {
				t.Fatalf("reset backfill marker: %v", err)
			}
		}
		return stores, orgID, teamID, reset
	}
	dbtest.RunShippedSyncConformance(t, factory)
	dbtest.RunShippedHandlerSyncConformance(t, factory)
}
