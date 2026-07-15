package sqlite_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestCuratorTurnCredentials_SQLite runs the shared per-turn credential
// conformance against the SQLite impl. Each factory call opens a
// fresh DB so subtests don't share state.
func TestCuratorTurnCredentials_SQLite(t *testing.T) {
	dbtest.RunCuratorTurnCredentialsConformance(t, func(t *testing.T) (db.CuratorStore, db.CuratorTurnCredentialsStore, string, dbtest.CuratorTurnCredentialsSeeder) {
		t.Helper()
		conn := newSQLiteForCuratorTest(t)
		stores := sqlitestore.New(conn)
		ctx := context.Background()
		org := runmode.LocalDefaultOrgID
		user := runmode.LocalDefaultUserID

		projectID, err := stores.Projects.Create(ctx, org, runmode.LocalDefaultTeamID, domain.Project{Name: "ctc"})
		if err != nil {
			t.Fatalf("create project: %v", err)
		}
		const homeID = "home-executor"
		epoch, err := stores.Instances.Register(ctx, homeID, "executor", "test-build", "test-pubkey")
		if err != nil {
			t.Fatalf("register instance: %v", err)
		}

		seed := dbtest.CuratorTurnCredentialsSeeder{
			NewTurn: func(t *testing.T) string {
				t.Helper()
				id, err := stores.Curator.CreateRequest(ctx, org, projectID, user, homeID, "hi")
				if err != nil {
					t.Fatalf("CreateRequest: %v", err)
				}
				return id
			},
			ForceStatus: func(t *testing.T, requestID, status string) {
				t.Helper()
				if _, err := conn.Exec(`UPDATE curator_requests SET status = ? WHERE id = ?`, status, requestID); err != nil {
					t.Fatalf("force status %q: %v", status, err)
				}
			},
			HomeInstanceID: homeID,
			HomeBootEpoch:  epoch,
		}
		return stores.Curator, stores.CuratorTurnCredentials, org, seed
	})
}
