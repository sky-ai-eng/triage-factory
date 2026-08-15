package domain

import (
	"fmt"
	"strings"
	"time"
)

// RepoSourceGitHub is the only provider that issues repositories today. The
// value set is validated here rather than by a CHECK constraint (see
// NormalizeRepoSource) so adding a provider costs a constant, not a SQLite
// table rebuild.
const RepoSourceGitHub = "github"

// NormalizeRepoSource canonicalizes a repository's source column. An empty
// source means the caller didn't say, and every caller that doesn't say means
// GitHub — the only provider TF reads repositories from. Anything else is
// refused rather than stored: the column is app-validated, so this function is
// the validation, and a typo that reached the row would key a repository under
// a provider nothing resolves.
func NormalizeRepoSource(source string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(source))
	if s == "" {
		return RepoSourceGitHub, nil
	}
	if s != RepoSourceGitHub {
		return "", fmt.Errorf("unknown repo source %q", source)
	}
	return s, nil
}

// RepoRef names one repository by provider identity — the argument shape of
// RepoStore's get-or-create. Owner/Repo is the slug the rest of the product
// carries; ExternalID is the provider's own id for that repository, which is
// what survives a rename, and is empty whenever the caller has no id to hand
// (nothing fetches one just to fill it in).
type RepoRef struct {
	Source     string // "" → RepoSourceGitHub
	Owner      string
	Repo       string
	ExternalID string // "" = unknown
}

// Slug renders the reference as the "owner/repo" string every slug-keyed
// column and store method uses.
func (r RepoRef) Slug() string { return r.Owner + "/" + r.Repo }

// RepoProfile is a cached AI-generated profile for a GitHub repository.
type RepoProfile struct {
	ID    string // "owner/repo"
	Owner string
	Repo  string

	// Source is the provider that issued this repository (RepoSourceGitHub
	// today); empty on a struct the caller built without one, which the
	// stores normalize on write. ExternalID is that provider's own id for
	// the repository — the identity a rename or transfer does not move —
	// and is empty when TF has not learned it yet, which is a supported
	// state everywhere rather than a gap to backfill.
	Source     string
	ExternalID string

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
	// open-PR discovery. PullsETag is the last ETag GitHub returned for
	// the repo's GET /pulls?state=open listing; PullsPolledAt
	// is the last successful list (200 or 304). Populated/consumed only by
	// the dedicated *PullsPollState* store methods — the general List/Get
	// projections leave them zero-valued.
	PullsETag     string
	PullsPolledAt *time.Time
}
