package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// TestDashboardBackfillMarker_SQLite runs the shared TFAC-396 marker
// conformance against the SQLite impl. Each subtest gets a fresh in-memory DB;
// the user row is seeded via raw SQL (no store method creates users) while the
// marker round-trip and login-change reset go through the real UsersStore.
func TestDashboardBackfillMarker_SQLite(t *testing.T) {
	dbtest.RunDashboardBackfillMarkerConformance(t, func(t *testing.T) (db.UsersStore, func(t *testing.T) string) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seedUser := func(t *testing.T) string {
			t.Helper()
			id := uuid.NewString()
			if _, err := conn.Exec(`INSERT INTO users (id, display_name) VALUES (?, ?)`, id, "u-"+id[:8]); err != nil {
				t.Fatalf("seed user: %v", err)
			}
			return id
		}
		return stores.Users, seedUser
	})
}
