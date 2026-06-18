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
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDashboardStore_SQLite runs the shared dashboard conformance
// suite against the SQLite impl. The seeder writes each PRSnapshot
// into entities.snapshot_json so DashboardStore reads from the
// production-shaped source.
func TestDashboardStore_SQLite(t *testing.T) {
	dbtest.RunDashboardStoreConformance(t, func(t *testing.T) (db.DashboardStore, string, dbtest.PRSnapshotSeederForDashboard) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := func(t *testing.T, snap domain.PRSnapshot) {
			t.Helper()
			seedSQLitePRSnapshot(t, conn, snap)
		}
		return stores.Dashboard, runmode.LocalDefaultOrgID, seed
	})
}

// seedSQLitePRSnapshot serializes the snapshot and inserts an
// entity row carrying it as snapshot_json. The harness doesn't
// care about the entity columns beyond what dashboard reads from.
func seedSQLitePRSnapshot(t *testing.T, conn *sql.DB, snap domain.PRSnapshot) {
	t.Helper()
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	now := time.Now().UTC()
	entityID := uuid.New().String()
	sourceID := fmt.Sprintf("dashboard-conformance-%d-%d", snap.Number, now.UnixNano())
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at, last_polled_at)
		VALUES (?, 'github', ?, 'pr', ?, ?, ?, ?, ?)
	`, entityID, sourceID, snap.Title, snap.URL, string(blob), now, now); err != nil {
		t.Fatalf("seed entity for snapshot %d: %v", snap.Number, err)
	}
}
