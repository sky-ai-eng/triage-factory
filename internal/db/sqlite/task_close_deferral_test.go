package sqlite_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTaskCloseDeferral_SQLite runs the shared close-deferral suite against
// the SQLite impl, a fresh in-memory DB per subtest.
func TestTaskCloseDeferral_SQLite(t *testing.T) {
	dbtest.RunTaskCloseDeferralConformance(t, func(t *testing.T) (dbtest.TaskCloseDeferralStores, string, dbtest.TaskCloseDeferralSeeder) {
		t.Helper()
		conn, err := sql.Open("sqlite", db.TestDSNMemory)
		if err != nil {
			t.Fatalf("open in-memory db: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		conn.SetMaxOpenConns(1)
		conn.SetMaxIdleConns(1)
		if err := db.BootstrapSchemaForTest(conn); err != nil {
			t.Fatalf("bootstrap schema: %v", err)
		}
		stores := sqlitestore.New(conn)
		seed := dbtest.TaskCloseDeferralSeeder{
			EntityTerminalCloseSeeder: newSQLiteTerminalCloseSeeder(conn),
			SetDeferralColumns: func(t *testing.T, taskID, eventID, blueprintRunID string) error {
				t.Helper()
				_, err := conn.Exec(`
					UPDATE tasks SET close_deferred_event_id = NULLIF(?, ''), close_deferred_blueprint_run_id = NULLIF(?, '')
					WHERE id = ?
				`, eventID, blueprintRunID, taskID)
				return err
			},
		}
		return dbtest.TaskCloseDeferralStores{
			Tasks:           stores.Tasks,
			Entities:        stores.Entities,
			Blueprints:      stores.Blueprints,
			ExternalActions: stores.ExternalActions,
		}, runmode.LocalDefaultOrgID, seed
	})
}
