package domain

import (
	"encoding/json"
	"strings"
	"time"
)

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
	PrimaryEventID string `json:"primary_event_id"` // FK to events.id — the event that spawned the task; the events that bumped it are task_events rows (TaskStore.RecordEvent)
	// TeamID is the owning/attributed team for this task, or nil when the
	// owner is unresolved (author-centric routing couldn't pick a single
	// team). Pointer-for-nullable: nil = "no owner yet" — distinct from a
	// team whose id happens to be empty (which never occurs). An unowned
	// task is still visible via task_teams and gates auto-delegation off
	// (the bot never claims an unowned task); it consolidates to a single
	// team on the first human claim.
	TeamID *string `json:"team_id,omitempty"`

	// Status + lifecycle. Responsibility lives on the claim cols, not
	// here: `in_progress` is a lifecycle stage the board shows
	// independently of who (user or bot) is doing the work. A bot-claimed
	// task is placed there once, when its delegation is minted; a
	// user-claimed one is moved there by hand.
	Status         string     `json:"status"`           // queued | in_progress | done | dismissed | snoozed
	CloseReason    string     `json:"close_reason"`     // run_completed | user_completed | user_dismissed | auto_closed_by_event (another event type resolved the task) | reassigned (the task's own event type retired it: the issue was assigned again) | entity_closed | reconciled (the terminal-state sweep found the entity already finished, with no event to name) | duplicate_entity_merged (two entity rows turned out to be one Jira issue and were merged — by a migration, or by the tracker when a row learning its issue id found another row already carrying it: this card duplicated one already on the surviving entity)
	CloseEventType string     `json:"close_event_type"` // FK to events_catalog.id; the event type that triggered the close (event-driven closes: auto_closed_by_event, reassigned + entity_closed). NULL for non-event closes (run_completed, user_*, reconciled, duplicate_entity_merged)
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
	// SourceStatus is the tracker issue's state when a person claimed the task.
	// nil until a person claims a Jira- or Linear-backed task, again once the
	// task is back in the queue, and when the snapshot named no status.
	SourceStatus *SourceStatus `json:"source_status,omitempty"`

	// Claim columns. XOR via tasks_claim_xor CHECK:
	// at most one is set at a time. Both NULL = unclaimed (in the
	// team queue). Sticky past close — status in ('done', 'dismissed')
	// + non-empty claim is the audit "who finished this." Empty string = NULL.
	ClaimedByAgentID string `json:"claimed_by_agent_id,omitempty"`
	ClaimedByUserID  string `json:"claimed_by_user_id,omitempty"`

	// Version is the task's optimistic-concurrency token: every write that
	// changes one of db.TaskVersionColumns raises it, and no other write does,
	// so a scorer pass or a held close never makes a person's pending write
	// stale. A person's write names the version it read and is refused
	// (db.ErrTaskVersion) once the task has moved past it.
	Version int64 `json:"version"`

	CreatedAt time.Time `json:"created_at"`

	// Join-populated display fields — from entities, NOT stored on tasks row.
	// Populated by Tasks.Get / Tasks.List via entity JOIN.
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
	// waiting — it rises as follow-ups land while a conversation is in flight.
	SlackMessageCount int `json:"slack_message_count,omitempty"`
	// RequestedReviewer is, on a github:pr:review_requested task, the
	// requested user's login or team's "org/slug" as the event that last
	// bumped the task named it; empty on every other task. Display only: the
	// task's dedup_key is what identifies the reviewer, by GitHub's id when
	// the request carried one, and an id says nothing a person can read.
	RequestedReviewer string `json:"requested_reviewer,omitempty"`
	// VulnerablePackage is, on a task whose entity is a vulnerable package
	// (EntityKindVulnerablePackage), the entity's host and stored snapshot,
	// read in the same join; nil on every other task.
	VulnerablePackage *TaskVulnerablePackage `json:"vulnerable_package,omitempty"`

	// ConversationID is the task's current conversation: its newest un-ended
	// top-level one, the row the claim scan treats as owning the task's tree.
	// Derived on every read, never stored. On a task a person holds it is the
	// conversation their claim minted; on a task the agent holds, the agent's.
	// Empty on a task nobody holds, and on one whose queued firing has not
	// started its run. Not projected by the bare write-returning columns.
	ConversationID string `json:"conversation_id,omitempty"`

	// MemoryPending says a conversation on this task ended without leaving a
	// memory behind, and nothing has filed one for it yet. Derived on every
	// read from the conversations/conversation_memory pair — there is no
	// column, because the answer changes when a memory row lands and nothing
	// would be there to clear a flag.
	//
	// Only a conversation that has run owes one (db.ConversationHasRunSQL): a
	// conversation a person's claim minted and nobody typed in ends with
	// nothing to hand off.
	//
	// It is a gate, not a decoration: the dispatcher's claim scan reads the
	// same predicate and claims none of this task's conversations while it
	// holds, so the next turn cannot start without the handoff the ended
	// conversation owes. Both ways a conversation opens on a task pass
	// through that scan. A delegation mints a queued step, which the scan
	// holds. A person's claim mints a parked conversation and composes no
	// turn, and the first message a person sends to it waits behind the same
	// check.
	MemoryPending bool `json:"memory_pending"`
	// MemoryAttempt is the newest attempt at the memory MemoryPending is
	// waiting on, or nil when none has run yet (and always nil when the task
	// is not pending). It is what turns an unexplained wait into a readable
	// one: a failed attempt names what stopped it.
	MemoryAttempt *MemoryAttemptSummary `json:"memory_attempt,omitempty"`

	// The ordering values a task list orders on that live on no column the
	// task read otherwise carries: the sort_order of the lowest-numbered
	// enabled rule covering this event type, the display name of whoever holds
	// the claim, and the attention tier — whose move the task is. Each comes
	// from a join or subquery Tasks.List adds only for the order it is running,
	// so each is zero on a point read and on a list whose order doesn't use it.
	// None is on the wire: they are the list's own ordering arithmetic rather
	// than anything a card shows.
	//
	// They are projected so a keyset page can resume from the last row it
	// returned: the position a token carries is the ORDER BY tuple's values,
	// and a value the caller cannot see is a value it cannot resume from. Which
	// of them a given read fills is db.TaskSortKey's business — it reads
	// exactly the ones that read's order put in the tuple.
	ListSortOrder     int    `json:"-"`
	ListClaimeeName   string `json:"-"`
	ListAttentionTier int    `json:"-"`
}

// SourceStatus is the state a tracker issue was in when a person claimed its
// task: its Jira status or Linear workflow state, and its assignee. The claim
// UPDATE reads it from the entity's snapshot into tasks.source_status, and
// returning the task to the queue clears it. A requeue or undo reads it to
// reverse only what the claim changed: the issue goes back to this status
// unless the claim found it already in progress, and it is unassigned unless
// it was already the claimer's.
//
// It is the snapshot's view, so it is as old as the last poll.
type SourceStatus struct {
	// StatusID is the status or state id. Empty for a Jira snapshot taken
	// before status ids were recorded, where StatusName is the only key.
	StatusID   string `json:"status_id,omitempty"`
	StatusName string `json:"status_name,omitempty"`
	// AssigneeID is the Jira accountId or Linear user id of the issue's
	// assignee, empty when it had none.
	AssigneeID string `json:"assignee_id,omitempty"`
}

// ParseSourceStatus decodes a tasks.source_status value. An empty value, one
// that does not decode, and one naming no status are all nil: the column is
// written only by the claim UPDATE, and a value the revert cannot act on is
// the same as no record.
func ParseSourceStatus(raw string) *SourceStatus {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var s SourceStatus
	if err := json.Unmarshal([]byte(raw), &s); err != nil || (s.StatusID == "" && s.StatusName == "") {
		return nil
	}
	return &s
}

// TaskScoreUpdate holds the fields to update on a task after AI scoring.
type TaskScoreUpdate struct {
	ID                  string
	PriorityScore       float64
	AutonomySuitability float64
	PriorityReasoning   string
	Summary             string
}
