package sqlite_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDashboardStore_SQLite runs the shared dashboard conformance
// suite against the SQLite impl. The seeder writes each PRSnapshot
// into entities.snapshot_json so DashboardStore reads from the
// production-shaped source.
func TestDashboardStore_SQLite(t *testing.T) {
	dbtest.RunDashboardStoreConformance(t, func(t *testing.T) (db.DashboardStore, string, string, dbtest.DashboardSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		// The viewer is a real users row because the commissioned-by column
		// is an FK to it in both dialects; local mode's sole user is the
		// natural one to be.
		viewerID := seedSQLiteDashboardUser(t, conn, "Local Operator")
		return stores.Dashboard, runmode.LocalDefaultOrgID, viewerID, dbtest.DashboardSeeder{
			PR: func(t *testing.T, fx dbtest.DashboardPRFixture) string {
				t.Helper()
				return seedSQLiteDashboardPR(t, conn, fx)
			},
			User: func(t *testing.T, name string) string {
				t.Helper()
				return seedSQLiteDashboardUser(t, conn, name)
			},
		}
	})
}

// seedSQLiteDashboardPR serializes the fixture's snapshot and inserts an
// entity row carrying it as snapshot_json, plus the two columns the read
// consults beside it. The harness doesn't care about the entity columns
// beyond what dashboard reads from.
func seedSQLiteDashboardPR(t *testing.T, conn *sql.DB, fx dbtest.DashboardPRFixture) string {
	t.Helper()
	blob, err := json.Marshal(fx.Snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	state := fx.EntityState
	if state == "" {
		state = "active"
	}
	var commissioned any
	if fx.CommissionedBy != "" {
		commissioned = fx.CommissionedBy
	}
	now := time.Now().UTC()
	entityID := uuid.New().String()
	sourceID := fmt.Sprintf("dashboard-conformance-%d-%d", fx.Snapshot.Number, now.UnixNano())
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, state,
		                      commissioned_by_user_id, created_at, last_polled_at)
		VALUES (?, 'github', ?, 'pr', ?, ?, ?, ?, ?, ?, ?)
	`, entityID, sourceID, fx.Snapshot.Title, fx.Snapshot.URL, string(blob), state,
		commissioned, now, now); err != nil {
		t.Fatalf("seed entity for snapshot %d: %v", fx.Snapshot.Number, err)
	}
	return entityID
}

func seedSQLiteDashboardUser(t *testing.T, conn *sql.DB, name string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := conn.Exec(`INSERT INTO users (id, display_name) VALUES (?, ?)`, id, name); err != nil {
		t.Fatalf("seed user %s: %v", name, err)
	}
	return id
}
