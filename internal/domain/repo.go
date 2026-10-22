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
// RepositoryStore's ref-keyed methods. Host is the GitHub deployment the
// repository lives on (GitHubHost of the org's base URL), and every ref-keyed
// store method requires it: a name and a provider id are each unique only
// within one host, so a ref without one names no repository. Owner/Repo is the
// slug the rest of the product carries; ExternalID is the provider's own id for
// that repository, which is what survives a rename, and is empty whenever the
// caller has no id to hand (nothing fetches one just to fill it in).
// OwnerExternalID is the provider's own id for the account that owns the
// repository (GitHub's numeric account id), empty under the same terms; it
// describes the repository ExternalID names, so a ref without ExternalID has
// nothing to attach it to.
type RepoRef struct {
	Source          string // "" → RepoSourceGitHub
	Host            string
	Owner           string
	Repo            string
	ExternalID      string // "" = unknown
	OwnerExternalID string // "" = unknown
}

// Slug renders the reference as the "owner/repo" string every slug-keyed
// column and store method uses.
func (r RepoRef) Slug() string { return r.Owner + "/" + r.Repo }

// RepoRefFromSlug is Slug's inverse: it splits "owner/repo" at the first
// slash, on host. A string with no slash yields an empty Repo, which every
// caller reads as "not a repository reference" rather than as a repository
// named by half a slug.
func RepoRefFromSlug(host, slug string) RepoRef {
	owner, repo, _ := strings.Cut(slug, "/")
	return RepoRef{Host: host, Owner: owner, Repo: repo}
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
	// stores normalize on write. Host is the GitHub deployment it lives on
	// (GitHubHost), a create-time identity column like Source: an org that
	// moves to another host meets other repositories under the same names,
	// so this row stays where it is and the new host's repository is another
	// row. ExternalID is that provider's own id for the repository on that
	// host — the identity a rename or transfer does not move — and is empty
	// when TF has not learned it yet, which is a supported state everywhere
	// rather than a gap to backfill.
	Source     string
	Host       string
	ExternalID string

	// OwnerExternalID is the provider's own id for the account that owns the
	// repository: GitHub's numeric account id, which an account rename does
	// not move, while Owner, the login, stays stale until TF applies the
	// rename. It is what matches the repository to the App installation on
	// its owner account (org_github_app_installations.account_id); the login
	// stands in only while either side has no id. Empty when TF has not
	// learned it, a supported state like an empty ExternalID. It is written
	// only onto a row that carries the repository id of the response it was
	// read from.
	OwnerExternalID string

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

	// RetiredAt is when TF retired the row: GitHub gave its name to another
	// repository and the row's own id no longer answers, because the
	// repository was deleted or the org's credential can no longer see it. A
	// retired row keeps its id, name, provider id and everything that
	// references it, and holds no name: reads by name and the lists skip it,
	// and the repository GitHub now serves under the name gets a row of its
	// own. Reads by id still return it. nil on a row that holds its name.
	RetiredAt *time.Time

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
	return RepoRef{
		Source: r.Source, Host: r.Host, Owner: r.Owner, Repo: r.Repo,
		ExternalID: r.ExternalID, OwnerExternalID: r.OwnerExternalID,
	}
}

// SplitGitHubEntitySourceID splits a GitHub entity's key into its parts:
// "owner/repo#42" for a pull request or issue, "owner/repo" for a repository.
// prNumber is 0 when the key names no number, and an unparseable key yields
// empty parts rather than an error — every caller gates on a value it can
// check, and a malformed key is the same answer as a missing one.
//
// It is the boundary where a GitHub entity's repository enters TF: the
// entity's create resolves the owner/repo the key names to a registry row
// once (db.GitHubEntityRepoRef) and stores the row's id in
// entities.repository_id. A rename leaves the name in a key stale until the
// key is rewritten, so nothing past the create reads a repository out of a
// key; GitHubPRNumber reads the part a rename does not change.
//
// The "#N" suffix is cut BEFORE the owner/repo split: splitting the raw key
// leaves the repository named "repo#42", which no repository row resolves.
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

// GitHubPRNumber is the number a GitHub entity key ends in ("owner/repo#42" is
// 42), or 0 when it ends in none. The number is the pull request's identity
// within its repository and no rename changes it, so a reader takes the
// repository from entities.repository_id and only the number from the key.
func GitHubPRNumber(sourceID string) int {
	_, _, n := SplitGitHubEntitySourceID(sourceID)
	return n
}
