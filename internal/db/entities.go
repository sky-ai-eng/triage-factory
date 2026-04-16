package db

import (
	"database/sql"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// FindOrCreateEntity returns the entity for (source, source_id), creating it
// if it doesn't exist. Returns (entity, created, error).
func FindOrCreateEntity(db *sql.DB, source, sourceID, kind, title, url string) (*domain.Entity, bool, error) {
	// Try to find existing first (common path on subsequent polls).
	existing, err := GetEntityBySource(db, source, sourceID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}

	// Create new entity.
	id := uuid.New().String()
	now := time.Now()
	_, err = db.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, state, created_at, last_polled_at)
		VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)
	`, id, source, sourceID, kind, title, url, now, now)
	if err != nil {
		// Race condition: another goroutine may have created it between our
		// SELECT and INSERT. Re-read.
		existing, err2 := GetEntityBySource(db, source, sourceID)
		if err2 == nil && existing != nil {
			return existing, false, nil
		}
		return nil, false, err
	}

	entity := &domain.Entity{
		ID:           id,
		Source:       source,
		SourceID:     sourceID,
		Kind:         kind,
		Title:        title,
		URL:          url,
		State:        "active",
		CreatedAt:    now,
		LastPolledAt: &now,
	}
	return entity, true, nil
}

// MarkEntityClosed sets state='closed' and closed_at=now without the task
// cascade. Used at discovery time when the initial snapshot is already terminal
// (merged/closed PR, completed Jira issue) — the entity was never active, so
// there are no tasks to cascade-close.
func MarkEntityClosed(db *sql.DB, entityID string) error {
	_, err := db.Exec(`
		UPDATE entities SET state = 'closed', closed_at = ? WHERE id = ?
	`, time.Now(), entityID)
	return err
}

// ReactivateEntity transitions a closed entity back to active. Used when a
// previously-terminal entity reappears as open (e.g., reopened PR, reopened
// Jira issue). Returns true if the entity was actually reactivated.
func ReactivateEntity(db *sql.DB, entityID string) (bool, error) {
	result, err := db.Exec(`
		UPDATE entities SET state = 'active', closed_at = NULL WHERE id = ? AND state = 'closed'
	`, entityID)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// UpdateEntitySnapshot updates the snapshot_json and last_polled_at for an entity.
func UpdateEntitySnapshot(db *sql.DB, entityID, snapshotJSON string) error {
	_, err := db.Exec(`
		UPDATE entities SET snapshot_json = ?, last_polled_at = ? WHERE id = ?
	`, snapshotJSON, time.Now(), entityID)
	return err
}

// UpdateEntityTitle updates the title for an entity (e.g., PR title changed).
func UpdateEntityTitle(db *sql.DB, entityID, title string) error {
	_, err := db.Exec(`UPDATE entities SET title = ? WHERE id = ?`, title, entityID)
	return err
}

// CloseEntity sets state='closed' and closed_at=now. Called by the entity
// lifecycle handler when an entity-terminating event fires.
func CloseEntity(db *sql.DB, entityID string) error {
	_, err := db.Exec(`
		UPDATE entities SET state = 'closed', closed_at = ? WHERE id = ? AND state = 'active'
	`, time.Now(), entityID)
	return err
}

// GetEntity returns an entity by ID, or nil if not found.
func GetEntity(db *sql.DB, id string) (*domain.Entity, error) {
	row := db.QueryRow(`
		SELECT id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
		       COALESCE(snapshot_json, ''), state, created_at, last_polled_at, closed_at
		FROM entities WHERE id = ?
	`, id)
	return scanEntity(row)
}

// GetEntityBySource returns an entity by (source, source_id), or nil if not found.
func GetEntityBySource(db *sql.DB, source, sourceID string) (*domain.Entity, error) {
	row := db.QueryRow(`
		SELECT id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
		       COALESCE(snapshot_json, ''), state, created_at, last_polled_at, closed_at
		FROM entities WHERE source = ? AND source_id = ?
	`, source, sourceID)
	return scanEntity(row)
}

// ListActiveEntities returns all entities with state='active' for a given source.
func ListActiveEntities(db *sql.DB, source string) ([]domain.Entity, error) {
	rows, err := db.Query(`
		SELECT id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
		       COALESCE(snapshot_json, ''), state, created_at, last_polled_at, closed_at
		FROM entities WHERE source = ? AND state = 'active'
		ORDER BY last_polled_at ASC
	`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entities []domain.Entity
	for rows.Next() {
		var e domain.Entity
		if err := rows.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
			&e.SnapshotJSON, &e.State, &e.CreatedAt, &e.LastPolledAt, &e.ClosedAt); err != nil {
			return nil, err
		}
		entities = append(entities, e)
	}
	return entities, rows.Err()
}

func scanEntity(row *sql.Row) (*domain.Entity, error) {
	var e domain.Entity
	err := row.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
		&e.SnapshotJSON, &e.State, &e.CreatedAt, &e.LastPolledAt, &e.ClosedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}
