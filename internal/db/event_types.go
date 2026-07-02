package db

import (
	"database/sql"
	"fmt"

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

// SeedEventTypes reconciles events_catalog with domain.AllEventTypes(),
// the Go source of truth: every id it declares is upserted — inserted if
// missing, overwritten (source/category/label/description) if already
// present. A row whose id isn't in domain.AllEventTypes() is left alone;
// events_catalog is an FK target for historical events/tasks, so retiring
// an event type is out of scope here.
//
// The DO UPDATE is WHERE-guarded to a no-op when the incoming values match
// what's already stored: this runs on every Migrate() call (every server
// boot, every test bootstrap), and without the guard an unconditional
// UPDATE would write a new row version (Postgres: WAL + a dead tuple for
// autovacuum) for all ~30 rows every single time even though almost none
// of them ever change. IS DISTINCT FROM is null-safe, though these columns
// are all NOT NULL today.
//
// Safe to call repeatedly, including concurrently across replicas: each
// row's upsert is atomic under the dialect's own MVCC, so there's no
// cross-row transaction to race.
func SeedEventTypes(db *sql.DB, dialect string) error {
	var query string
	switch dialect {
	case "postgres":
		query = `INSERT INTO events_catalog (id, source, category, label, description)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (id) DO UPDATE SET
				source = EXCLUDED.source,
				category = EXCLUDED.category,
				label = EXCLUDED.label,
				description = EXCLUDED.description
			WHERE events_catalog.source IS DISTINCT FROM EXCLUDED.source
			   OR events_catalog.category IS DISTINCT FROM EXCLUDED.category
			   OR events_catalog.label IS DISTINCT FROM EXCLUDED.label
			   OR events_catalog.description IS DISTINCT FROM EXCLUDED.description`
	case "sqlite3":
		query = `INSERT INTO events_catalog (id, source, category, label, description)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				source = excluded.source,
				category = excluded.category,
				label = excluded.label,
				description = excluded.description
			WHERE events_catalog.source IS DISTINCT FROM excluded.source
			   OR events_catalog.category IS DISTINCT FROM excluded.category
			   OR events_catalog.label IS DISTINCT FROM excluded.label
			   OR events_catalog.description IS DISTINCT FROM excluded.description`
	default:
		return fmt.Errorf("seed event types: unsupported dialect %q", dialect)
	}

	for _, et := range domain.AllEventTypes() {
		if _, err := db.Exec(query, et.ID, et.Source, et.Category, et.Label, et.Description); err != nil {
			return fmt.Errorf("seed event type %q: %w", et.ID, err)
		}
	}
	return nil
}
