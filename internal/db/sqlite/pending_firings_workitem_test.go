package sqlite_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestPendingFiringsWorkItem_SQLite runs the table-agnostic half of the
// work-item conformance suite against the production pending_firings table,
// as the migrated schema builds it, under the kind production declares. Each
// admission needs the entity, task, handler and event rows the table's
// foreign keys name, which the conformance seeder's tuple helper inserts.
func TestPendingFiringsWorkItem_SQLite(t *testing.T) {
	workitemtest.RunTable(t, func(t *testing.T) (*sql.DB, workitem.Kind, string, func(int) map[string]any) {
		t.Helper()
		conn := newSQLiteForPendingFiringsTest(t)
		seed := newSQLitePendingFiringsSeeder(conn, sqlitestore.New(conn))
		cols := func(int) map[string]any {
			tup := seed.Tuple(t)
			return map[string]any{
				"entity_id":           tup.EntityID,
				"task_id":             tup.TaskID,
				"trigger_id":          tup.TriggerID,
				"triggering_event_id": tup.EventID,
			}
		}
		return conn, workkinds.PendingFirings(workitem.SQLite), runmode.LocalDefaultOrgID, cols
	})
}
