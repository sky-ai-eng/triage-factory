package domain

import "time"

// Task represents an actionable item from GitHub or Jira.
type Task struct {
	ID                string
	Source            string // "github" | "jira"
	SourceID          string
	SourceURL         string
	Title             string
	Description       string
	Repo              string
	Author            string
	Labels            []string
	Severity          string
	DiffAdditions     int
	DiffDeletions     int
	FilesChanged      int
	CIStatus          string // "success" | "failure" | "pending" | ""
	RelevanceReason   string // "review_requested" | "authored" | "mentioned" | "assigned"
	SourceStatus      string // Original status in external system (e.g. Jira workflow status)
	ScoringStatus     string // "unscored" | "scoring" | "scored"
	CreatedAt         time.Time
	FetchedAt         time.Time
	Status            string
	PriorityScore     *float64
	AISummary         string
	PriorityReasoning string
	AgentConfidence   *float64
	SnoozeUntil       *time.Time
}

// TaskScoreUpdate holds the fields to update on a task after AI scoring.
type TaskScoreUpdate struct {
	ID                string
	PriorityScore     float64
	AgentConfidence   float64
	PriorityReasoning string
	Summary           string
}
