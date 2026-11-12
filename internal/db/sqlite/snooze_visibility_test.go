package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSnoozeVisibility_SQLite runs the snooze-visibility conformance suite —
// the seam between "SnoozeTask wrote a wake time" and "List hides the row
// until it arrives" — against the SQLite store.
func TestSnoozeVisibility_SQLite(t *testing.T) {
	dbtest.RunSnoozeVisibilityConformance(t, func(t *testing.T) (db.TaskStore, string, dbtest.QueuedTaskSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		seed := func(t *testing.T) string {
			t.Helper()
			return seedSQLiteSourceTask(t, conn, "github", "{}")
		}
		return sqlitestore.New(conn).Tasks, runmode.LocalDefaultOrgID, seed
	})
}
