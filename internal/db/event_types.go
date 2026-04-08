package db

import (
	"database/sql"

	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

// ListEventTypes returns all event types from the catalog, ordered by user sort order.
func ListEventTypes(db *sql.DB) ([]domain.EventType, error) {
	rows, err := db.Query(`SELECT id, source, category, label, description, default_priority, enabled, sort_order FROM event_types ORDER BY sort_order ASC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var types []domain.EventType
	for rows.Next() {
		var et domain.EventType
		if err := rows.Scan(&et.ID, &et.Source, &et.Category, &et.Label, &et.Description, &et.DefaultPriority, &et.Enabled, &et.SortOrder); err != nil {
			return nil, err
		}
		types = append(types, et)
	}
	return types, rows.Err()
}

// UpdateEventTypeEnabled toggles the enabled flag for an event type.
func UpdateEventTypeEnabled(db *sql.DB, id string, enabled bool) error {
	_, err := db.Exec(`UPDATE event_types SET enabled = ? WHERE id = ?`, enabled, id)
	return err
}

// ReorderEventTypes bulk-updates sort_order for a list of event type IDs.
// The order of the slice determines the sort_order values (0, 1, 2, ...).
func ReorderEventTypes(db *sql.DB, ids []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE event_types SET sort_order = ? WHERE id = ?`, i, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListAllBindings returns every prompt binding in the system.
func ListAllBindings(db *sql.DB) ([]domain.PromptBinding, error) {
	rows, err := db.Query(`SELECT prompt_id, event_type, is_default FROM prompt_bindings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bindings []domain.PromptBinding
	for rows.Next() {
		var b domain.PromptBinding
		if err := rows.Scan(&b.PromptID, &b.EventType, &b.IsDefault); err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}
