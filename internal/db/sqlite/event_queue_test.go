package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
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
// guard on the org-scoped methods. Claim and PruneSettled are cross-org
// system sweeps and take no orgID, so they're excluded by design; the
// holder verbs carry the org on their receipt.
func TestEventQueueStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForEventQueueTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	ctx := t.Context()
	bogusReceipt := workitem.Receipt{ItemID: 1, OrgID: bogusOrg, LeaseGeneration: 1}

	// assertLocalOrg runs before any DB work, so an empty event is fine.
	if _, err := stores.EventQueue.Enqueue(ctx, bogusOrg, domain.Event{}, ""); err == nil {
		t.Errorf("Enqueue with non-local orgID should error")
	}
	if _, _, err := stores.EventQueue.EnqueueBatchWithSnapshotCAS(ctx, bogusOrg, "e", "{}", 0, nil, nil); err == nil {
		t.Errorf("EnqueueBatchWithSnapshotCAS with non-local orgID should error")
	}
	if _, err := stores.EventQueue.RenewLease(ctx, bogusReceipt); err == nil {
		t.Errorf("RenewLease with a non-local receipt should error")
	}
	if err := stores.EventQueue.MarkDone(ctx, bogusReceipt); err == nil {
		t.Errorf("MarkDone with a non-local receipt should error")
	}
	if _, err := stores.EventQueue.Requeue(ctx, bogusReceipt, workitem.OutcomeTransient, nil); err == nil {
		t.Errorf("Requeue with a non-local receipt should error")
	}
	if _, err := stores.EventQueue.ListForEntity(ctx, bogusOrg, "e"); err == nil {
		t.Errorf("ListForEntity with non-local orgID should error")
	}
	if _, _, err := stores.EventQueue.ListParked(ctx, bogusOrg, db.ListOpts{Limit: 50}); err == nil {
		t.Errorf("ListParked with non-local orgID should error")
	}
	if _, err := stores.EventQueue.GetParked(ctx, bogusOrg, 1); err == nil {
		t.Errorf("GetParked with non-local orgID should error")
	}
	// Non-empty ids: the guard must run before the per-id loop, or a
	// cross-org redrive would pass by looking like a no-op.
	if _, err := stores.EventQueue.Redrive(ctx, bogusOrg, []int64{1}, "operator"); err == nil {
		t.Errorf("Redrive with non-local orgID should error")
	}
	if _, err := stores.EventQueue.UnsettledCloseExistsSystem(ctx, bogusOrg, "e"); err == nil {
		t.Errorf("UnsettledCloseExistsSystem with non-local orgID should error")
	}
}

func newSQLiteForEventQueueTest(t *testing.T) *sql.DB {
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
	return conn
}

// execOne runs a statement that must touch exactly one row.
func execOne(t *testing.T, conn *sql.DB, what, query string, args ...any) {
	t.Helper()
	res, err := conn.Exec(query, args...)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("%s touched %d rows, want 1", what, n)
	}
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
	return dbtest.EventQueueSeeder{
		Entity: entity,
		ExpireLease: func(t *testing.T, queueID int64) {
			t.Helper()
			// Rewound on the database's own clock, in the block's layout,
			// which is the clock and the shape the claim's guard reads.
			execOne(t, conn, "expire lease",
				`UPDATE event_queue SET lease_expires_at = strftime('%Y-%m-%d %H:%M:%f','now','-1 hours') WHERE id = ? AND status = 'leased'`, queueID)
		},
		Ripen: func(t *testing.T, queueID int64) {
			t.Helper()
			execOne(t, conn, "ripen", `UPDATE event_queue SET next_attempt_at = NULL WHERE id = ? AND status = 'ready'`, queueID)
		},
		RequestCancel: func(t *testing.T, queueID int64) {
			t.Helper()
			execOne(t, conn, "request cancel",
				`UPDATE event_queue SET cancel_requested_at = strftime('%Y-%m-%d %H:%M:%f','now'), cancel_requested_by = 'operator', cancel_reason = 'test' WHERE id = ?`, queueID)
		},
		ClearEntityRef: func(t *testing.T, queueID int64) {
			t.Helper()
			execOne(t, conn, "clear entity_id", `UPDATE event_queue SET entity_id = NULL WHERE id = ?`, queueID)
		},
		KeyedRow: func(t *testing.T, entityID string) {
			t.Helper()
			eventID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json)
				VALUES (?, ?, ?, '', '{}')
			`, eventID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
				t.Fatalf("seed keyed event: %v", err)
			}
			if _, err := conn.Exec(`
				INSERT INTO event_queue (event_id, entity_id, event_type, status, max_attempts, unique_key, first_enqueued_at, created_at)
				VALUES (?, ?, ?, 'ready', 5, ?, strftime('%Y-%m-%d %H:%M:%f','now'), strftime('%Y-%m-%d %H:%M:%f','now'))
			`, eventID, entityID, domain.EventGitHubPRCICheckFailed, workkinds.EventQueueCloseOwedKey(entityID)); err != nil {
				t.Fatalf("seed keyed row: %v", err)
			}
		},
		EntitySnapshot: func(t *testing.T, entityID string) (string, int64) {
			t.Helper()
			var snap sql.NullString
			var seq int64
			if err := conn.QueryRow(`SELECT snapshot_json, poll_seq FROM entities WHERE id = ?`, entityID).
				Scan(&snap, &seq); err != nil {
				t.Fatalf("read entity snapshot: %v", err)
			}
			return snap.String, seq
		},
		CountEventRows: func(t *testing.T, entityID string) int {
			t.Helper()
			var n int
			if err := conn.QueryRow(`SELECT COUNT(*) FROM events WHERE entity_id = ?`, entityID).Scan(&n); err != nil {
				t.Fatalf("count events: %v", err)
			}
			return n
		},
	}
}
