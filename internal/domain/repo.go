package domain

import "time"

// RepoProfile is a cached AI-generated profile for a GitHub repository.
type RepoProfile struct {
	ID          string // "owner/repo"
	Owner       string
	Repo        string
	Description string
	HasReadme   bool
	HasClaudeMd bool
	HasAgentsMd bool
	ProfileText string
	ProfiledAt  *time.Time
	UpdatedAt   time.Time
}
