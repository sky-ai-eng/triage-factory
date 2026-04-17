package db

import (
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ListEventTypes returns all event types from the events_catalog registry.
// Behavioral / preference fields (enabled, default_priority, sort_order) are
// no longer persisted; the returned struct's deprecated fields stay zero-valued
// so existing UI consumers can still render the list (they'll be removed once
// the UI is rewritten in a later sub-ticket).
func ListEventTypes(db *sql.DB) ([]domain.EventType, error) {
	rows, err := db.Query(`SELECT id, source, category, label, description FROM events_catalog ORDER BY source, category, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var types []domain.EventType
	for rows.Next() {
		var et domain.EventType
		if err := rows.Scan(&et.ID, &et.Source, &et.Category, &et.Label, &et.Description); err != nil {
			return nil, err
		}
		types = append(types, et)
	}
	return types, rows.Err()
}
