package sqlite_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestWorkItem_SQLite runs the shared work-item conformance suite against
// SQLite. Each subtest opens a fresh in-memory database and the suite creates
// its own fixture tables there — no production table adopts the contract yet,
// and the fixture must never reach a production schema.
func TestWorkItem_SQLite(t *testing.T) {
	workitemtest.Run(t, func(t *testing.T) (*sql.DB, workitem.Dialect, string) {
		t.Helper()
		return openSQLiteForTest(t), workitem.SQLite, runmode.LocalDefaultOrgID
	})
}
