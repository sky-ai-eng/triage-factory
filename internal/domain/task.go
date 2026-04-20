package domain

import "time"

// Task is an actionable situation — spawned by a task_rule or prompt_trigger
// match on an event, lives in the user's queue/board. Mirrors the `tasks`
// table; display-oriented fields (Title, SourceURL, etc.) are populated from
// an entities JOIN at query time, not stored on the tasks row.
type Task struct {
	// Identity — stored on the tasks row.
	ID             string `json:"id"`
	EntityID       string `json:"entity_id"`        // FK to entities.id
	EventType      string `json:"event_type"`       // FK to events_catalog.id — the event that spawned this task
	DedupKey       string `json:"dedup_key"`        // open-set discriminator; empty for most event types
	PrimaryEventID string `json:"primary_event_id"` // FK to events.id — the specific event that spawned/last bumped

	// Status + lifecycle.
	Status         string     `json:"status"`           // queued | claimed | delegated | done | dismissed | snoozed
	CloseReason    string     `json:"close_reason"`     // run_completed | user_claimed | user_dismissed | auto_closed_by_event | entity_closed
	CloseEventType string     `json:"close_event_type"` // FK to events_catalog.id; set when close_reason=auto_closed_by_event
	ClosedAt       *time.Time `json:"closed_at"`
	SnoozeUntil    *time.Time `json:"snooze_until"`

	// AI scoring.
	PriorityScore       *float64 `json:"priority_score"`
	AutonomySuitability *float64 `json:"autonomy_suitability"`
	AISummary           string   `json:"ai_summary"`
	PriorityReasoning   string   `json:"priority_reasoning"`
	ScoringStatus       string   `json:"scoring_status"` // pending | in_progress | scored

	// Display context (stored on row but derived from event/entity).
	Severity        string `json:"severity"`
	RelevanceReason string `json:"relevance_reason"` // "review_requested" | "authored" | "mentioned" | "assigned"
	SourceStatus    string `json:"source_status"`    // captured for undo (e.g., Jira ticket's prior status)

	CreatedAt time.Time `json:"created_at"`

	// Join-populated display fields — from entities, NOT stored on tasks row.
	// Populated by GetTask / QueuedTasks / TasksByStatus via entity JOIN.
	Title          string `json:"title"`
	SourceURL      string `json:"source_url"`
	EntitySourceID string `json:"entity_source_id"` // e.g. "owner/repo#42", "SKY-123"
	EntitySource   string `json:"entity_source"`    // "github" | "jira"
	EntityKind     string `json:"entity_kind"`      // "pr" | "issue"
	// OpenSubtaskCount is extracted from the Jira entity's snapshot_json in
	// the same join (json_extract). Zero for GitHub tasks and for Jira
	// tickets with no subtasks. Surfaced so the UI can flag a task whose
	// entity has gained subtasks since the task was created — SKY-173's
	// "consider decomposing" pill.
	OpenSubtaskCount int `json:"open_subtask_count"`
}

// TaskScoreUpdate holds the fields to update on a task after AI scoring.
type TaskScoreUpdate struct {
	ID                  string
	PriorityScore       float64
	AutonomySuitability float64
	PriorityReasoning   string
	Summary             string
}
