package domain

import "time"

// RepoProfile is a cached AI-generated profile for a GitHub repository.
type RepoProfile struct {
	ID             string // "owner/repo"
	Owner          string
	Repo           string
	Description    string
	HasReadme      bool
	HasClaudeMd    bool
	HasAgentsMd    bool
	ProfileText    string
	CloneURL       string // chosen clone URL (HTTPS or SSH form, per GitHubConfig.CloneProtocol)
	DefaultBranch  string // repo's default branch (detected during profiling)
	BaseBranch     string // user-configured branch to base feature work on (empty = use default)
	ProfiledAt     *time.Time
	UpdatedAt      time.Time
	CloneStatus    string // "ok" | "failed" | "pending"
	CloneError     string // raw stderr / preflight output captured at failure time
	CloneErrorKind string // "ssh" | "other" | ""

	// PullsETag / PullsPolledAt back the GitHub poller's conditional
	// open-PR discovery (SKY-353). PullsETag is the last ETag GitHub
	// returned for the repo's GET /pulls?state=open listing; PullsPolledAt
	// is the last successful list (200 or 304). Populated/consumed only by
	// the dedicated *PullsPollState* store methods — the general List/Get
	// projections leave them zero-valued.
	PullsETag     string
	PullsPolledAt *time.Time
}
