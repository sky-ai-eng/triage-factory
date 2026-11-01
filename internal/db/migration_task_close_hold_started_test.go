package db

import (
	"database/sql"
	"strings"
	"testing"
)

// The migration that records when a held close began, and the version just
// before it.
const (
	taskCloseHoldStarted      = 202611010004
	taskCloseHoldStartedPrior = 202611010003
)

// TestMigrate_TaskCloseHoldStarted pins what the migration leaves: a task
// holding a close when it runs has its time start then, a task holding none
// has no time, and a write that sets one column of the pair without the
// other is refused.
func TestMigrate_TaskCloseHoldStarted(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemoryNoForeignKeys, taskCloseHoldStartedPrior)
	if _, err := database.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, close_deferred_event_id)
		VALUES ('held', 'e1', 'github:pr:ci_check_failed', '', 'ev0', 'in_progress', 'ev1'),
		       ('plain', 'e2', 'github:pr:ci_check_failed', '', 'ev2', 'queued', NULL)
	`); err != nil {
		t.Fatalf("seed tasks: %v", err)
	}

	migrateThrough(t, database, taskCloseHoldStarted)

	startedAt := func(id string) sql.NullString {
		t.Helper()
		var at sql.NullString
		if err := database.QueryRow(`SELECT close_deferred_at FROM tasks WHERE id = ?`, id).Scan(&at); err != nil {
			t.Fatalf("read task %s: %v", id, err)
		}
		return at
	}
	if at := startedAt("held"); !at.Valid {
		t.Error("the held task has no time its hold began")
	}
	if at := startedAt("plain"); at.Valid {
		t.Errorf("the task holding nothing has a hold time %q", at.String)
	}

	for _, half := range []string{
		`UPDATE tasks SET close_deferred_event_id = 'ev3' WHERE id = 'plain'`,
		`UPDATE tasks SET close_deferred_event_id = NULL WHERE id = 'held'`,
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, close_deferred_event_id)
		 VALUES ('half', 'e3', 'github:pr:ci_check_failed', '', 'ev4', 'queued', 'ev5')`,
	} {
		_, err := database.Exec(half)
		if err == nil || !strings.Contains(err.Error(), "tasks_close_hold_paired") {
			t.Errorf("%s: err = %v, want the pairing refusal", strings.Join(strings.Fields(half), " "), err)
		}
	}
}
