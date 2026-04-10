package domain

import "time"

// TrackedItem represents a GitHub PR or Jira issue we're actively monitoring for state changes.
type TrackedItem struct {
	Source       string     // "github" | "jira"
	SourceID     string     // "owner/repo#42" (GitHub) or "SKY-45" (Jira)
	TaskID       string     // FK to tasks table
	NodeID       string     // GitHub GraphQL node ID (empty for Jira)
	Snapshot     string     // JSON-serialized snapshot
	TrackedSince time.Time
	LastPolled   *time.Time
	TerminalAt   *time.Time // non-nil = merged/closed/done
}

// PRSnapshot is the extracted state we store for a GitHub pull request.
// Every field here can trigger events when it changes between poll cycles.
type PRSnapshot struct {
	// Identity
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Author    string `json:"author"`     // login of the PR author
	Repo      string `json:"repo"`       // "owner/repo"
	HeadRepo  string `json:"head_repo"`  // fork repo if different
	URL       string `json:"url"`

	// State
	State     string `json:"state"`      // OPEN, CLOSED, MERGED
	IsDraft   bool   `json:"is_draft"`
	Merged    bool   `json:"merged"`
	Mergeable string `json:"mergeable"`  // MERGEABLE, CONFLICTING, UNKNOWN

	// Branches
	HeadRef string `json:"head_ref"`
	BaseRef string `json:"base_ref"`
	HeadSHA string `json:"head_sha"`

	// Size
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`

	// CI
	CIState string `json:"ci_state"` // SUCCESS, FAILURE, PENDING, EXPECTED, ERROR, ""

	// Reviews
	ReviewRequests []string       `json:"review_requests"` // logins of users/teams with pending requests
	Reviews        []ReviewState  `json:"reviews"`         // latest review per reviewer
	ReviewCount    int            `json:"review_count"`    // total reviews submitted

	// Metadata
	Labels       []string `json:"labels"`
	CommentCount int      `json:"comment_count"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	MergedAt     string   `json:"merged_at,omitempty"`
	ClosedAt     string   `json:"closed_at,omitempty"`
}

// ReviewState captures one reviewer's latest review.
type ReviewState struct {
	Author      string `json:"author"`
	State       string `json:"state"`        // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	SubmittedAt string `json:"submitted_at"`
}

// JiraSnapshot is the extracted state we store for a Jira issue.
type JiraSnapshot struct {
	Key          string   `json:"key"`
	Summary      string   `json:"summary"`
	Status       string   `json:"status"`
	Assignee     string   `json:"assignee"`
	Priority     string   `json:"priority"`
	Labels       []string `json:"labels"`
	IssueType    string   `json:"issue_type"`
	ParentKey    string   `json:"parent_key"`
	CommentCount int      `json:"comment_count"`
	URL          string   `json:"url"`
}
