package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that re-keys workspace_snapshots from the blueprint run to the
// task (202609120005). The new primary key admits one row per task where the
// old one admitted one per run, so an install that delegated twice on a task
// already holds what the new key forbids — and a straight column rename over
// those rows fails. What there is to pin is the choice the data step makes:
// the surviving row is the one belonging to the task's most recently started
// run (the only workspace anybody would want continued), and a task with a
// single run keeps its row untouched. The new key and its cascade are pinned
// beside it, since both only exist after the rebuild.
func TestMigrate_WorkspaceSnapshotsByTask_KeepsTheNewestRunsRowPerTask(t *testing.T) {
	database := openMigrationsTestDB(t)

	gooseMu.Lock()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		gooseMu.Unlock()
		t.Fatalf("migrationsFor: %v", err)
	}
	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		gooseMu.Unlock()
		t.Fatalf("SetDialect: %v", err)
	}
	// One version short, so the rows below are staged the way an install
	// keyed by the blueprint run wrote them.
	upToErr := goose.UpTo(database, dir, 202609120004)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	// events_catalog is seeded by Migrate, not by goose, and the chain below
	// FKs into it through events / tasks.
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}

	const (
		userID  = "00000000-0000-0000-0000-000000000100"
		writerA = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
		writerB = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
		writerC = "cccccccc-3333-4333-8333-cccccccccccc"
	)
	for _, stmt := range []string{
		`INSERT INTO users (id) VALUES ('` + userID + `')`,
		`INSERT INTO entities (id, source, source_id, kind, title) VALUES ('e1', 'github', 'owner/repo#1', 'pr', 'a pr')`,
		`INSERT INTO events (id, entity_id, event_type) VALUES ('ev1', 'e1', (SELECT id FROM events_catalog LIMIT 1))`,
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
			VALUES ('t-redelegated', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'ev1', 'queued')`,
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status)
			VALUES ('t-once', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'other', 'ev1', 'queued')`,
		`INSERT INTO blueprints (id, name, creator_user_id) VALUES ('bp1', 'BP', '` + userID + `')`,

		// The re-delegated task: two runs, each with its own snapshot row. The
		// index added one version earlier admits a single *running* run, so the
		// older one is settled — which is also the real shape, a second
		// delegation landing after the first concluded.
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id, status, started_at, step_plan, worktree_path)
			VALUES ('br-older', 'bp1', 't-redelegated', 'manual', '` + userID + `', 'completed', '2026-01-01 00:00:00', '[]', '')`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id, status, started_at, step_plan, worktree_path)
			VALUES ('br-newer', 'bp1', 't-redelegated', 'event', NULL, 'running', '2026-01-03 00:00:00', '[]', '')`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id, status, started_at, step_plan, worktree_path)
			VALUES ('br-only', 'bp1', 't-once', 'manual', '` + userID + `', 'running', '2026-01-02 00:00:00', '[]', '')`,

		`INSERT INTO workspace_snapshots (blueprint_run_id, state, writer_claim_id)
			VALUES ('br-older', 'failed', '` + writerA + `')`,
		`INSERT INTO workspace_snapshots (blueprint_run_id, state, writer_claim_id)
			VALUES ('br-newer', 'written', '` + writerB + `')`,
		`INSERT INTO workspace_snapshots (blueprint_run_id, state, writer_claim_id)
			VALUES ('br-only', 'pending', '` + writerC + `')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, 202609120005)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo workspace_snapshots_by_task: %v", upErr)
	}

	rows, err := database.Query(`SELECT task_id, state, writer_claim_id FROM workspace_snapshots ORDER BY task_id`)
	if err != nil {
		t.Fatalf("read re-keyed rows: %v", err)
	}
	defer rows.Close()
	type row struct{ taskID, state, writer string }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.taskID, &r.state, &r.writer); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []row{
		// The newer run's row survives; the older run's is dropped, blob and
		// all (the boot step deletes its object).
		{taskID: "t-redelegated", state: "written", writer: writerB},
		{taskID: "t-once", state: "pending", writer: writerC},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want %+v — the task's newest run's row is the one workspace anyone continues from", got, want)
	}
	byTask := map[string]row{}
	for _, r := range got {
		byTask[r.taskID] = r
	}
	for _, w := range want {
		g, ok := byTask[w.taskID]
		if !ok {
			t.Errorf("task %s lost its snapshot row entirely", w.taskID)
			continue
		}
		if g != w {
			t.Errorf("task %s row = %+v, want %+v", w.taskID, g, w)
		}
	}

	// And the new key bites: a second row on one task is refused, where the
	// old key would have taken one per run.
	if _, err := database.Exec(`
		INSERT INTO workspace_snapshots (task_id, state, writer_claim_id)
		VALUES ('t-redelegated', 'pending', '` + writerA + `')
	`); err == nil {
		t.Error("a second workspace_snapshots row on one task was accepted; the key is still per run")
	}

	// The cascade follows the task now, which is what keeps a purged task from
	// leaving a row pointing at a blob nobody will ask for again. Its runs go
	// first because they reference it too — a purge takes the whole chain.
	if _, err := database.Exec(`DELETE FROM blueprint_runs WHERE task_id = 't-once'`); err != nil {
		t.Fatalf("delete the task's blueprint runs: %v", err)
	}
	if _, err := database.Exec(`DELETE FROM tasks WHERE id = 't-once'`); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	var remaining int
	if err := database.QueryRow(`SELECT COUNT(*) FROM workspace_snapshots WHERE task_id = 't-once'`).Scan(&remaining); err != nil {
		t.Fatalf("count after the task delete: %v", err)
	}
	if remaining != 0 {
		t.Errorf("snapshot rows for the purged task = %d, want 0", remaining)
	}
}
