package db

import (
	"database/sql"

	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

// UpsertTrackedItem inserts or updates a tracked item.
// On conflict (same source+source_id), updates snapshot and last_polled.
func UpsertTrackedItem(database *sql.DB, item domain.TrackedItem) error {
	_, err := database.Exec(`
		INSERT INTO tracked_items (id, source, source_id, repo, task_id, node_id, snapshot, tracked_since, last_polled)
		VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
		ON CONFLICT(source, source_id) DO UPDATE SET
			repo       = excluded.repo,
			task_id    = excluded.task_id,
			node_id    = COALESCE(excluded.node_id, tracked_items.node_id),
			snapshot   = excluded.snapshot,
			last_polled = datetime('now')
	`, item.ID, item.Source, item.SourceID, nullIfEmpty(item.Repo), nullIfEmpty(item.TaskID), nullIfEmpty(item.NodeID), item.Snapshot)
	return err
}

// GetTrackedItem returns a single tracked item by source and source_id, or nil.
func GetTrackedItem(database *sql.DB, source, sourceID string) (*domain.TrackedItem, error) {
	var item domain.TrackedItem
	var repo, taskID, nodeID sql.NullString
	var lastPolled, terminalAt sql.NullTime
	err := database.QueryRow(`
		SELECT id, source, source_id, repo, task_id, node_id, snapshot, tracked_since, last_polled, terminal_at
		FROM tracked_items WHERE source = ? AND source_id = ?
	`, source, sourceID).Scan(
		&item.ID, &item.Source, &item.SourceID, &repo, &taskID, &nodeID,
		&item.Snapshot, &item.TrackedSince, &lastPolled, &terminalAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item.Repo = repo.String
	item.TaskID = taskID.String
	item.NodeID = nodeID.String
	if lastPolled.Valid {
		item.LastPolled = &lastPolled.Time
	}
	if terminalAt.Valid {
		item.TerminalAt = &terminalAt.Time
	}
	return &item, nil
}

// ListActiveTrackedItems returns all non-terminal tracked items for a source.
func ListActiveTrackedItems(database *sql.DB, source string) ([]domain.TrackedItem, error) {
	rows, err := database.Query(`
		SELECT id, source, source_id, repo, task_id, node_id, snapshot, tracked_since, last_polled
		FROM tracked_items
		WHERE source = ? AND terminal_at IS NULL
		ORDER BY tracked_since
	`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []domain.TrackedItem
	for rows.Next() {
		var item domain.TrackedItem
		var repo, taskID, nodeID sql.NullString
		var lastPolled sql.NullTime
		if err := rows.Scan(&item.ID, &item.Source, &item.SourceID, &repo, &taskID, &nodeID, &item.Snapshot, &item.TrackedSince, &lastPolled); err != nil {
			return nil, err
		}
		item.Repo = repo.String
		item.TaskID = taskID.String
		item.NodeID = nodeID.String
		if lastPolled.Valid {
			item.LastPolled = &lastPolled.Time
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListNodeIDs returns just the GraphQL node IDs for all active tracked items of a source.
func ListNodeIDs(database *sql.DB, source string) ([]string, error) {
	rows, err := database.Query(`
		SELECT node_id FROM tracked_items
		WHERE source = ? AND terminal_at IS NULL AND node_id IS NOT NULL AND node_id != ''
		ORDER BY tracked_since
	`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// UpdateTrackedSnapshot updates just the snapshot and last_polled timestamp.
func UpdateTrackedSnapshot(database *sql.DB, source, sourceID, snapshot string) error {
	_, err := database.Exec(`
		UPDATE tracked_items SET snapshot = ?, last_polled = datetime('now')
		WHERE source = ? AND source_id = ?
	`, snapshot, source, sourceID)
	return err
}

// MarkTerminal sets terminal_at on a tracked item (merged, closed, done).
func MarkTerminal(database *sql.DB, source, sourceID string) error {
	_, err := database.Exec(`
		UPDATE tracked_items SET terminal_at = datetime('now')
		WHERE source = ? AND source_id = ?
	`, source, sourceID)
	return err
}

// PruneTerminalItems removes tracked items that have been terminal for longer than the given duration.
func PruneTerminalItems(database *sql.DB, olderThanDays int) (int64, error) {
	result, err := database.Exec(`
		DELETE FROM tracked_items
		WHERE terminal_at IS NOT NULL AND terminal_at < datetime('now', ? || ' days')
	`, -olderThanDays)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
