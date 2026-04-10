package db

import (
	"database/sql"

	"github.com/sky-ai-eng/todo-triage/internal/domain"
)

// UpsertTrackedItem registers a tracked item. On conflict, updates task_id and
// node_id but preserves the existing snapshot so the refresh→diff phase can
// detect transitions since the last cycle.
func UpsertTrackedItem(database *sql.DB, item domain.TrackedItem) error {
	_, err := database.Exec(`
		INSERT INTO tracked_items (source, source_id, task_id, node_id, snapshot, tracked_since, last_polled)
		VALUES (?, ?, ?, ?, '{}', datetime('now'), datetime('now'))
		ON CONFLICT(source, source_id) DO UPDATE SET
			task_id = excluded.task_id,
			node_id = COALESCE(excluded.node_id, tracked_items.node_id)
	`, item.Source, item.SourceID, nullIfEmpty(item.TaskID), nullIfEmpty(item.NodeID))
	return err
}

// GetTrackedItem returns a single tracked item by source and source_id, or nil.
func GetTrackedItem(database *sql.DB, source, sourceID string) (*domain.TrackedItem, error) {
	var item domain.TrackedItem
	var taskID, nodeID sql.NullString
	var lastPolled, terminalAt sql.NullTime
	err := database.QueryRow(`
		SELECT source, source_id, task_id, node_id, snapshot, tracked_since, last_polled, terminal_at
		FROM tracked_items WHERE source = ? AND source_id = ?
	`, source, sourceID).Scan(
		&item.Source, &item.SourceID, &taskID, &nodeID,
		&item.Snapshot, &item.TrackedSince, &lastPolled, &terminalAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
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
		SELECT source, source_id, task_id, node_id, snapshot, tracked_since, last_polled
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
		var taskID, nodeID sql.NullString
		var lastPolled sql.NullTime
		if err := rows.Scan(&item.Source, &item.SourceID, &taskID, &nodeID, &item.Snapshot, &item.TrackedSince, &lastPolled); err != nil {
			return nil, err
		}
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

// ReactivateTrackedItem clears terminal_at for an item that has reappeared in a non-terminal state
// (e.g., a closed PR was reopened). Returns true if a row was actually reactivated.
func ReactivateTrackedItem(database *sql.DB, source, sourceID string) (bool, error) {
	result, err := database.Exec(`
		UPDATE tracked_items SET terminal_at = NULL
		WHERE source = ? AND source_id = ? AND terminal_at IS NOT NULL
	`, source, sourceID)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// MarkTerminal sets terminal_at on a tracked item (merged, closed, done).
func MarkTerminal(database *sql.DB, source, sourceID string) error {
	_, err := database.Exec(`
		UPDATE tracked_items SET terminal_at = datetime('now')
		WHERE source = ? AND source_id = ?
	`, source, sourceID)
	return err
}

// ClearTrackedItems removes all tracked items for a source (e.g., when credentials are disabled).
func ClearTrackedItems(database *sql.DB, source string) error {
	_, err := database.Exec(`DELETE FROM tracked_items WHERE source = ?`, source)
	return err
}
