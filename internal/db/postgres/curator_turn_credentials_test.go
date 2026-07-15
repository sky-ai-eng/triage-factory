package postgres_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestCuratorTurnCredentials_Postgres runs the shared per-turn credential
// conformance against the Postgres impl (admin pool — the brain
// writes and the executor reads/deletes never carry JWT claims). Each factory
// call resets the harness so subtests don't share state.
func TestCuratorTurnCredentials_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	ctx := context.Background()

	dbtest.RunCuratorTurnCredentialsConformance(t, func(t *testing.T) (db.CuratorStore, db.CuratorTurnCredentialsStore, string, dbtest.CuratorTurnCredentialsSeeder) {
		t.Helper()
		h.Reset(t)
		// Both pools on AdminDB so CreateRequest (app-pool in production) runs
		// without JWT claims in the harness, exactly like the run_queue
		// credentials conformance factory does.
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		orgID, userID, _ := seedPgProjectOrg(t, h)
		projectID := seedPgEntityProject(t, h, orgID, userID, "ctc")

		const homeID = "home-executor"
		epoch, err := stores.Instances.Register(ctx, homeID, "executor", "test-build", "test-pubkey")
		if err != nil {
			t.Fatalf("register instance: %v", err)
		}

		seed := dbtest.CuratorTurnCredentialsSeeder{
			NewTurn: func(t *testing.T) string {
				t.Helper()
				id, err := stores.Curator.CreateRequest(ctx, orgID, projectID, userID, homeID, "hi")
				if err != nil {
					t.Fatalf("CreateRequest: %v", err)
				}
				return id
			},
			ForceStatus: func(t *testing.T, requestID, status string) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`UPDATE curator_requests SET status = $1 WHERE id = $2`, status, requestID); err != nil {
					t.Fatalf("force status %q: %v", status, err)
				}
			},
			HomeInstanceID: homeID,
			HomeBootEpoch:  epoch,
		}
		return stores.Curator, stores.CuratorTurnCredentials, orgID, seed
	})
}
