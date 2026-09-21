package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
)

// The migration that adds tasks.score_revision and the task_rederive_queue
// table, and the version just before it, at which the test seeds the
// previous shape.
const (
	taskReDeriveQueueFile  = "migrations-sqlite/202609220002_task_rederive_queue.sql"
	taskReDeriveQueuePrior = 202609220001
)

// TestMigrate_TaskReDeriveQueue pins the upgrade: every task gains a
// score_revision of zero, a task owed a pass before the upgrade holds one
// ready queue row after it — at revision 0, keyed on the task — a task that
// owed nothing holds none, the owed column and its index are gone, the
// kind's indexes are present, and a second Migrate is a no-op.
func TestMigrate_TaskReDeriveQueue(t *testing.T) {
	database := openMigrationsTestDB(t)
	goose.SetBaseFS(migrationsSQLiteFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpTo(database, "migrations-sqlite", taskReDeriveQueuePrior); err != nil {
		t.Fatalf("goose UpTo %d: %v", taskReDeriveQueuePrior, err)
	}
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}

	for _, stmt := range []string{
		`INSERT INTO entities (id, source, source_id, kind) VALUES ('e1', 'github', 'o/r#1', 'pr')`,
		`INSERT INTO events (id, event_type, entity_id) VALUES ('ev1', 'github:pr:ci_check_failed', 'e1')`,
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, rederive_owed)
			VALUES ('t-owed', 'e1', 'github:pr:ci_check_failed', 'a', 'ev1', 'queued', 'scored', 1)`,
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, rederive_owed)
			VALUES ('t-clear', 'e1', 'github:pr:ci_check_failed', 'b', 'ev1', 'queued', 'scored', 0)`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for _, id := range []string{"t-owed", "t-clear"} {
		var rev int64
		if err := database.QueryRow(`SELECT score_revision FROM tasks WHERE id = ?`, id).Scan(&rev); err != nil {
			t.Fatalf("read score_revision of %s: %v", id, err)
		}
		if rev != 0 {
			t.Errorf("task %s score_revision = %d, want 0", id, rev)
		}
	}

	type row struct {
		status, uniqueKey            string
		attempt, maxAttempts         int
		generation, rev              int64
		nextAt, owner, expires, done sql.NullString
		firstEnqueuedAt, createdAt   string
	}
	rows, err := database.Query(`
		SELECT task_id, status, COALESCE(unique_key, ''), attempt, max_attempts, lease_generation, requested_revision,
		       next_attempt_at, lease_owner, lease_expires_at, done_at, first_enqueued_at, created_at
		FROM task_rederive_queue ORDER BY id`)
	if err != nil {
		t.Fatalf("read task_rederive_queue: %v", err)
	}
	defer rows.Close()
	got := map[string]row{}
	for rows.Next() {
		var taskID string
		var r row
		if err := rows.Scan(&taskID, &r.status, &r.uniqueKey, &r.attempt, &r.maxAttempts, &r.generation, &r.rev,
			&r.nextAt, &r.owner, &r.expires, &r.done, &r.firstEnqueuedAt, &r.createdAt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[taskID] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read task_rederive_queue: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("task_rederive_queue holds %d rows, want the one owed task's: %+v", len(got), got)
	}
	owed, ok := got["t-owed"]
	if !ok {
		t.Fatalf("no queue row for the owed task; rows = %+v", got)
	}
	if owed.status != workitem.StatusReady || owed.uniqueKey != "t-owed" || owed.attempt != 0 || owed.maxAttempts != 5 || owed.generation != 0 || owed.rev != 0 {
		t.Errorf("owed row = %+v, want ready, keyed on the task, attempt 0 of 5, generation 0, revision 0", owed)
	}
	if owed.nextAt.Valid || owed.owner.Valid || owed.expires.Valid || owed.done.Valid {
		t.Errorf("owed row carries lease or terminal columns: %+v", owed)
	}
	if owed.firstEnqueuedAt == "" || owed.firstEnqueuedAt != owed.createdAt {
		t.Errorf("owed row first_enqueued_at %q / created_at %q, want equal and set", owed.firstEnqueuedAt, owed.createdAt)
	}

	// The owed column is gone, and so is its index.
	cols, err := database.Query(`PRAGMA table_info(tasks)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	for cols.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == "rederive_owed" {
			t.Error("tasks still carries rederive_owed after the migration")
		}
	}
	cols.Close()
	var oldIdx int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_tasks_rederive_owed'`).Scan(&oldIdx); err != nil {
		t.Fatalf("look up the owed index: %v", err)
	}
	if oldIdx != 0 {
		t.Error("idx_tasks_rederive_owed survived the migration")
	}

	// The kind's indexes are on the new table.
	kind := workkinds.TaskReDerive(workitem.SQLite)
	for _, name := range workitem.IndexNames(kind) {
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'task_rederive_queue' AND name = ?`, name).Scan(&n); err != nil {
			t.Fatalf("look up index %s: %v", name, err)
		}
		if n != 1 {
			t.Errorf("index %s missing after the migration", name)
		}
	}

	// The backfilled row is claimable through the package, and freezes the
	// revision it carries.
	res, err := workitem.Claim(context.Background(), database, kind, workitem.Owner{ID: "pod", Epoch: 1}, "", 10)
	if err != nil {
		t.Fatalf("Claim after migration: %v", err)
	}
	if len(res.Claimed) != 1 || res.Claimed[0].UniqueKey != "t-owed" {
		t.Fatalf("claim = %+v, want the owed task's row", res.Claimed)
	}
	if rev, ok := res.Claimed[0].Frozen["requested_revision"].(int64); !ok || rev != 0 {
		t.Errorf("frozen requested_revision = %T %v, want int64 0", res.Claimed[0].Frozen["requested_revision"], res.Claimed[0].Frozen["requested_revision"])
	}

	// A second Migrate is a no-op.
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM task_rederive_queue WHERE task_id = 't-owed'`).Scan(&status); err != nil || status != workitem.StatusLeased {
		t.Errorf("row after a second Migrate = %q (%v), want the claim's leased row untouched", status, err)
	}
}
