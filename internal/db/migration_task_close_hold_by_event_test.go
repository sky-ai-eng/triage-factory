package db

import (
	"database/sql"
	"strings"
	"testing"
)

// The migration that drops a held close's blueprint run, and the version
// just before it.
const (
	taskCloseHoldByEvent      = 202611010003
	taskCloseHoldByEventPrior = 202611010002
)

// TestMigrate_TaskCloseHoldByEvent pins what the migration leaves: the
// blueprint run column and its pairing triggers are gone, and a task that
// held a close still holds it, under its event.
func TestMigrate_TaskCloseHoldByEvent(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemoryNoForeignKeys, taskCloseHoldByEventPrior)
	if _, err := database.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status,
		                   close_deferred_event_id, close_deferred_blueprint_run_id)
		VALUES ('held', 'e1', 'github:pr:ci_check_failed', '', 'ev0', 'in_progress', 'ev1', 'br1')
	`); err != nil {
		t.Fatalf("seed held task: %v", err)
	}

	migrateThrough(t, database, taskCloseHoldByEvent)

	var held sql.NullString
	if err := database.QueryRow(`SELECT close_deferred_event_id FROM tasks WHERE id = 'held'`).Scan(&held); err != nil {
		t.Fatalf("read held task: %v", err)
	}
	if held.String != "ev1" {
		t.Errorf("held close = %q after the migration, want ev1", held.String)
	}
	var columns string
	if err := database.QueryRow(`SELECT group_concat(name) FROM pragma_table_info('tasks')`).Scan(&columns); err != nil {
		t.Fatalf("read tasks columns: %v", err)
	}
	if strings.Contains(columns, "close_deferred_blueprint_run_id") {
		t.Errorf("tasks still has close_deferred_blueprint_run_id: %s", columns)
	}
	var triggers int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'tasks_close_deferral_paired%'
	`).Scan(&triggers); err != nil {
		t.Fatalf("count pairing triggers: %v", err)
	}
	if triggers != 0 {
		t.Errorf("%d pairing triggers survived the migration, want 0", triggers)
	}
}
