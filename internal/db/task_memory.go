package db

import (
	"database/sql"
	"time"

	"github.com/sky-ai-eng/todo-triage/internal/domain"
)

// SaveTaskMemory inserts a task_memory row. Callers populate the struct;
// this helper does not generate IDs or timestamps on their behalf — the
// spawner knows which run_id to use (either the just-completed agent run
// or the system-stub placeholder for an involuntary failure).
func SaveTaskMemory(database *sql.DB, m domain.TaskMemory) error {
	_, err := database.Exec(`
		INSERT INTO task_memory (id, task_id, run_id, content, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, m.ID, m.TaskID, m.RunID, m.Content, m.Source, m.CreatedAt)
	return err
}

// GetTaskMemoriesForTask returns all memories for a task, oldest first.
// The ascending order matters: when the spawner materializes these into
// `./task_memory/` on a fresh worktree, we want the chronology to be
// obvious from filesystem listing order (even though each file is a
// self-contained narrative, humans inspecting the directory benefit).
func GetTaskMemoriesForTask(database *sql.DB, taskID string) ([]domain.TaskMemory, error) {
	rows, err := database.Query(`
		SELECT id, task_id, run_id, content, source, created_at
		FROM task_memory WHERE task_id = ? ORDER BY created_at ASC
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.TaskMemory
	for rows.Next() {
		var m domain.TaskMemory
		var createdAt time.Time
		if err := rows.Scan(&m.ID, &m.TaskID, &m.RunID, &m.Content, &m.Source, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = createdAt
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetTaskMemoryByRun returns the single memory row associated with an
// agent run, or nil if none exists. Used by the UI and diagnostics to
// surface a specific run's reasoning. Returns nil without error for the
// not-found case so callers can distinguish "missing" from "error."
func GetTaskMemoryByRun(database *sql.DB, runID string) (*domain.TaskMemory, error) {
	row := database.QueryRow(`
		SELECT id, task_id, run_id, content, source, created_at
		FROM task_memory WHERE run_id = ?
	`, runID)

	var m domain.TaskMemory
	var createdAt time.Time
	err := row.Scan(&m.ID, &m.TaskID, &m.RunID, &m.Content, &m.Source, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt = createdAt
	return &m, nil
}
