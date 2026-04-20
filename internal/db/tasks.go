package db

import (
	"database/sql"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// --- Column lists for task queries ----------------------------------------
//
// Every query that feeds into scanTask must use these columns in this order.
// The entity JOIN columns are appended for display.

const taskColumnsWithEntity = `
	t.id, t.entity_id, t.event_type, t.dedup_key, t.primary_event_id,
	t.status, t.priority_score, t.ai_summary, t.autonomy_suitability,
	t.priority_reasoning, t.scoring_status, t.severity, t.relevance_reason,
	t.source_status, t.snooze_until, t.close_reason, t.close_event_type,
	t.closed_at, t.created_at,
	COALESCE(e.title, ''), COALESCE(e.url, ''), e.source_id, e.source, e.kind,
	-- SQLite json_extract returns null for missing paths or null snapshot;
	-- COALESCE to 0 so GitHub tasks and subtask-less Jira tickets report
	-- cleanly as "no open subtasks" instead of NULL.
	COALESCE(json_extract(e.snapshot_json, '$.open_subtask_count'), 0)`

// FindOrCreateTask implements the dedup logic via the partial unique index
// (entity_id, event_type, dedup_key) WHERE status NOT IN ('done','dismissed').
// If an active task exists, returns it with created=false. Otherwise creates
// one with created=true.
func FindOrCreateTask(db *sql.DB, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64) (*domain.Task, bool, error) {
	return FindOrCreateTaskAt(db, entityID, eventType, dedupKey, primaryEventID, defaultPriority, time.Now())
}

// FindOrCreateTaskAt is the same as FindOrCreateTask but stamps a caller-
// supplied createdAt on the new row. Used by initial-discovery backfills
// that represent activity older than "now" — e.g. a pending review request
// observed on a 2-week-old PR should show the PR's age on the card, not
// the moment we first polled. Existing tasks (find branch) keep their
// original created_at.
func FindOrCreateTaskAt(db *sql.DB, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error) {
	// Try to find an existing active task.
	var existing domain.Task
	err := scanTaskRow(db.QueryRow(`
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.entity_id = ? AND t.event_type = ? AND t.dedup_key = ?
			AND t.status NOT IN ('done', 'dismissed')
		LIMIT 1
	`, entityID, eventType, dedupKey), &existing)

	if err == nil {
		return &existing, false, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}

	// Create new task. If a concurrent goroutine raced us past the SELECT
	// above, the partial unique index (entity_id, event_type, dedup_key)
	// WHERE status NOT IN ('done','dismissed') will reject the INSERT. In
	// that case, re-read the winner's row.
	id := uuid.New().String()
	_, err = db.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
		                   status, priority_score, scoring_status, created_at)
		VALUES (?, ?, ?, ?, ?, 'queued', ?, 'pending', ?)
	`, id, entityID, eventType, dedupKey, primaryEventID, defaultPriority, createdAt)
	if err != nil {
		// Race: another goroutine created the task between our SELECT and
		// INSERT. Re-read to return the winner's row.
		var raced domain.Task
		err2 := scanTaskRow(db.QueryRow(`
			SELECT `+taskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id
			WHERE t.entity_id = ? AND t.event_type = ? AND t.dedup_key = ?
				AND t.status NOT IN ('done', 'dismissed')
			LIMIT 1
		`, entityID, eventType, dedupKey), &raced)
		if err2 == nil {
			return &raced, false, nil
		}
		// Genuine error (not a race).
		return nil, false, err
	}

	task, err := GetTask(db, id)
	if err != nil {
		return nil, false, err
	}
	return task, true, nil
}

// BumpTask records a new matching event on an existing task. Does NOT update
// primary_event_id — that stays as the original spawning event (the task_events
// junction with kind=bumped tracks subsequent events). If the task is snoozed,
// un-snoozes it (wake-on-bump: the snooze premise "nothing new" is invalidated).
func BumpTask(db *sql.DB, taskID, eventID string) error {
	_, err := db.Exec(`
		UPDATE tasks
		SET status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END,
		    snooze_until = CASE WHEN status = 'snoozed' THEN NULL ELSE snooze_until END
		WHERE id = ?
	`, taskID)
	return err
}

// CloseTask sets a task to done with the given close reason. Used by run-
// completion, inline close checks, and user actions (dismiss/claim-done).
func CloseTask(db *sql.DB, taskID, closeReason, closeEventType string) error {
	now := time.Now()
	var cet *string
	if closeEventType != "" {
		cet = &closeEventType
	}
	_, err := db.Exec(`
		UPDATE tasks SET status = 'done', close_reason = ?, close_event_type = ?,
		                 closed_at = ?
		WHERE id = ? AND status NOT IN ('done', 'dismissed')
	`, closeReason, cet, now, taskID)
	return err
}

// CloseAllEntityTasks closes every active task on an entity with the given
// close reason. Returns the number of tasks closed. Used by entity lifecycle
// (close_reason="entity_closed").
func CloseAllEntityTasks(db *sql.DB, entityID, closeReason string) (int, error) {
	now := time.Now()
	result, err := db.Exec(`
		UPDATE tasks SET status = 'done', close_reason = ?, closed_at = ?
		WHERE entity_id = ? AND status NOT IN ('done', 'dismissed')
	`, closeReason, now, entityID)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// SetTaskStatus updates a task's status. Used by the router (queued→delegated)
// and the swipe handler (queued→claimed, etc.).
func SetTaskStatus(db *sql.DB, taskID, status string) error {
	_, err := db.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, status, taskID)
	return err
}

// RecordTaskEvent inserts into the task_events junction table.
func RecordTaskEvent(db *sql.DB, taskID, eventID, kind string) error {
	_, err := db.Exec(`
		INSERT OR IGNORE INTO task_events (task_id, event_id, kind, created_at)
		VALUES (?, ?, ?, ?)
	`, taskID, eventID, kind, time.Now())
	return err
}

// FindActiveTasksByEntityAndType returns all non-terminal tasks for an entity
// matching the given event type. Used by inline close checks to find sibling
// tasks to close.
func FindActiveTasksByEntityAndType(db *sql.DB, entityID, eventType string) ([]domain.Task, error) {
	return queryTasks(db, `
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.entity_id = ? AND t.event_type = ? AND t.status NOT IN ('done', 'dismissed')
	`, entityID, eventType)
}

// FindActiveTasksByEntity returns all non-terminal tasks for an entity,
// regardless of event type. Used by entity lifecycle to close everything.
func FindActiveTasksByEntity(db *sql.DB, entityID string) ([]domain.Task, error) {
	return queryTasks(db, `
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.entity_id = ? AND t.status NOT IN ('done', 'dismissed')
	`, entityID)
}

// EntityIDsWithActiveTasks returns the set of entity IDs that have at least
// one non-terminal task, scoped to the given entity source (e.g. "jira").
// Used to batch-check active-task membership in one query instead of N.
func EntityIDsWithActiveTasks(db *sql.DB, source string) (map[string]struct{}, error) {
	rows, err := db.Query(`
		SELECT DISTINCT t.entity_id
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE e.source = ? AND t.status NOT IN ('done', 'dismissed')
	`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

// GetTask returns a single task by ID, joined with its entity for display fields.
func GetTask(db *sql.DB, id string) (*domain.Task, error) {
	var t domain.Task
	err := scanTaskRow(db.QueryRow(`
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.id = ?
	`, id), &t)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// QueuedTasks returns queued tasks ordered by task_rules.sort_order (category
// ordering) then priority_score DESC within each tier. JOINs entities for
// display and task_rules for ordering.
func QueuedTasks(db *sql.DB) ([]domain.Task, error) {
	return queryTasks(db, `
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		LEFT JOIN task_rules tr ON t.event_type = tr.event_type AND tr.enabled = 1
		WHERE t.status = 'queued'
			AND (t.snooze_until IS NULL OR t.snooze_until <= datetime('now'))
		ORDER BY COALESCE(tr.sort_order, 999) ASC, COALESCE(t.priority_score, 0.5) DESC
	`)
}

// TasksByStatus returns tasks with the given status, ordered by priority.
func TasksByStatus(db *sql.DB, status string) ([]domain.Task, error) {
	return queryTasks(db, `
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.status = ?
		ORDER BY COALESCE(t.priority_score, 0.5) DESC
	`, status)
}

// --- Breaker queries (query-based, no counter column) --------------------

// CountConsecutiveFailedRuns counts consecutive non-success auto-runs at the
// tail of runs for (entity_id, prompt_id), stopping at the first 'completed'
// row. Used by the router to check the breaker threshold.
func CountConsecutiveFailedRuns(db *sql.DB, entityID, promptID string) (int, error) {
	var count int
	err := db.QueryRow(`
		WITH recent AS (
			SELECT r.status, r.started_at
			FROM runs r
			JOIN tasks t ON r.task_id = t.id
			WHERE t.entity_id = ?
				AND r.prompt_id = ?
				AND r.trigger_type = 'event'
			ORDER BY r.started_at DESC
			LIMIT 20
		)
		SELECT COUNT(*)
		FROM recent
		WHERE status IN ('failed', 'task_unsolvable')
			AND started_at > (
				SELECT COALESCE(MAX(started_at), '1970-01-01')
				FROM recent WHERE status = 'completed'
			)
	`, entityID, promptID).Scan(&count)
	return count, err
}

// --- Internal query helpers -----------------------------------------------

func queryTasks(database *sql.DB, query string, args ...any) ([]domain.Task, error) {
	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []domain.Task
	for rows.Next() {
		var t domain.Task
		if err := scanTaskFields(rows, &t); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func scanTaskRow(row *sql.Row, t *domain.Task) error {
	return scanFields(row, t)
}

// scanFields works for both *sql.Row and *sql.Rows via the Scanner interface.
func scanFields(scanner interface{ Scan(...any) error }, t *domain.Task) error {
	var priorityScore, autonomySuitability sql.NullFloat64
	var aiSummary, priorityReasoning, severity, relevanceReason, sourceStatus sql.NullString
	var scoringStatus, closeReason, closeEventType sql.NullString
	var snoozeUntil, closedAt sql.NullTime

	err := scanner.Scan(
		&t.ID, &t.EntityID, &t.EventType, &t.DedupKey, &t.PrimaryEventID,
		&t.Status, &priorityScore, &aiSummary, &autonomySuitability,
		&priorityReasoning, &scoringStatus, &severity, &relevanceReason,
		&sourceStatus, &snoozeUntil, &closeReason, &closeEventType,
		&closedAt, &t.CreatedAt,
		// Entity JOIN columns:
		&t.Title, &t.SourceURL, &t.EntitySourceID, &t.EntitySource, &t.EntityKind,
		&t.OpenSubtaskCount,
	)
	if err != nil {
		return err
	}

	if priorityScore.Valid {
		t.PriorityScore = &priorityScore.Float64
	}
	if autonomySuitability.Valid {
		t.AutonomySuitability = &autonomySuitability.Float64
	}
	t.AISummary = aiSummary.String
	t.PriorityReasoning = priorityReasoning.String
	t.Severity = severity.String
	t.RelevanceReason = relevanceReason.String
	t.SourceStatus = sourceStatus.String
	t.ScoringStatus = scoringStatus.String
	t.CloseReason = closeReason.String
	t.CloseEventType = closeEventType.String
	if snoozeUntil.Valid {
		t.SnoozeUntil = &snoozeUntil.Time
	}
	if closedAt.Valid {
		t.ClosedAt = &closedAt.Time
	}
	return nil
}

func scanTaskFields(rows *sql.Rows, t *domain.Task) error {
	return scanFields(rows, t)
}

// --- Enabled rules query (for routing) ------------------------------------

// GetEnabledRulesForEvent returns all enabled task_rules for an event type.
func GetEnabledRulesForEvent(db *sql.DB, eventType string) ([]domain.TaskRule, error) {
	rows, err := db.Query(`
		SELECT id, event_type, scope_predicate_json, enabled, name,
		       default_priority, sort_order, source, created_at, updated_at
		FROM task_rules
		WHERE event_type = ? AND enabled = 1
	`, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []domain.TaskRule
	for rows.Next() {
		var r domain.TaskRule
		if err := rows.Scan(&r.ID, &r.EventType, &r.ScopePredicateJSON, &r.Enabled, &r.Name,
			&r.DefaultPriority, &r.SortOrder, &r.Source, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}
