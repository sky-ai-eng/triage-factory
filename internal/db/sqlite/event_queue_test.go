package sqlite_test

import (
	"database/sql"
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

// TestEventQueueStore_SQLite runs the shared conformance suite against
// the SQLite EventQueueStore impl. Each subtest gets a fresh in-memory
// DB; the seeder inserts entity rows inline (Enqueue itself writes the
// events audit row + the queue row).
func TestEventQueueStore_SQLite(t *testing.T) {
	dbtest.RunEventQueueStoreConformance(t, func(t *testing.T) (db.EventQueueStore, string, dbtest.EventQueueSeeder) {
		t.Helper()
		conn := newSQLiteForEventQueueTest(t)
		stores := sqlitestore.New(conn)
		return stores.EventQueue, runmode.LocalDefaultOrgID, newSQLiteEventQueueSeeder(conn)
	})
}

// TestEventQueueStore_SQLite_RejectsNonLocalOrg pins the assertLocalOrg
// guard on the org-scoped methods. ClaimNext / ResetProcessing /
// PruneDone are cross-org system sweeps and take no orgID, so they're
// excluded by design.
func TestEventQueueStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForEventQueueTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	ctx := t.Context()

	// assertLocalOrg runs before any DB work, so an empty event is fine.
	if _, err := stores.EventQueue.Enqueue(ctx, bogusOrg, domain.Event{}, ""); err == nil {
		t.Errorf("Enqueue with non-local orgID should error")
	}
	if err := stores.EventQueue.MarkDone(ctx, bogusOrg, 1); err == nil {
		t.Errorf("MarkDone with non-local orgID should error")
	}
	if err := stores.EventQueue.MarkFailed(ctx, bogusOrg, 1, "x"); err == nil {
		t.Errorf("MarkFailed with non-local orgID should error")
	}
	if err := stores.EventQueue.Requeue(ctx, bogusOrg, 1, "x"); err == nil {
		t.Errorf("Requeue with non-local orgID should error")
	}
	if _, err := stores.EventQueue.ListForEntity(ctx, bogusOrg, "e"); err == nil {
		t.Errorf("ListForEntity with non-local orgID should error")
	}
}

func newSQLiteForEventQueueTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}

// newSQLiteEventQueueSeeder returns a closure-bound seeder. Every Entity
// call inserts a fresh entity so the (source, source_id) UNIQUE stays
// happy across subtests.
func newSQLiteEventQueueSeeder(conn *sql.DB) dbtest.EventQueueSeeder {
	entity := func(t *testing.T) string {
		t.Helper()
		suf := uuid.New().String()[:8]
		entityID := "e-" + suf
		if _, err := conn.Exec(`
			INSERT INTO entities (id, source, source_id, kind, title, url)
			VALUES (?, 'github', ?, 'pr', 'Test PR', '')
		`, entityID, "owner/repo#"+suf); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		return entityID
	}
	backdateClaim := func(t *testing.T, queueID int64, age time.Duration) {
		t.Helper()
		// Local is one process, so the claim's clock and the sweep's clock
		// are the same one — rewind it in Go the way the store does.
		res, err := conn.Exec(`UPDATE event_queue SET claimed_at = ? WHERE id = ?`,
			time.Now().Add(-age), queueID)
		if err != nil {
			t.Fatalf("backdate claimed_at: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("backdate claimed_at touched %d rows, want 1", n)
		}
	}
	return dbtest.EventQueueSeeder{Entity: entity, BackdateClaim: backdateClaim}
}
