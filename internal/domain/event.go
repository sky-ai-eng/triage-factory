package domain

import "time"

// EventType defines a specific kind of triage event that can occur.
// These form a closed taxonomy — new types are added by extending the seed list.
type EventType struct {
	ID              string  `json:"id"`               // e.g. "github:pr:review_requested"
	Source          string  `json:"source"`            // "github", "jira", "system"
	Category        string  `json:"category"`          // "pr", "issue", "scoring", "delegation"
	Label           string  `json:"label"`             // Human-readable: "Review Requested"
	Description     string  `json:"description"`
	DefaultPriority float64 `json:"default_priority"`
}

// Event is a single occurrence of an EventType, emitted by a poller or system component.
type Event struct {
	ID        int64     `json:"id,omitempty"` // DB-assigned
	EventType string    `json:"event_type"`   // FK to EventType.ID
	TaskID    string    `json:"task_id"`      // FK to Task.ID
	SourceID  string    `json:"source_id"`    // The source item identifier (PR number, Jira key)
	Metadata  string    `json:"metadata"`     // JSON blob with event-specific data
	CreatedAt time.Time `json:"created_at"`
}

// PollerState stores the last-known snapshot for a source item, used to diff and detect transitions.
type PollerState struct {
	Source    string `json:"source"`
	SourceID string `json:"source_id"`
	StateJSON string `json:"state_json"`
}

// --- Event Type Taxonomy ---
// These are the canonical event types seeded into the DB on first run.

// GitHub PR events
const (
	EventGitHubPRReviewRequested = "github:pr:review_requested"
	EventGitHubPRReviewReceived  = "github:pr:review_received"
	EventGitHubPRApproved        = "github:pr:approved"
	EventGitHubPRChangesReq      = "github:pr:changes_requested"
	EventGitHubPRCIPassed        = "github:pr:ci_passed"
	EventGitHubPRCIFailed        = "github:pr:ci_failed"
	EventGitHubPRMerged          = "github:pr:merged"
	EventGitHubPRMentioned       = "github:pr:mentioned"
	EventGitHubPRConflicts       = "github:pr:conflicts"
	EventGitHubPROpened          = "github:pr:opened"
	EventGitHubPRReadyForReview  = "github:pr:ready_for_review"
)

// Jira events
const (
	EventJiraIssueAvailable     = "jira:issue:available"
	EventJiraIssueAssigned      = "jira:issue:assigned"
	EventJiraIssueStatusChanged = "jira:issue:status_changed"
	EventJiraIssueCompleted     = "jira:issue:completed"
	EventJiraIssuePriorityChanged = "jira:issue:priority_changed"
)

// System events
const (
	EventSystemPollCompleted       = "system:poll:completed"
	EventSystemScoringCompleted    = "system:scoring:completed"
	EventSystemDelegationCompleted = "system:delegation:completed"
	EventSystemDelegationFailed    = "system:delegation:failed"
)

// AllEventTypes returns the full seed catalog.
func AllEventTypes() []EventType {
	return []EventType{
		// GitHub PR events
		{ID: EventGitHubPRReviewRequested, Source: "github", Category: "pr", Label: "Review Requested", Description: "Someone requested your review on a PR", DefaultPriority: 0.8},
		{ID: EventGitHubPRReviewReceived, Source: "github", Category: "pr", Label: "Review Received", Description: "Your PR received a review", DefaultPriority: 0.7},
		{ID: EventGitHubPRApproved, Source: "github", Category: "pr", Label: "PR Approved", Description: "Your PR was approved", DefaultPriority: 0.9},
		{ID: EventGitHubPRChangesReq, Source: "github", Category: "pr", Label: "Changes Requested", Description: "A reviewer requested changes on your PR", DefaultPriority: 0.85},
		{ID: EventGitHubPRCIPassed, Source: "github", Category: "pr", Label: "CI Passed", Description: "CI checks passed on your PR", DefaultPriority: 0.6},
		{ID: EventGitHubPRCIFailed, Source: "github", Category: "pr", Label: "CI Failed", Description: "CI checks failed on your PR", DefaultPriority: 0.75},
		{ID: EventGitHubPRMerged, Source: "github", Category: "pr", Label: "PR Merged", Description: "Your PR was merged", DefaultPriority: 0.3},
		{ID: EventGitHubPRMentioned, Source: "github", Category: "pr", Label: "Mentioned", Description: "You were @mentioned in a PR", DefaultPriority: 0.5},
		{ID: EventGitHubPRConflicts, Source: "github", Category: "pr", Label: "Merge Conflicts", Description: "Your PR has merge conflicts", DefaultPriority: 0.7},
		{ID: EventGitHubPROpened, Source: "github", Category: "pr", Label: "Authored PR", Description: "Your open pull request is being tracked", DefaultPriority: 0.4},
		{ID: EventGitHubPRReadyForReview, Source: "github", Category: "pr", Label: "Ready for Review", Description: "A draft PR was marked ready for review", DefaultPriority: 0.7},

		// Jira events
		{ID: EventJiraIssueAvailable, Source: "jira", Category: "issue", Label: "Issue Available", Description: "New unassigned issue in pickup queue", DefaultPriority: 0.5},
		{ID: EventJiraIssueAssigned, Source: "jira", Category: "issue", Label: "Issue Assigned", Description: "Issue was assigned to you", DefaultPriority: 0.6},
		{ID: EventJiraIssueStatusChanged, Source: "jira", Category: "issue", Label: "Status Changed", Description: "Issue status changed", DefaultPriority: 0.4},
		{ID: EventJiraIssueCompleted, Source: "jira", Category: "issue", Label: "Issue Completed", Description: "Issue was marked as done", DefaultPriority: 0.2},
		{ID: EventJiraIssuePriorityChanged, Source: "jira", Category: "issue", Label: "Priority Changed", Description: "Issue priority was changed", DefaultPriority: 0.5},

		// System events
		{ID: EventSystemPollCompleted, Source: "system", Category: "poll", Label: "Poll Complete", Description: "A poller finished a cycle and ingested tasks", DefaultPriority: 0.0},
		{ID: EventSystemScoringCompleted, Source: "system", Category: "scoring", Label: "Scoring Complete", Description: "AI scoring finished for tasks", DefaultPriority: 0.0},
		{ID: EventSystemDelegationCompleted, Source: "system", Category: "delegation", Label: "Delegation Complete", Description: "Agent delegation run completed", DefaultPriority: 0.0},
		{ID: EventSystemDelegationFailed, Source: "system", Category: "delegation", Label: "Delegation Failed", Description: "Agent delegation run failed", DefaultPriority: 0.0},
	}
}
