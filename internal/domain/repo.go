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
// RepositoryStore's get-or-create. Owner/Repo is the slug the rest of the product
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

// RepoRefFromSlug is Slug's inverse: it splits "owner/repo" at the first
// slash. A string with no slash yields an empty Repo, which every caller reads
// as "not a repository reference" rather than as a repository named by half a
// slug.
func RepoRefFromSlug(slug string) RepoRef {
	owner, repo, _ := strings.Cut(slug, "/")
	return RepoRef{Owner: owner, Repo: repo}
}

// Repository is one row of the repository registry: the repositories TF works
// with, each carrying the provider identity that survives a rename, the
// tracked-set membership the reconcile writes, the clone state, the poller's
// conditional-request cursor, and the cached AI-generated profile the name
// used to promise on its own.
type Repository struct {
	// ID is the slug, "owner/repo", derived by the scanners rather than read
	// from a column — so it cannot drift from Owner/Repo, but it is a name
	// rather than a handle, and a name is a thing that stops resolving.
	//
	// TODO(TFAC-834): this should be the registry row's id, with the slug
	// rendered by a Slug() method for display and wire use only. The stored
	// references are already ids; the store's outward contract is the last
	// place a repository is passed around by a value GitHub can change, and
	// the lookups that accept one miss silently when it goes stale.
	ID    string
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
