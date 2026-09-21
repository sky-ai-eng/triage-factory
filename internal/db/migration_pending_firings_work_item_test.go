package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
)

// The migration that moves pending_firings onto the work-item block, and the
// version just before it, at which the test seeds the previous shape.
const (
	pendingFiringsWorkItemFile  = "migrations-sqlite/202609220001_pending_firings_work_item.sql"
	pendingFiringsWorkItemPrior = 202609210001
)

// TestMigrate_PendingFiringsAdoptWorkItemBlock pins the data mapping cell by
// cell: one row per previous status, each carrying a timestamp in a
// different layout, two draining rows (one with a claim stamp, one without),
// and a pending row sharing its (task, trigger) with a fired row. Both
// draining rows must be claimable straight after the migration as reclaims,
// with no recovery path of their own; the unique index must create over the
// shared key and collapse a duplicate admitted under the store's spelling of
// it; and a second Migrate must be a no-op.
//
// Foreign keys are off: the rows are staged as the older schema held them,
// and the chain a firing references (entity, task, handler, event, run) is
// not what this test is about.
func TestMigrate_PendingFiringsAdoptWorkItemBlock(t *testing.T) {
	database, err := sql.Open("sqlite", TestDSNMemoryNoForeignKeys)
	if err != nil {
		t.Fatalf("open sqlite memory: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	goose.SetBaseFS(migrationsSQLiteFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpTo(database, "migrations-sqlite", pendingFiringsWorkItemPrior); err != nil {
		t.Fatalf("goose UpTo %d: %v", pendingFiringsWorkItemPrior, err)
	}
	// An earlier migration's table rebuild switches enforcement back on
	// behind the DSN's setting.
	if _, err := database.Exec(`PRAGMA foreign_keys = off`); err != nil {
		t.Fatalf("foreign_keys off: %v", err)
	}

	// Nothing may reference pending_firings by foreign key, trigger or view:
	// the migration swaps the table out from under its name.
	var referrers int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE name <> 'pending_firings' AND type IN ('table','trigger','view')
		  AND (sql LIKE '%REFERENCES pending_firings%' OR (type <> 'table' AND sql LIKE '%pending_firings%'))
	`).Scan(&referrers); err != nil {
		t.Fatalf("scan referrers: %v", err)
	}
	if referrers != 0 {
		t.Fatalf("%d schema objects reference pending_firings; the table rebuild would orphan them", referrers)
	}

	for _, stmt := range []string{
		// One row per previous status, each timestamp in a different layout:
		// CURRENT_TIMESTAMP's second resolution, the driver's nanoseconds with
		// an offset, RFC3339, and millisecond text.
		`INSERT INTO pending_firings (id, entity_id, task_id, trigger_id, triggering_event_id, status, queued_at)
			VALUES (1, 'e1', 't1', 'tr1', 'ev-1', 'pending', '2026-09-01 10:00:00')`,
		`INSERT INTO pending_firings (id, entity_id, task_id, trigger_id, triggering_event_id, status, queued_at, claimed_at)
			VALUES (2, 'e1', 't2', 'tr2', 'ev-2', 'draining', '2026-09-01 10:00:01.123456789+00:00', '2026-09-01 10:00:02.5')`,
		`INSERT INTO pending_firings (id, entity_id, task_id, trigger_id, triggering_event_id, status, queued_at)
			VALUES (3, 'e1', 't3', 'tr3', 'ev-3', 'draining', '2026-09-01T10:00:03Z')`,
		`INSERT INTO pending_firings (id, entity_id, task_id, trigger_id, triggering_event_id, status, queued_at, drained_at, fired_run_id)
			VALUES (4, 'e1', 't1', 'tr1', 'ev-4', 'fired', '2026-09-01 10:00:04', '2026-09-01 10:00:05.25', 'br-4')`,
		`INSERT INTO pending_firings (id, entity_id, task_id, trigger_id, triggering_event_id, status, queued_at, drained_at, skip_reason)
			VALUES (5, 'e2', 't5', 'tr5', 'ev-5', 'skipped_stale', '2026-09-01 10:00:06', '2026-09-01 10:00:07', 'task_closed')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	if err := goose.Up(database, "migrations-sqlite"); err != nil {
		t.Fatalf("goose Up: %v", err)
	}

	type row struct {
		status, attempt, maxAttempts, generation        string
		owner, epoch, leasedAt, expiresAt               sql.NullString
		nextAt, cancelAt, cancelBy, cancelReason        sql.NullString
		lastError, lastOutcome, uniqueKey, supersededBy sql.NullString
		firstEnqueuedAt, createdAt                      string
		doneAt, skipReason, firedRunID                  sql.NullString
		entityID, taskID, triggerID, eventID            string
	}
	read := func(id int64) row {
		t.Helper()
		var r row
		if err := database.QueryRow(`
			SELECT status, attempt, max_attempts, lease_generation,
			       lease_owner, lease_epoch, leased_at, lease_expires_at,
			       next_attempt_at, cancel_requested_at, cancel_requested_by, cancel_reason,
			       last_error, last_outcome, unique_key, superseded_by,
			       first_enqueued_at, created_at, done_at, skip_reason, fired_run_id,
			       entity_id, task_id, trigger_id, triggering_event_id
			FROM pending_firings WHERE id = ?`, id).Scan(
			&r.status, &r.attempt, &r.maxAttempts, &r.generation,
			&r.owner, &r.epoch, &r.leasedAt, &r.expiresAt,
			&r.nextAt, &r.cancelAt, &r.cancelBy, &r.cancelReason,
			&r.lastError, &r.lastOutcome, &r.uniqueKey, &r.supersededBy,
			&r.firstEnqueuedAt, &r.createdAt, &r.doneAt, &r.skipReason, &r.firedRunID,
			&r.entityID, &r.taskID, &r.triggerID, &r.eventID,
		); err != nil {
			t.Fatalf("read row %d: %v", id, err)
		}
		return r
	}
	want := func(id int64, field, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("row %d %s = %q, want %q", id, field, got, want)
		}
	}
	wantNull := func(id int64, field string, v sql.NullString) {
		t.Helper()
		if v.Valid {
			t.Errorf("row %d %s = %q, want NULL", id, field, v.String)
		}
	}
	const layout = "2006-01-02 15:04:05.000"
	wantTime := func(id int64, field string, v string) {
		t.Helper()
		if _, err := time.Parse(layout, v); err != nil {
			t.Errorf("row %d %s = %q is not in the block's layout: %v", id, field, v, err)
		}
	}

	// Every row: the block defaults, both enqueue timestamps equal and
	// normalized, the key in the store's spelling, cancellation and
	// supersession empty, no error or outcome invented.
	for id := int64(1); id <= 5; id++ {
		r := read(id)
		want(id, "max_attempts", r.maxAttempts, "5")
		wantNull(id, "next_attempt_at", r.nextAt)
		wantNull(id, "cancel_requested_at", r.cancelAt)
		wantNull(id, "cancel_requested_by", r.cancelBy)
		wantNull(id, "cancel_reason", r.cancelReason)
		wantNull(id, "superseded_by", r.supersededBy)
		wantNull(id, "last_error", r.lastError)
		wantNull(id, "last_outcome", r.lastOutcome)
		wantNull(id, "lease_owner", r.owner)
		wantNull(id, "lease_epoch", r.epoch)
		wantTime(id, "first_enqueued_at", r.firstEnqueuedAt)
		want(id, "created_at", r.createdAt, r.firstEnqueuedAt)
		want(id, "unique_key", r.uniqueKey.String, workkinds.PendingFiringKey(r.taskID, r.triggerID))
		if r.doneAt.Valid {
			wantTime(id, "done_at", r.doneAt.String)
		}
	}

	// pending -> ready.
	r := read(1)
	want(1, "status", r.status, workitem.StatusReady)
	want(1, "attempt", r.attempt, "0")
	want(1, "lease_generation", r.generation, "0")
	want(1, "first_enqueued_at", r.firstEnqueuedAt, "2026-09-01 10:00:00.000")
	wantNull(1, "leased_at", r.leasedAt)
	wantNull(1, "lease_expires_at", r.expiresAt)
	wantNull(1, "done_at", r.doneAt)
	wantNull(1, "skip_reason", r.skipReason)
	wantNull(1, "fired_run_id", r.firedRunID)
	want(1, "entity_id", r.entityID, "e1")
	want(1, "triggering_event_id", r.eventID, "ev-1")

	// draining -> leased, expired, the claim stamp carried onto leased_at.
	for _, id := range []int64{2, 3} {
		r = read(id)
		want(id, "status", r.status, workitem.StatusLeased)
		want(id, "attempt", r.attempt, "1")
		want(id, "lease_generation", r.generation, "1")
		wantTime(id, "lease_expires_at", r.expiresAt.String)
		if exp, _ := time.Parse(layout, r.expiresAt.String); !exp.Before(time.Now().UTC()) {
			t.Errorf("row %d lease_expires_at = %s is not already in the past", id, r.expiresAt.String)
		}
		wantNull(id, "done_at", r.doneAt)
	}
	r = read(2)
	want(2, "leased_at", r.leasedAt.String, "2026-09-01 10:00:02.500")
	want(2, "first_enqueued_at", r.firstEnqueuedAt, "2026-09-01 10:00:01.123")
	r = read(3)
	wantNull(3, "leased_at", r.leasedAt)
	want(3, "first_enqueued_at", r.firstEnqueuedAt, "2026-09-01 10:00:03.000")

	// fired -> done, the run kept.
	r = read(4)
	want(4, "status", r.status, workitem.StatusDone)
	want(4, "attempt", r.attempt, "0")
	want(4, "done_at", r.doneAt.String, "2026-09-01 10:00:05.250")
	want(4, "fired_run_id", r.firedRunID.String, "br-4")
	wantNull(4, "skip_reason", r.skipReason)
	wantNull(4, "leased_at", r.leasedAt)

	// skipped_stale -> done, the reason kept.
	r = read(5)
	want(5, "status", r.status, workitem.StatusDone)
	want(5, "done_at", r.doneAt.String, "2026-09-01 10:00:07.000")
	want(5, "skip_reason", r.skipReason.String, "task_closed")
	wantNull(5, "fired_run_id", r.firedRunID)

	// The unique index created over a table holding one ready and one done
	// row for the same key, and a second admission under the store's spelling
	// of that key collapses onto the ready row.
	kind := workkinds.PendingFirings(workitem.SQLite)
	ctx := context.Background()
	id, deduplicated, err := workitem.Admit(ctx, database, kind, "00000000-0000-0000-0000-000000000001",
		workkinds.PendingFiringKey("t1", "tr1"),
		map[string]any{"entity_id": "e1", "task_id": "t1", "trigger_id": "tr1", "triggering_event_id": "ev-9"})
	if err != nil {
		t.Fatalf("Admit after migration: %v", err)
	}
	if !deduplicated || id != 1 {
		t.Errorf("Admit of the migrated key = (id %d, deduplicated %v), want collapsed onto row 1", id, deduplicated)
	}

	// Both expired leases are reclaimed by an ordinary claim, and say so; the
	// ready row is claimed beside them.
	res, err := workitem.Claim(ctx, database, kind, workitem.Owner{ID: "pod-b", Epoch: 1}, "", 10)
	if err != nil {
		t.Fatalf("Claim after migration: %v", err)
	}
	if res.Reclaimed != 2 {
		t.Errorf("claim reclaimed %d rows, want the two migrated draining rows", res.Reclaimed)
	}
	if got := len(res.Claimed); got != 3 {
		t.Errorf("claim returned %d receipts, want the ready row and the two reclaimed rows", got)
	}
	for _, rc := range res.Claimed {
		switch rc.ItemID {
		case 2, 3:
			if !rc.Reclaimed || rc.PreviousOwner != "" {
				t.Errorf("row %d receipt: reclaimed=%v previous=%q, want true and no previous owner", rc.ItemID, rc.Reclaimed, rc.PreviousOwner)
			}
			if rc.Attempt != 2 || rc.LeaseGeneration != 2 {
				t.Errorf("row %d receipt attempt=%d generation=%d, want 2/2", rc.ItemID, rc.Attempt, rc.LeaseGeneration)
			}
		case 1:
			if rc.Reclaimed || rc.Attempt != 1 || rc.LeaseGeneration != 1 {
				t.Errorf("row 1 receipt = %+v, want a fresh first acquisition", rc)
			}
		default:
			t.Errorf("claim returned row %d, which was terminal", rc.ItemID)
		}
	}

	// The indexes the kind requires are on the table, and the swap left the
	// name in place.
	names := map[string]bool{}
	rows, err := database.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'pending_firings'`)
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		names[n] = true
	}
	rows.Close()
	for _, n := range append(workitem.IndexNames(kind), "idx_pending_firings_task_unsettled") {
		if !names[n] {
			t.Errorf("index %s missing after migration (present: %v)", n, names)
		}
	}
	var leftover int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'pending_firings_new'`).Scan(&leftover); err != nil {
		t.Fatalf("scan leftover: %v", err)
	}
	if leftover != 0 {
		t.Error("pending_firings_new survived the rename")
	}

	// A second Migrate is a no-op.
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if r := read(1); r.status != workitem.StatusLeased {
		t.Errorf("row 1 after a second Migrate = %q, want the claim's leased row untouched", r.status)
	}
}
