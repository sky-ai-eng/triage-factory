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
	// TeamID is the owning/attributed team for this task, or nil when the
	// owner is unresolved (author-centric routing couldn't pick a single
	// team). Pointer-for-nullable: nil = "no owner yet" — distinct from a
	// team whose id happens to be empty (which never occurs). An unowned
	// task is still visible via task_teams and gates auto-delegation off
	// (the bot never claims an unowned task); it consolidates to a single
	// team on the first human claim.
	TeamID *string `json:"team_id,omitempty"`

	// Status + lifecycle. `claimed` and `delegated` were removed
	// (responsibility moved to the claim cols). `in_progress` and
	// `in_review` were added as real lifecycle stages so the
	// board can show work moving through stages independently of who
	// (user or bot) is doing it. Bot-claimed tasks auto-transition
	// based on run state; user-claimed tasks transition manually.
	Status         string     `json:"status"`           // queued | in_progress | in_review | done | dismissed | snoozed
	CloseReason    string     `json:"close_reason"`     // run_completed | user_completed | user_dismissed | auto_closed_by_event | entity_closed
	CloseEventType string     `json:"close_event_type"` // FK to events_catalog.id; the event type that triggered the close (event-driven closes: auto_closed_by_event + entity_closed). NULL for non-event closes (run_completed, user_*)
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

	// Claim columns. XOR via tasks_claim_xor CHECK:
	// at most one is set at a time. Both NULL = unclaimed (in the
	// team queue). Sticky past close — status in ('done', 'dismissed')
	// + non-empty claim is the audit "who finished this." Empty string = NULL.
	ClaimedByAgentID string `json:"claimed_by_agent_id,omitempty"`
	ClaimedByUserID  string `json:"claimed_by_user_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	// Join-populated display fields — from entities, NOT stored on tasks row.
	// Populated by GetTask / QueuedTasks / TasksByStatus via entity JOIN.
	Title          string `json:"title"`
	SourceURL      string `json:"source_url"`
	EntitySourceID string `json:"entity_source_id"` // e.g. "owner/repo#42", a Jira issue key
	EntitySource   string `json:"entity_source"`    // "github" | "jira"
	EntityKind     string `json:"entity_kind"`      // "pr" | "issue"
	// OpenSubtaskCount is extracted from the Jira entity's snapshot_json in
	// the same join (json_extract). Zero for GitHub tasks and for Jira
	// tickets with no subtasks. Surfaced so the UI can flag a task whose
	// entity has gained subtasks since the task was created — the
	// "consider decomposing" pill.
	OpenSubtaskCount int `json:"open_subtask_count"`
	// SlackMessageCount is the number of slack:message events on this task's
	// entity (the messages addressed to the bot in the thread), counted in
	// the same entity join. Zero for non-Slack tasks. A Slack thread carries
	// one long-lived task whose generic title names only the channel, so the
	// card surfaces this count to say how much of the conversation is
	// waiting — it rises as follow-ups land while a run is in flight.
	SlackMessageCount int `json:"slack_message_count,omitempty"`
}

// TaskScoreUpdate holds the fields to update on a task after AI scoring.
type TaskScoreUpdate struct {
	ID                  string
	PriorityScore       float64
	AutonomySuitability float64
	PriorityReasoning   string
	Summary             string
}
