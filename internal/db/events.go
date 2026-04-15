package db

import (
	"database/sql"
	"log"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SeedEventTypes inserts the canonical event type catalog into events_catalog.
// The events_catalog table is read-only system registry — only id/source/
// category/label/description are persisted (no enabled, default_priority,
// sort_order — those concerns moved to task_rules).
//
// Uses ON CONFLICT DO UPDATE (true upsert) rather than INSERT OR REPLACE so
// that updated labels/descriptions propagate without a DELETE+INSERT cycle.
// REPLACE would trip ON DELETE RESTRICT against task_rules / prompt_triggers /
// tasks rows that reference the event_type on every reseed after the first.
func SeedEventTypes(db *sql.DB) error {
	stmt, err := db.Prepare(`
		INSERT INTO events_catalog (id, source, category, label, description)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			source = excluded.source,
			category = excluded.category,
			label = excluded.label,
			description = excluded.description
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, et := range domain.AllEventTypes() {
		if _, err := stmt.Exec(et.ID, et.Source, et.Category, et.Label, et.Description); err != nil {
			return err
		}
	}
	log.Printf("[db] seeded %d event types into events_catalog", len(domain.AllEventTypes()))
	return nil
}

// RecordEvent inserts an event into the audit log and returns its ID.
func RecordEvent(db *sql.DB, evt domain.Event) (int64, error) {
	result, err := db.Exec(`
		INSERT INTO events (event_type, task_id, source_id, metadata)
		VALUES (?, ?, ?, ?)
	`, evt.EventType, evt.TaskID, evt.SourceID, evt.Metadata)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// SetTaskEventType updates the event_type column on a task.
func SetTaskEventType(db *sql.DB, taskID, eventType string) error {
	_, err := db.Exec(`UPDATE tasks SET event_type = ? WHERE id = ?`, eventType, taskID)
	return err
}

// GetPollerState returns the last-known state JSON for a source item, or "" if none.
func GetPollerState(db *sql.DB, source, sourceID string) (string, error) {
	var stateJSON string
	err := db.QueryRow(`SELECT state_json FROM poller_state WHERE source = ? AND source_id = ?`, source, sourceID).Scan(&stateJSON)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return stateJSON, err
}

// SetPollerState upserts the state snapshot for a source item.
func SetPollerState(db *sql.DB, source, sourceID, stateJSON string) error {
	_, err := db.Exec(`
		INSERT INTO poller_state (source, source_id, state_json, updated_at)
		VALUES (?, ?, ?, datetime('now'))
		ON CONFLICT(source, source_id) DO UPDATE SET
			state_json = excluded.state_json,
			updated_at = excluded.updated_at
	`, source, sourceID, stateJSON)
	return err
}

// RecentEvents returns the most recent N events, newest first.
func RecentEvents(db *sql.DB, limit int) ([]domain.Event, error) {
	rows, err := db.Query(`
		SELECT id, event_type, task_id, source_id, COALESCE(metadata, ''), created_at
		FROM events ORDER BY created_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []domain.Event
	for rows.Next() {
		var e domain.Event
		if err := rows.Scan(&e.ID, &e.EventType, &e.TaskID, &e.SourceID, &e.Metadata, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
