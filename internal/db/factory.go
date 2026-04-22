package db

import (
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Factory-view queries. All scoped to "active" state (entities that haven't
// closed, runs that haven't terminated) and to 24h windows for throughput
// counts. Used by the /api/factory/snapshot handler to drive the 2.5D
// visualization at /factory.

// factoryActiveRunStatuses is the set of run.status values we treat as "in
// flight" for the factory view. Matches the X-button window in AgentCard —
// every state before a terminal transition (completed | failed | cancelled
// | task_unsolvable). pending_approval counts as active: the run is paused
// waiting for user input, not done.
var factoryActiveRunStatuses = []string{
	"initializing",
	"cloning",
	"fetching",
	"worktree_created",
	"agent_starting",
	"running",
	"pending_approval",
}

// EventCountsByTypeSince counts events per event_type that were emitted
// after `since`. Keys with zero counts are absent. Uses
// idx_events_type_created for the scan.
func EventCountsByTypeSince(database *sql.DB, since time.Time) (map[string]int, error) {
	rows, err := database.Query(`
		SELECT event_type, COUNT(*)
		FROM events
		WHERE created_at > ?
		GROUP BY event_type
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var eventType string
		var count int
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, err
		}
		out[eventType] = count
	}
	return out, rows.Err()
}

// TaskCountsByEventTypeSince counts tasks per event_type created after
// `since`. Used alongside EventCountsByTypeSince to compute the "triggered
// / seen" ratio displayed in the station overlay.
func TaskCountsByEventTypeSince(database *sql.DB, since time.Time) (map[string]int, error) {
	rows, err := database.Query(`
		SELECT event_type, COUNT(*)
		FROM tasks
		WHERE created_at > ?
		GROUP BY event_type
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var eventType string
		var count int
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, err
		}
		out[eventType] = count
	}
	return out, rows.Err()
}

// FactoryActiveRun is a run + its task + enough entity context to render an
// overlay row and decide ownership tint. Returned by ListFactoryActiveRuns
// so the handler doesn't have to do N+1 follow-up queries.
type FactoryActiveRun struct {
	Run            domain.AgentRun
	Task           domain.Task
	EntityAuthor   string // PR author login (github) or assignee (jira); "" if unknown
	EntityEventTyp string // task.event_type; pre-copied for keyed lookup
}

// ListFactoryActiveRuns returns every run currently in-flight (status in
// factoryActiveRunStatuses) joined with its task and entity. Ordered by
// started_at so the overlay can render most-recent-first without client-side
// sorting.
func ListFactoryActiveRuns(database *sql.DB) ([]FactoryActiveRun, error) {
	placeholders := "?"
	args := make([]any, 0, len(factoryActiveRunStatuses))
	args = append(args, factoryActiveRunStatuses[0])
	for i := 1; i < len(factoryActiveRunStatuses); i++ {
		placeholders += ", ?"
		args = append(args, factoryActiveRunStatuses[i])
	}

	query := `
		SELECT
			r.id, r.task_id, COALESCE(r.prompt_id, ''),
			r.status, COALESCE(r.model, ''), r.started_at, r.completed_at,
			r.total_cost_usd, r.duration_ms, r.num_turns,
			COALESCE(r.stop_reason, ''), COALESCE(r.worktree_path, ''),
			COALESCE(r.result_summary, ''), COALESCE(r.session_id, ''),
			r.memory_missing, r.trigger_type, COALESCE(r.trigger_id, ''),
			` + taskColumnsWithEntity + `
		FROM runs r
		JOIN tasks t ON r.task_id = t.id
		JOIN entities e ON t.entity_id = e.id
		WHERE r.status IN (` + placeholders + `)
		ORDER BY r.started_at DESC
	`

	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FactoryActiveRun
	for rows.Next() {
		var r domain.AgentRun
		var t domain.Task
		var completedAt sql.NullTime
		var costUSD sql.NullFloat64
		var durationMs, numTurns sql.NullInt64

		if err := rows.Scan(
			&r.ID, &r.TaskID, &r.PromptID,
			&r.Status, &r.Model, &r.StartedAt, &completedAt,
			&costUSD, &durationMs, &numTurns,
			&r.StopReason, &r.WorktreePath,
			&r.ResultSummary, &r.SessionID,
			&r.MemoryMissing, &r.TriggerType, &r.TriggerID,
			&t.ID, &t.EntityID, &t.EventType, &t.DedupKey, &t.PrimaryEventID,
			&t.Status, &t.PriorityScore, &t.AISummary, &t.AutonomySuitability,
			&t.PriorityReasoning, &t.ScoringStatus, &t.Severity, &t.RelevanceReason,
			&t.SourceStatus, &t.SnoozeUntil, &t.CloseReason, &t.CloseEventType,
			&t.ClosedAt, &t.CreatedAt,
			&t.Title, &t.SourceURL, &t.EntitySourceID, &t.EntitySource, &t.EntityKind,
			&t.OpenSubtaskCount,
		); err != nil {
			return nil, err
		}
		if completedAt.Valid {
			r.CompletedAt = &completedAt.Time
		}
		if costUSD.Valid {
			r.TotalCostUSD = &costUSD.Float64
		}
		if durationMs.Valid {
			v := int(durationMs.Int64)
			r.DurationMs = &v
		}
		if numTurns.Valid {
			v := int(numTurns.Int64)
			r.NumTurns = &v
		}
		out = append(out, FactoryActiveRun{Run: r, Task: t, EntityEventTyp: t.EventType})
	}
	return out, rows.Err()
}

// FactoryEntityRow is an active entity plus the event_type of its most
// recent event. LatestEventType is "" if the entity has no recorded events
// (fresh discovery before any diff has fired). LatestEventAt is nil under
// the same condition.
type FactoryEntityRow struct {
	Entity          domain.Entity
	LatestEventType string
	LatestEventAt   *time.Time
}

// ListFactoryEntities returns up to `limit` active entities with their
// most-recent-event context. Ordered by last_polled_at DESC so fresh
// movement lives at the top of the list when the frontend caps display.
//
// The latest-event subquery uses idx_events_entity_created and runs per
// row — fine at 100-entity scope, would need reworking at 10k+ (window
// function or a materialized "current" column).
func ListFactoryEntities(database *sql.DB, limit int) ([]FactoryEntityRow, error) {
	rows, err := database.Query(`
		SELECT
			e.id, e.source, e.source_id, e.kind,
			COALESCE(e.title, ''), COALESCE(e.url, ''),
			COALESCE(e.snapshot_json, ''), COALESCE(e.description, ''),
			e.state, e.created_at, e.last_polled_at, e.closed_at,
			(SELECT event_type FROM events WHERE entity_id = e.id ORDER BY created_at DESC LIMIT 1),
			(SELECT created_at FROM events WHERE entity_id = e.id ORDER BY created_at DESC LIMIT 1)
		FROM entities e
		WHERE e.state = 'active'
		ORDER BY e.last_polled_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FactoryEntityRow
	for rows.Next() {
		var row FactoryEntityRow
		var latestType sql.NullString
		var latestAt sql.NullTime
		if err := rows.Scan(
			&row.Entity.ID, &row.Entity.Source, &row.Entity.SourceID, &row.Entity.Kind,
			&row.Entity.Title, &row.Entity.URL,
			&row.Entity.SnapshotJSON, &row.Entity.Description,
			&row.Entity.State, &row.Entity.CreatedAt, &row.Entity.LastPolledAt, &row.Entity.ClosedAt,
			&latestType, &latestAt,
		); err != nil {
			return nil, err
		}
		if latestType.Valid {
			row.LatestEventType = latestType.String
		}
		if latestAt.Valid {
			row.LatestEventAt = &latestAt.Time
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
