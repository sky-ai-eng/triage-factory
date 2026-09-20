package db

import (
	"context"
	"database/sql"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
)

// The migration that moves event_queue onto the work-item block, and the
// version just before it, at which the test seeds the previous shape.
const (
	eventQueueWorkItemFile  = "migrations-sqlite/202609210001_event_queue_work_item.sql"
	eventQueueWorkItemPrior = 202609200001
)

// TestMigrate_EventQueueAdoptsWorkItemBlock pins the data mapping cell by
// cell: one row per previous status, each carrying a timestamp in a different
// layout, two parked close obligations for one entity, and a processing row
// with an owner. The processing row must be claimable straight after the
// migration as a reclaim, with no recovery path of its own, and a second
// Migrate must be a no-op.
func TestMigrate_EventQueueAdoptsWorkItemBlock(t *testing.T) {
	database := openMigrationsTestDB(t)
	goose.SetBaseFS(migrationsSQLiteFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpTo(database, "migrations-sqlite", eventQueueWorkItemPrior); err != nil {
		t.Fatalf("goose UpTo %d: %v", eventQueueWorkItemPrior, err)
	}
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}

	// Nothing may reference event_queue by foreign key, trigger or view: the
	// migration swaps the table out from under its name.
	var referrers int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE name <> 'event_queue' AND type IN ('table','trigger','view')
		  AND (sql LIKE '%REFERENCES event_queue%' OR (type <> 'table' AND sql LIKE '%event_queue%'))
	`).Scan(&referrers); err != nil {
		t.Fatalf("scan referrers: %v", err)
	}
	if referrers != 0 {
		t.Fatalf("%d schema objects reference event_queue; the table rebuild would orphan them", referrers)
	}

	const closeOwed = "system:entity:close_owed"
	for _, stmt := range []string{
		`INSERT INTO entities (id, source, source_id, kind) VALUES ('e1', 'github', 'o/r#1', 'pr')`,
		`INSERT INTO entities (id, source, source_id, kind) VALUES ('e2', 'github', 'o/r#2', 'pr')`,
		`INSERT INTO events (id, event_type, entity_id) VALUES ('ev-pending', 'github:pr:ci_check_failed', 'e1')`,
		`INSERT INTO events (id, event_type, entity_id) VALUES ('ev-processing', 'github:pr:ci_check_failed', 'e1')`,
		`INSERT INTO events (id, event_type, entity_id) VALUES ('ev-done', 'github:pr:ci_check_failed', 'e1')`,
		`INSERT INTO events (id, event_type, entity_id) VALUES ('ev-failed', 'github:pr:ci_check_failed', 'e1')`,
		`INSERT INTO events (id, event_type, entity_id) VALUES ('ev-owed-1', '` + closeOwed + `', 'e2')`,
		`INSERT INTO events (id, event_type, entity_id) VALUES ('ev-owed-2', '` + closeOwed + `', 'e2')`,
		// One row per previous status, each timestamp in a different layout:
		// CURRENT_TIMESTAMP's second resolution, the driver's nanoseconds with
		// an offset, and millisecond text.
		`INSERT INTO event_queue (id, event_id, entity_id, event_type, status, attempts, enqueued_at)
			VALUES (1, 'ev-pending', 'e1', 'github:pr:ci_check_failed', 'pending', 0, '2026-09-01 10:00:00')`,
		`INSERT INTO event_queue (id, event_id, entity_id, event_type, status, attempts, enqueued_at, claimed_at, executor_id, boot_epoch, traceparent, entity_poll_seq)
			VALUES (2, 'ev-processing', 'e1', 'github:pr:ci_check_failed', 'processing', 2, '2026-09-01 10:00:01.123456789+00:00', '2026-09-01 10:00:02.5', 'pod-a', 3, '00-abc-def-01', 7)`,
		`INSERT INTO event_queue (id, event_id, entity_id, event_type, status, attempts, enqueued_at, processed_at)
			VALUES (3, 'ev-done', 'e1', 'github:pr:ci_check_failed', 'done', 1, '2026-09-01T10:00:03Z', '2026-09-01 10:00:04')`,
		`INSERT INTO event_queue (id, event_id, entity_id, event_type, status, attempts, enqueued_at, processed_at, last_error)
			VALUES (4, 'ev-failed', 'e1', 'github:pr:ci_check_failed', 'failed', 5, '2026-09-01 10:00:05', '2026-09-01 10:00:06.25', 'route: boom (after 5 attempts)')`,
		`INSERT INTO event_queue (id, event_id, entity_id, event_type, status, attempts, enqueued_at, processed_at, last_error)
			VALUES (5, 'ev-owed-1', 'e2', '` + closeOwed + `', 'failed', 5, '2026-09-01 10:00:07', '2026-09-01 10:00:08', 'close: boom')`,
		`INSERT INTO event_queue (id, event_id, entity_id, event_type, status, attempts, enqueued_at, processed_at, last_error)
			VALUES (6, 'ev-owed-2', 'e2', '` + closeOwed + `', 'failed', 5, '2026-09-01 10:00:09', '2026-09-01 10:00:10', 'close: boom again')`,
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
		doneAt, traceparent, pollSeq                    sql.NullString
		eventID, entityID, eventType                    string
	}
	read := func(id int64) row {
		t.Helper()
		var r row
		if err := database.QueryRow(`
			SELECT status, attempt, max_attempts, lease_generation,
			       lease_owner, lease_epoch, leased_at, lease_expires_at,
			       next_attempt_at, cancel_requested_at, cancel_requested_by, cancel_reason,
			       last_error, last_outcome, unique_key, superseded_by,
			       first_enqueued_at, created_at, done_at, traceparent, entity_poll_seq,
			       event_id, entity_id, event_type
			FROM event_queue WHERE id = ?`, id).Scan(
			&r.status, &r.attempt, &r.maxAttempts, &r.generation,
			&r.owner, &r.epoch, &r.leasedAt, &r.expiresAt,
			&r.nextAt, &r.cancelAt, &r.cancelBy, &r.cancelReason,
			&r.lastError, &r.lastOutcome, &r.uniqueKey, &r.supersededBy,
			&r.firstEnqueuedAt, &r.createdAt, &r.doneAt, &r.traceparent, &r.pollSeq,
			&r.eventID, &r.entityID, &r.eventType,
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
	wantTime := func(id int64, field string, v string) {
		t.Helper()
		if _, err := time.Parse("2006-01-02 15:04:05.000", v); err != nil {
			t.Errorf("row %d %s = %q is not in the block's layout: %v", id, field, v, err)
		}
	}
	const layout = "2006-01-02 15:04:05.000"

	// Every row: the block defaults, both enqueue timestamps equal and
	// normalized, cancellation and supersession empty unless collapsed.
	for id := int64(1); id <= 6; id++ {
		r := read(id)
		want(id, "max_attempts", r.maxAttempts, "5")
		wantNull(id, "next_attempt_at", r.nextAt)
		wantNull(id, "cancel_requested_at", r.cancelAt)
		wantNull(id, "cancel_requested_by", r.cancelBy)
		wantNull(id, "cancel_reason", r.cancelReason)
		wantTime(id, "first_enqueued_at", r.firstEnqueuedAt)
		want(id, "created_at", r.createdAt, r.firstEnqueuedAt)
		if id != 5 {
			wantNull(id, "superseded_by", r.supersededBy)
		}
		if r.doneAt.Valid {
			wantTime(id, "done_at", r.doneAt.String)
		}
	}

	// pending -> ready.
	r := read(1)
	want(1, "status", r.status, workitem.StatusReady)
	want(1, "attempt", r.attempt, "0")
	want(1, "lease_generation", r.generation, "0")
	wantNull(1, "lease_owner", r.owner)
	wantNull(1, "unique_key", r.uniqueKey)
	wantNull(1, "done_at", r.doneAt)
	want(1, "first_enqueued_at", r.firstEnqueuedAt, "2026-09-01 10:00:00.000")

	// processing -> leased, expired, owner carried onto the lease.
	r = read(2)
	want(2, "status", r.status, workitem.StatusLeased)
	want(2, "attempt", r.attempt, "2")
	want(2, "lease_generation", r.generation, "1")
	want(2, "lease_owner", r.owner.String, "pod-a")
	want(2, "lease_epoch", r.epoch.String, "3")
	want(2, "leased_at", r.leasedAt.String, "2026-09-01 10:00:02.500")
	want(2, "first_enqueued_at", r.firstEnqueuedAt, "2026-09-01 10:00:01.123")
	want(2, "traceparent", r.traceparent.String, "00-abc-def-01")
	want(2, "entity_poll_seq", r.pollSeq.String, "7")
	wantTime(2, "lease_expires_at", r.expiresAt.String)
	if exp, _ := time.Parse(layout, r.expiresAt.String); !exp.Before(time.Now().UTC()) {
		t.Errorf("row 2 lease_expires_at = %s is not already in the past", r.expiresAt.String)
	}
	wantNull(2, "done_at", r.doneAt)

	// done -> done.
	r = read(3)
	want(3, "status", r.status, workitem.StatusDone)
	want(3, "attempt", r.attempt, "1")
	want(3, "first_enqueued_at", r.firstEnqueuedAt, "2026-09-01 10:00:03.000")
	want(3, "done_at", r.doneAt.String, "2026-09-01 10:00:04.000")
	wantNull(3, "lease_owner", r.owner)

	// failed -> parked, error kept, no typed outcome invented.
	r = read(4)
	want(4, "status", r.status, workitem.StatusParked)
	want(4, "attempt", r.attempt, "5")
	want(4, "last_error", r.lastError.String, "route: boom (after 5 attempts)")
	wantNull(4, "last_outcome", r.lastOutcome)
	want(4, "done_at", r.doneAt.String, "2026-09-01 10:00:06.250")
	wantNull(4, "unique_key", r.uniqueKey)

	// Two parked obligations for one entity: the highest id keeps its status
	// and its key; the other is settled as superseded by it, key kept.
	r = read(6)
	want(6, "status", r.status, workitem.StatusParked)
	want(6, "unique_key", r.uniqueKey.String, workkinds.EventQueueCloseOwedKey("e2"))
	wantNull(6, "last_outcome", r.lastOutcome)
	want(6, "last_error", r.lastError.String, "close: boom again")
	r = read(5)
	want(5, "status", r.status, workitem.StatusCancelled)
	want(5, "last_outcome", r.lastOutcome.String, "superseded")
	want(5, "superseded_by", r.supersededBy.String, "6")
	want(5, "unique_key", r.uniqueKey.String, workkinds.EventQueueCloseOwedKey("e2"))
	want(5, "last_error", r.lastError.String, "close: boom")
	wantTime(5, "done_at", r.doneAt.String)

	// The expired lease is reclaimed by an ordinary claim, and says so.
	kind := workkinds.EventQueue(workitem.SQLite)
	res, err := workitem.Claim(context.Background(), database, kind, workitem.Owner{ID: "pod-b", Epoch: 1}, "", 10)
	if err != nil {
		t.Fatalf("Claim after migration: %v", err)
	}
	if res.Reclaimed != 1 {
		t.Errorf("claim reclaimed %d rows, want the one migrated processing row", res.Reclaimed)
	}
	var reclaimed bool
	for _, rc := range res.Claimed {
		if rc.ItemID == 2 {
			reclaimed = true
			if !rc.Reclaimed || rc.PreviousOwner != "pod-a" {
				t.Errorf("row 2 receipt: reclaimed=%v previous=%q, want true/pod-a", rc.Reclaimed, rc.PreviousOwner)
			}
			if rc.Attempt != 3 || rc.LeaseGeneration != 2 {
				t.Errorf("row 2 receipt attempt=%d generation=%d, want 3/2", rc.Attempt, rc.LeaseGeneration)
			}
		}
	}
	if !reclaimed {
		t.Errorf("the migrated processing row was not claimed: %+v", res.Claimed)
	}
	if got := len(res.Claimed); got != 2 {
		t.Errorf("claim returned %d receipts, want the ready row and the reclaimed row", got)
	}

	// The indexes the kind requires are on the table, and the swap left the
	// name in place.
	names := map[string]bool{}
	rows, err := database.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'event_queue'`)
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
	for _, n := range append(workitem.IndexNames(kind), "idx_event_queue_entity", "idx_event_queue_event", "idx_event_queue_done_at") {
		if !names[n] {
			t.Errorf("index %s missing after migration (present: %v)", n, names)
		}
	}
	var leftover int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'event_queue_new'`).Scan(&leftover); err != nil {
		t.Fatalf("scan leftover: %v", err)
	}
	if leftover != 0 {
		t.Error("event_queue_new survived the rename")
	}

	// A second Migrate is a no-op.
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if r := read(1); r.status != workitem.StatusLeased {
		t.Errorf("row 1 after a second Migrate = %q, want the claim's leased row untouched", r.status)
	}
}

// TestEventQueueSchemaCarriesWorkItemIndexes pins that both dialects' schema
// files carry every statement workitem.IndexDDL renders for the kind,
// verbatim, so the index shape the claim relies on cannot drift from the
// package that owns it.
func TestEventQueueSchemaCarriesWorkItemIndexes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fsys    fs.FS
		file    string
		dialect workitem.Dialect
	}{
		{"postgres baseline", migrationsPostgresFS, "migrations-postgres/202605130001_pg_baseline.sql", workitem.Postgres},
		{"sqlite migration", migrationsSQLiteFS, eventQueueWorkItemFile, workitem.SQLite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := fs.ReadFile(tc.fsys, tc.file)
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			kind := workkinds.EventQueue(tc.dialect)
			for i, stmt := range workitem.IndexDDL(kind) {
				if !strings.Contains(string(raw), stmt+";") {
					t.Errorf("%s lacks index %s verbatim:\n%s", tc.file, workitem.IndexNames(kind)[i], stmt)
				}
			}
			for _, old := range []string{"'pending'", "'processing'", "idx_event_queue_pending", "idx_event_queue_status_processed"} {
				if tc.dialect == workitem.Postgres && strings.Contains(string(raw), "event_queue") && strings.Contains(eventQueueBlock(string(raw)), old) {
					t.Errorf("the baseline's event_queue block still carries %s", old)
				}
			}
		})
	}
}

// eventQueueBlock cuts the baseline down to the event_queue table's own
// statements, from its CREATE TABLE to the next table's.
func eventQueueBlock(baseline string) string {
	start := strings.Index(baseline, "CREATE TABLE public.event_queue")
	if start < 0 {
		return ""
	}
	rest := baseline[start:]
	if end := strings.Index(rest[1:], "CREATE TABLE public."); end >= 0 {
		return rest[:end+1]
	}
	return rest
}
