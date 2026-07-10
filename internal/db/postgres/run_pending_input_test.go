package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestRunPendingInputStore_Postgres runs the shared conformance suite
// against the Postgres RunPendingInputStore impl, wired against AdminDB
// (production wiring is admin-pool only — see the store's doc comment).
// Skips cleanly when Docker isn't available (pgtest.Shared).
func TestRunPendingInputStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunRunPendingInputStoreConformance(t, func(t *testing.T) (db.RunPendingInputStore, string, string, dbtest.RunPendingInputSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		seed := dbtest.RunPendingInputSeeder{
			Run: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedPgArtifactRun(t, h, orgID, teamID, userID)
			},
			DeleteRun: func(t *testing.T, runID string) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`DELETE FROM runs WHERE id = $1`, runID); err != nil {
					t.Fatalf("delete run: %v", err)
				}
			},
		}
		return stores.RunPendingInput, orgID, userID, seed
	})
}
