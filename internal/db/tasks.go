package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)


// taskColumns is the canonical SELECT column list for queryTasks. Every query that
// feeds into queryTasks must use this exact list so the Scan stays in sync.
const taskColumns = `id, source, source_id, source_url, title, description, repo, author, labels, severity,
       diff_additions, diff_deletions, files_changed, ci_status, relevance_reason, source_status, scoring_status,
       event_type, created_at, fetched_at, status, priority_score, ai_summary,
       priority_reasoning, agent_confidence, snooze_until`

// qualifiedTaskColumns is taskColumns with tasks. prefix, for use in JOINs where column names are ambiguous.
const qualifiedTaskColumns = `tasks.id, tasks.source, tasks.source_id, tasks.source_url, tasks.title, tasks.description, tasks.repo, tasks.author, tasks.labels, tasks.severity,
       tasks.diff_additions, tasks.diff_deletions, tasks.files_changed, tasks.ci_status, tasks.relevance_reason, tasks.source_status, tasks.scoring_status,
       tasks.event_type, tasks.created_at, tasks.fetched_at, tasks.status, tasks.priority_score, tasks.ai_summary,
       tasks.priority_reasoning, tasks.agent_confidence, tasks.snooze_until`

// UpsertTask inserts a new task or updates an existing one matched by (source, source_id).
// Only updates metadata fields — does not overwrite status, priority_score, ai_summary,
// or agent_confidence so that user/AI state is preserved across polls.
func UpsertTask(db *sql.DB, t domain.Task) error {
	labelsJSON, err := json.Marshal(t.Labels)
	if err != nil {
		return fmt.Errorf("marshal labels: %w", err)
	}

	_, err = db.Exec(`
		INSERT INTO tasks (id, source, source_id, source_url, title, description, repo, author, labels, severity, diff_additions, diff_deletions, files_changed, ci_status, relevance_reason, source_status, event_type, status, created_at, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source, source_id) DO UPDATE SET
			source_url = excluded.source_url,
			title = excluded.title,
			description = excluded.description,
			repo = excluded.repo,
			author = excluded.author,
			labels = excluded.labels,
			severity = excluded.severity,
			diff_additions = excluded.diff_additions,
			diff_deletions = excluded.diff_deletions,
			files_changed = excluded.files_changed,
			ci_status = excluded.ci_status,
			relevance_reason = excluded.relevance_reason,
			source_status = excluded.source_status,
			event_type = COALESCE(NULLIF(excluded.event_type, ''), tasks.event_type),
			fetched_at = excluded.fetched_at
	`,
		t.ID, t.Source, t.SourceID, t.SourceURL,
		t.Title, t.Description, t.Repo, t.Author,
		string(labelsJSON), t.Severity, t.DiffAdditions, t.DiffDeletions, t.FilesChanged,
		t.CIStatus, t.RelevanceReason, t.SourceStatus, t.EventType, t.Status,
		t.CreatedAt, t.FetchedAt,
	)
	return err
}

// QueuedTasks returns queued tasks, filtered to only enabled event types.
// Orders by user-defined event type sort_order first, then AI priority_score within each tier.
// Tasks with no event_type (or an unknown one) sort last.
func QueuedTasks(db *sql.DB) ([]domain.Task, error) {
	return queryTasks(db, `SELECT `+qualifiedTaskColumns+` FROM tasks
		LEFT JOIN event_types et ON tasks.event_type = et.id
		WHERE tasks.status = 'queued'
			AND (tasks.snooze_until IS NULL OR tasks.snooze_until <= datetime('now'))
			AND (tasks.event_type IS NULL OR et.enabled = 1 OR et.enabled IS NULL)
		ORDER BY COALESCE(et.sort_order, 999) ASC, COALESCE(tasks.priority_score, 0.5) DESC`)
}

// TasksByStatus returns tasks filtered by status.
func TasksByStatus(db *sql.DB, status string) ([]domain.Task, error) {
	return queryTasks(db, `SELECT `+taskColumns+` FROM tasks
		WHERE status = ?
		ORDER BY COALESCE(priority_score, 0.5) DESC`, status)
}

// GetTask returns a single task by ID.
func GetTask(db *sql.DB, id string) (*domain.Task, error) {
	tasks, err := queryTasks(db, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	return &tasks[0], nil
}

func queryTasks(database *sql.DB, query string, args ...any) ([]domain.Task, error) {
	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []domain.Task
	for rows.Next() {
		var t domain.Task
		var labelsStr sql.NullString
		var desc, repo, author, severity, aiSummary, priorityReasoning sql.NullString
		var ciStatus, relevanceReason, sourceStatus, scoringStatus, eventType sql.NullString
		var priorityScore, agentConfidence sql.NullFloat64
		var diffAdditions, diffDeletions, filesChanged sql.NullInt64
		var snoozeUntil sql.NullTime

		err := rows.Scan(
			&t.ID, &t.Source, &t.SourceID, &t.SourceURL, &t.Title,
			&desc, &repo, &author, &labelsStr, &severity,
			&diffAdditions, &diffDeletions, &filesChanged, &ciStatus, &relevanceReason, &sourceStatus, &scoringStatus,
			&eventType, &t.CreatedAt, &t.FetchedAt,
			&t.Status, &priorityScore, &aiSummary,
			&priorityReasoning, &agentConfidence, &snoozeUntil,
		)
		if err != nil {
			return nil, err
		}

		t.Description = desc.String
		t.Repo = repo.String
		t.Author = author.String
		t.Severity = severity.String
		t.AISummary = aiSummary.String
		t.PriorityReasoning = priorityReasoning.String
		t.CIStatus = ciStatus.String
		t.RelevanceReason = relevanceReason.String
		t.SourceStatus = sourceStatus.String
		t.ScoringStatus = scoringStatus.String
		t.EventType = eventType.String
		t.DiffAdditions = int(diffAdditions.Int64)
		t.DiffDeletions = int(diffDeletions.Int64)
		t.FilesChanged = int(filesChanged.Int64)

		if priorityScore.Valid {
			t.PriorityScore = &priorityScore.Float64
		}
		if agentConfidence.Valid {
			t.AgentConfidence = &agentConfidence.Float64
		}
		if snoozeUntil.Valid {
			t.SnoozeUntil = &snoozeUntil.Time
		}
		if labelsStr.Valid {
			_ = json.Unmarshal([]byte(labelsStr.String), &t.Labels)
		}

		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}
