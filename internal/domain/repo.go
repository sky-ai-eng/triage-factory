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
	// ID is the registry row's own id — the handle every reference to this
	// repository is stored as, and the one value about it GitHub cannot
	// change. A caller that holds it holds something that cannot stop
	// resolving without an FK violation somewhere; a caller that holds the
	// display name has to say so (RepositoryStore's *ByRef* lookups) and gets
	// a nil row when the name no longer resolves.
	//
	// Zero on a Repository the caller built to hand to Upsert: the write is
	// keyed on the identity columns below, so nothing reads it there.
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
	CloneURL       string // chosen clone URL (HTTPS or SSH form, per OrgSettings.GitHubCloneProtocol)
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

// Slug renders the repository's display name, "owner/repo" — the form the HTTP
// payloads, the agent's argv, the websocket envelopes and the GitHub API paths
// all use. A method rather than a field so there is exactly one renderer and no
// second copy to fall out of step with the columns it is built from.
func (r Repository) Slug() string { return r.Owner + "/" + r.Repo }

// Ref is the repository's provider identity as a RepoRef — what to hand a
// *ByRef* store lookup when all you have is a row you already read.
func (r Repository) Ref() RepoRef {
	return RepoRef{Source: r.Source, Owner: r.Owner, Repo: r.Repo, ExternalID: r.ExternalID}
}

// SplitGitHubEntitySourceID splits a GitHub entity's source id into its parts:
// "owner/repo#42" for a pull request or issue, "owner/repo" for a repository.
// prNumber is 0 when the id names no number, and an unparseable id yields
// empty parts rather than an error — every caller gates on a value it can
// check, and a malformed id is the same answer as a missing one.
//
// The "#N" suffix is cut BEFORE the owner/repo split, which is the reason this
// is one helper rather than a split spelled out per call site: splitting the
// raw id leaves the repository named "repo#42", which no repository row and no
// remote resolves, and the resulting failure surfaces far downstream of the
// parse that caused it. It lives here because the parse decides an
// authorization input on two separate gates (the git proxy's and exec-gh's),
// and a per-package copy is how those two would come to disagree about which
// repo a task is on.
func SplitGitHubEntitySourceID(sourceID string) (owner, repo string, prNumber int) {
	repoStr := sourceID
	if idx := strings.LastIndex(sourceID, "#"); idx >= 0 {
		repoStr = sourceID[:idx]
		fmt.Sscanf(sourceID[idx+1:], "%d", &prNumber)
	}
	owner, repo, ok := strings.Cut(repoStr, "/")
	if !ok {
		return "", "", prNumber
	}
	return owner, repo, prNumber
}

// GitHubTaskRepo is the "owner/repo" a GitHub task targets, or "" for a task
// of any other source.
//
// The source gate is load-bearing beyond politeness: a Slack entity's source
// id ("<channel>/<timestamp>") splits on "/" perfectly well, so a caller that
// skipped the check would read a nonexistent repo out of a Slack task and
// authorize against it.
func GitHubTaskRepo(task Task) string {
	if task.EntitySource != "github" {
		return ""
	}
	owner, repo, _ := SplitGitHubEntitySourceID(task.EntitySourceID)
	if owner == "" || repo == "" {
		return ""
	}
	return owner + "/" + repo
}
