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
	CloneURL      string // HTTPS clone URL (detected during profiling)
	DefaultBranch string // repo's default branch (detected during profiling)
	BaseBranch    string // user-configured branch to base feature work on (empty = use default)
	ProfiledAt    *time.Time
	UpdatedAt   time.Time
}
