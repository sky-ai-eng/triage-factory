package db

import (
	"database/sql"

	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

// ListEventTypes returns all event types from the catalog.
func ListEventTypes(db *sql.DB) ([]domain.EventType, error) {
	rows, err := db.Query(`SELECT id, source, category, label, description, default_priority FROM event_types ORDER BY source, category, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var types []domain.EventType
	for rows.Next() {
		var et domain.EventType
		if err := rows.Scan(&et.ID, &et.Source, &et.Category, &et.Label, &et.Description, &et.DefaultPriority); err != nil {
			return nil, err
		}
		types = append(types, et)
	}
	return types, rows.Err()
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
