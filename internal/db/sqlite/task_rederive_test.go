package sqlite_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTaskReDeriveStore_SQLite runs the shared conformance suite against the
// SQLite TaskReDeriveStore impl. Each subtest gets a fresh in-memory DB; the
// task chains come from the pending-firings seeder, since a completion's
// effect is a firing admission and needs the same parents.
//
// Of the two orders of the score-versus-completion race, only the first is
// expressible here — the score commits between the claim and the completion,
// which the suite's ErrScoreMoved subtest covers — because the handle's one
// connection serializes the two transactions: a score write cannot be in
// flight while a completion holds the file. The second order is a Postgres
// test.
func TestTaskReDeriveStore_SQLite(t *testing.T) {
	dbtest.RunTaskReDeriveStoreConformance(t, func(t *testing.T) dbtest.TaskReDeriveFixture {
		t.Helper()
		conn := newSQLiteForPendingFiringsTest(t)
		stores := sqlitestore.New(conn)
		seed := newSQLitePendingFiringsSeeder(conn, stores)
		return dbtest.TaskReDeriveFixture{
			Store:   stores.TaskReDerive,
			Scores:  stores.Scores,
			Firings: stores.PendingFirings,
			OrgID:   runmode.LocalDefaultOrgID,
			Tuple:   seed.Tuple,
			QueueRows: func(t *testing.T, taskID string) []dbtest.ReDeriveQueueRow {
				t.Helper()
				return readSQLiteReDeriveRows(t, conn, taskID)
			},
			ExpireLease: func(t *testing.T, id int64) {
				t.Helper()
				execOne(t, conn, "expire lease",
					`UPDATE task_rederive_queue SET lease_expires_at = strftime('%Y-%m-%d %H:%M:%f','now','-1 hours') WHERE id = ? AND status = 'leased'`, id)
			},
		}
	})
}

// TestTaskReDeriveStore_SQLite_RejectsNonLocalOrg pins the assertLocalOrg
// guard on every receipt-addressed verb and on Describe.
func TestTaskReDeriveStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForPendingFiringsTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	ctx := t.Context()
	bogus := workitem.Receipt{ItemID: 1, OrgID: bogusOrg, LeaseGeneration: 1, Frozen: map[string]any{"requested_revision": int64(1)}}

	if _, err := stores.TaskReDerive.RenewLease(ctx, bogus); err == nil {
		t.Error("RenewLease with non-local orgID should error")
	}
	if err := stores.TaskReDerive.Complete(ctx, bogus, func(db.PendingFiringsStore) error { return nil }); err == nil {
		t.Error("Complete with non-local orgID should error")
	}
	if err := stores.TaskReDerive.DeferScoreMoved(ctx, bogus); err == nil {
		t.Error("DeferScoreMoved with non-local orgID should error")
	}
	if _, err := stores.TaskReDerive.Requeue(ctx, bogus, workitem.OutcomeTransient, nil); err == nil {
		t.Error("Requeue with non-local orgID should error")
	}
	if _, err := stores.TaskReDerive.(db.WorkKindHandle).Describe(ctx, bogusOrg, []int64{1}); err == nil {
		t.Error("Describe with non-local orgID should error")
	}
}

// TestTaskReDeriveWorkItem_SQLite runs the table-agnostic half of the
// work-item conformance suite against the production task_rederive_queue
// table, as the migrated schema builds it, under the kind production
// declares. Each admission needs a task for the table's foreign key, which
// the pending-firings seeder's tuple helper inserts along with the rest of
// its chain.
func TestTaskReDeriveWorkItem_SQLite(t *testing.T) {
	workitemtest.RunTable(t, func(t *testing.T) (*sql.DB, workitem.Kind, string, func(int) map[string]any) {
		t.Helper()
		conn := newSQLiteForPendingFiringsTest(t)
		seed := newSQLitePendingFiringsSeeder(conn, sqlitestore.New(conn))
		cols := func(int) map[string]any {
			return db.TaskReDeriveRowCols(seed.Tuple(t).TaskID)
		}
		return conn, workkinds.TaskReDerive(workitem.SQLite), runmode.LocalDefaultOrgID, cols
	})
}
