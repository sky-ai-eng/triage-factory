package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that gives SQLite the one-running-blueprint_run-per-task index
// (202609120004). Local mode never had the backstop, so an install can already
// hold what the index forbids — a task with a manual run and an auto one both
// 'running' — and creating a unique index over that fails the migration and
// bricks boot. What there is to pin is the cleanup it does first: the task keeps
// its most recently started run, every other running run on it is cancelled, the
// conversations those runs carried get a boundary so nothing reads them as the
// task's live one forever, and rows the migration has no business touching are
// left exactly as they were.
func TestMigrate_OneActiveRunPerTask_SettlesWhatThePreIndexEraAllowed(t *testing.T) {
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
	// One version short, so the rows below are staged the way an install with
	// no index wrote them.
	upToErr := goose.UpTo(database, dir, 202609120003)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	// events_catalog is seeded by Migrate, not by goose, and the chain below
	// FKs into it through events / tasks.
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}

	const userID = "00000000-0000-0000-0000-000000000100"
	for _, stmt := range []string{
		`INSERT INTO users (id) VALUES ('` + userID + `')`,
		`INSERT INTO entities (id, source, source_id, kind, title) VALUES ('e1', 'github', 'owner/repo#1', 'pr', 'a pr')`,
		`INSERT INTO events (id, entity_id, event_type) VALUES ('ev1', 'e1', (SELECT id FROM events_catalog LIMIT 1))`,
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
			VALUES ('t-crowded', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'ev1', 'queued')`,
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status)
			VALUES ('t-quiet', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'other', 'ev1', 'queued')`,
		`INSERT INTO blueprints (id, name, creator_user_id) VALUES ('bp1', 'BP', '` + userID + `')`,
		`INSERT INTO prompts (id, name, body, creator_user_id) VALUES ('p1', 'P', 'b', '` + userID + `')`,

		// The crowded task: three running runs, which the index will allow one
		// of. started_at decides which, so they are stamped explicitly rather
		// than left to CURRENT_TIMESTAMP's one-second resolution.
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id, status, started_at, step_plan, worktree_path)
			VALUES ('br-oldest', 'bp1', 't-crowded', 'manual', '` + userID + `', 'running', '2026-01-01 00:00:00', '[]', '')`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id, status, started_at, step_plan, worktree_path)
			VALUES ('br-middle', 'bp1', 't-crowded', 'event', NULL, 'running', '2026-01-02 00:00:00', '[]', '')`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id, status, started_at, step_plan, worktree_path)
			VALUES ('br-newest', 'bp1', 't-crowded', 'manual', '` + userID + `', 'running', '2026-01-03 00:00:00', '[]', '')`,
		// A run that already concluded on the same task: outside the partial
		// index, and nothing the cleanup should rewrite.
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id, status, abort_reason, started_at, step_plan, worktree_path)
			VALUES ('br-done', 'bp1', 't-crowded', 'manual', '` + userID + `', 'cancelled', 'user_cancelled', '2025-12-01 00:00:00', '[]', '')`,
		// A different task's single running run: untouched, because the rule
		// is per task.
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id, status, started_at, step_plan, worktree_path)
			VALUES ('br-quiet', 'bp1', 't-quiet', 'manual', '` + userID + `', 'running', '2026-01-01 00:00:00', '[]', '')`,

		// Conversations: one live under each superseded run, one live under the
		// survivor, one already terminal under a superseded run, and one
		// already ended (with a reason that must not be rewritten).
		`INSERT INTO conversations (id, task_id, prompt_id, blueprint_run_id, status)
			VALUES ('c-oldest', 't-crowded', 'p1', 'br-oldest', 'open')`,
		`INSERT INTO conversations (id, task_id, prompt_id, blueprint_run_id, status)
			VALUES ('c-middle', 't-crowded', 'p1', 'br-middle', NULL)`,
		`INSERT INTO conversations (id, task_id, prompt_id, blueprint_run_id, status)
			VALUES ('c-newest', 't-crowded', 'p1', 'br-newest', 'running')`,
		`INSERT INTO conversations (id, task_id, prompt_id, blueprint_run_id, status)
			VALUES ('c-oldest-done', 't-crowded', 'p1', 'br-oldest', 'completed')`,
		`INSERT INTO conversations (id, task_id, prompt_id, blueprint_run_id, status, ended_at, ended_reason)
			VALUES ('c-oldest-ended', 't-crowded', 'p1', 'br-oldest', 'open', '2025-12-31 00:00:00', 'taken_over')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, 202609120004)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo one-active-run-per-task: %v", upErr)
	}

	for _, want := range []struct{ id, status, abortReason string }{
		{"br-newest", "running", ""},
		{"br-middle", "cancelled", "system_cancelled"},
		{"br-oldest", "cancelled", "system_cancelled"},
		// Untouched: already terminal, and on another task.
		{"br-done", "cancelled", "user_cancelled"},
		{"br-quiet", "running", ""},
	} {
		var status, abortReason string
		if err := database.QueryRow(
			`SELECT status, COALESCE(abort_reason, '') FROM blueprint_runs WHERE id = ?`, want.id,
		).Scan(&status, &abortReason); err != nil {
			t.Fatalf("read %s: %v", want.id, err)
		}
		if status != want.status || abortReason != want.abortReason {
			t.Errorf("%s = (%q, %q), want (%q, %q)", want.id, status, abortReason, want.status, want.abortReason)
		}
	}

	for _, want := range []struct{ id, reason string }{
		// Live under a superseded run: ended, because a cancelled blueprint's
		// step is not drivable and an un-ended one would hold its task's gate
		// over something nothing can wake.
		{"c-oldest", "delegated"},
		{"c-middle", "delegated"},
		// Under the surviving run: still the task's live conversation.
		{"c-newest", ""},
		// Terminal already — it is nobody's live conversation, so there is no
		// boundary to invent for it.
		{"c-oldest-done", ""},
		// Already ended: the first boundary is the one that happened.
		{"c-oldest-ended", "taken_over"},
	} {
		var reason string
		if err := database.QueryRow(
			`SELECT COALESCE(ended_reason, '') FROM conversations WHERE id = ?`, want.id,
		).Scan(&reason); err != nil {
			t.Fatalf("read %s: %v", want.id, err)
		}
		if reason != want.reason {
			t.Errorf("%s ended_reason = %q, want %q", want.id, reason, want.reason)
		}
	}

	// And the index the cleanup made room for actually exists and bites.
	if _, err := database.Exec(`
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id, status, step_plan, worktree_path)
		VALUES ('br-second', 'bp1', 't-crowded', 'event', NULL, 'running', '[]', '')
	`); err == nil {
		t.Error("a second running blueprint_run on the task was accepted; the index is missing")
	}
}
