package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestSnoozeVisibility_Postgres is the Postgres half of the snooze-visibility
// conformance suite — see the SQLite twin.
func TestSnoozeVisibility_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunSnoozeVisibilityConformance(t, func(t *testing.T) (db.TaskStore, string, dbtest.QueuedTaskSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, _ := seedPgOrgUserAgent(t, h)
		seed := func(t *testing.T) string {
			t.Helper()
			return seedPgSourceTask(t, h.AdminDB, orgID, userID, "github", "{}")
		}
		return pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).Tasks, orgID, seed
	})
}
