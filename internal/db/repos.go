package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=RepoStore --output=./mocks --case=underscore --with-expecter

// RepoStore owns the repo_profiles table — the user-configured GitHub
// repos plus their AI-generated profile cache and clone-attempt state.
//
// All methods take orgID; local mode passes runmode.LocalDefaultOrgID.
// Postgres impl filters on org_id alongside the
// (org_id = current_org_id()) RLS policy as defense in depth; SQLite
// impl asserts orgID equals the local sentinel and otherwise ignores
// it (single-tenant by design).
//
// # repoID identity across backends
//
// The store API surfaces every repo by its natural "owner/repo"
// string as repoID — that's what callers pass and what
// domain.RepoProfile.ID returns. SQLite stores that string directly
// in the id column. Postgres uses a synthetic uuid id internally
// plus a UNIQUE(org_id, source, owner, repo) natural key; the
// Postgres impl splits the passed-in "owner/repo" and queries by
// (org_id, owner, repo) so callers never see the synthetic uuid. The
// synthetic id exists because the PG style is "every table has a uuid
// PK" — not because callers need it.
//
// source is absent from the slug-keyed methods deliberately: they
// match a repo without naming one, which is unambiguous while GitHub
// is the only provider that issues repositories. The methods that DO
// name one take a domain.RepoRef.
type RepoStore interface {
	// Upsert inserts or updates a repo profile. On conflict it
	// refreshes profiling metadata (description, has_readme,
	// has_claude_md, has_agents_md, profile_text, clone_url,
	// default_branch, profiled_at) but PRESERVES user-configured
	// base_branch and clone-status fields — those are mutated by
	// dedicated methods and shouldn't be clobbered by a re-profile.
	//
	// p.Source normalizes through domain.NormalizeRepoSource (empty means
	// GitHub) and is a create-time identity column: a conflicting write
	// leaves it alone. p.ExternalID refreshes the stored id when non-empty
	// and leaves it alone when empty — the profiler reads the id off the
	// same /repos/{owner}/{repo} response it takes default_branch and
	// clone_url from, and a caller with no id has learned nothing to write.
	Upsert(ctx context.Context, orgID string, p domain.RepoProfile) error

	// List returns every configured repo, including those without
	// profile text. Ordered by repoID for stable display.
	List(ctx context.Context, orgID string) ([]domain.RepoProfile, error)

	// ListTeamScoped is the non-admin discovery read (TFAC-559): it
	// returns only the configured repos tracked by at least one of the
	// caller's teams, semi-joined through team_github_repos. Mirrors the
	// read-scoping seam entities.ListActiveJiraTeamScoped and
	// FactoryReadStore.Entities use for the same org-wide-entity-list
	// leak class (CLAUDE.md's multi-mode read-scoping standing rule).
	//
	// Multi-mode (Postgres): under the app pool (tf_app, RLS active) the
	// team_github_repos_select policy already scopes the semi-join to the
	// viewer's own team memberships, so this auto-scopes with no explicit
	// team_id. A teamless caller gets zero rows. Callers gate org admins
	// around this method instead — an admin calls List (the org-wide
	// view), never this one.
	//
	// Local mode (SQLite, N=1): returns the same set as List — there is
	// no other team to scope away, mirroring the local-mode asymmetry of
	// ListActiveJiraTeamScoped / FactoryReadStore.Entities.
	ListTeamScoped(ctx context.Context, orgID string) ([]domain.RepoProfile, error)

	// ListWithContent returns only repos that have a non-empty
	// profile_text — the subset the curator + delegate context
	// loaders care about. Subset of List by predicate; safe to call
	// before the profiler completes (returns empty slice).
	ListWithContent(ctx context.Context, orgID string) ([]domain.RepoProfile, error)

	// SetConfigured syncs the repo_profiles table with the given
	// "owner/repo" list. New entries get skeleton rows (no profile
	// text); entries no longer in the list are deleted. Single
	// transaction so the table can't observe a partial mid-sync
	// state.
	SetConfigured(ctx context.Context, orgID string, repoNames []string) error

	// ListConfiguredNames returns just the "owner/repo" IDs of
	// every configured repo. Ordered.
	ListConfiguredNames(ctx context.Context, orgID string) ([]string, error)

	// CountConfigured returns the number of configured repos. Used
	// by the settings endpoint to short-circuit a "no repos
	// configured yet" UI state without paying the full SELECT cost.
	CountConfigured(ctx context.Context, orgID string) (int, error)

	// UpdateBaseBranch sets the user-configured base branch for a
	// repo. Empty string stores SQL NULL → falls back to the
	// detected default_branch at use-site. repoID is matched
	// case-insensitively on owner/repo (GitHub identifiers are) so a
	// caller whose casing differs from what's stored still finds the
	// row — TFAC-559's repoAccessAllowed handler gate matches
	// case-insensitively too, and the two must agree or a request
	// could pass the gate yet silently affect 0 rows here.
	UpdateBaseBranch(ctx context.Context, orgID, repoID, baseBranch string) error

	// SeedCloneURL sets clone_url for a configured repo ONLY when the
	// stored value is NULL/empty — the project-bundle import's
	// warm-cache seed (the URL was discovered during import preflight
	// via the importing org's own GitHub client). Never clobbers a URL
	// the profiler/clone hooks already wrote; no-ops silently when the
	// repo isn't configured. repoID is "owner/repo", matched
	// case-insensitively like UpdateBaseBranch. TFAC-109.
	SeedCloneURL(ctx context.Context, orgID, repoID, cloneURL string) error

	// Get returns a single repo profile by "owner/repo" id, or nil
	// if not configured.
	Get(ctx context.Context, orgID, repoID string) (*domain.RepoProfile, error)

	// GetOrCreateSystem returns the row for one repository, minting a bare
	// one if it does not exist yet. It is the single primitive that brings
	// a repository into the table: the tracked-set reconcile
	// (TeamGitHubReposStore.ReplaceForTeam) and SetConfigured create their
	// rows through the same INSERT, so a repository row can never exist
	// without the identity columns the create sets.
	//
	// Get-or-create, not upsert. An existing row is returned as it stands —
	// same id, same profile text, same base branch, same clone and poll
	// state — with exactly one exception: external_id takes ref's value
	// when ref carries one. That is the same rule Upsert applies, and it is
	// one rule rather than two: a non-empty id the caller just read off a
	// provider payload is what that provider currently says the slug
	// resolves to, and an empty one means the caller learned nothing, so it
	// never clears a stored id.
	//
	// The lookup is case-INSENSITIVE on owner/repo (GitHub identifiers are),
	// matching the reconcile: a caller spelling a tracked repo differently
	// gets the existing row rather than minting a second one for the same
	// repository. Stored casing is therefore sticky. ref.Source normalizes
	// through domain.NormalizeRepoSource — empty means GitHub, an unknown
	// value is an error rather than a row.
	//
	// System (claims-free) variant only: the callers that learn about a
	// repository are background jobs. Control-plane ones — the executor's
	// Postgres role holds SELECT and UPDATE on repo_profiles, not INSERT, so
	// a repository is brought into the table by the side that polls and
	// tracks, never by a running agent's pod.
	//
	// Concurrent creators of the same repository resolve to one row, whatever
	// casing each of them spelled it with. That is enforced by the create
	// itself and not by anything the caller has to hold — the unique index is
	// case-sensitive and so cannot see the case-differing race, which is
	// precisely why the impls serialize on the case-folded identity instead of
	// leaning on it.
	GetOrCreateSystem(ctx context.Context, orgID string, ref domain.RepoRef) (*domain.RepoProfile, error)

	// UpdateCloneStatus records the outcome of an EnsureBareClone
	// attempt for the given repo. status is "ok" | "failed" |
	// "pending"; errMsg and errKind are stored as TEXT (empty
	// string serializes to NULL) — kind is "ssh" when our SSH
	// preflight has confirmed the SSH side is the cause, "other"
	// when the failure is on the git/transport side, and empty for
	// status="ok".
	//
	// No-ops silently when the repo isn't in repo_profiles
	// (configured-repos-only invariant — clone hooks fire after
	// repo selection).
	UpdateCloneStatus(ctx context.Context, orgID, owner, repo, status, errMsg, errKind string) error

	// --- Admin-pool variants (`...System`) ---
	//
	// Same pattern as EntityStore: routed through the admin
	// pool in Postgres for system-service callers that have no JWT
	// claims in scope. Consumers are the poller bootstrap (reads
	// every configured repo for every org at server boot) and the
	// startup clone-status writes (records EnsureBareClone outcomes
	// before any request can have arrived). Behavior is identical
	// to the non-System variants — same SQL, same return shape.

	ListSystem(ctx context.Context, orgID string) ([]domain.RepoProfile, error)
	ListConfiguredNamesSystem(ctx context.Context, orgID string) ([]string, error)
	UpdateCloneStatusSystem(ctx context.Context, orgID, owner, repo, status, errMsg, errKind string) error
	CountConfiguredSystem(ctx context.Context, orgID string) (int, error)
	GetSystem(ctx context.Context, orgID, repoID string) (*domain.RepoProfile, error)
	UpsertSystem(ctx context.Context, orgID string, p domain.RepoProfile) error

	// GetPullsPollStateSystem returns the stored conditional-request
	// state for a repo's open-PR listing: the last ETag and the last
	// successful poll time. Both are zero values ("" / nil)
	// when the repo has never been listed. System (claims-free) variant
	// — the poller goroutine has no JWT claims, same convention as
	// ListConfiguredNamesSystem. No-ops to ("", nil, nil) when the repo
	// isn't in repo_profiles.
	GetPullsPollStateSystem(ctx context.Context, orgID, repoID string) (etag string, polledAt *time.Time, err error)

	// SetPullsPollStateSystem records the conditional-request state after
	// a successful open-PR list. On a 304 the caller passes the stored
	// etag back unchanged (only polledAt advances); on a 200 it passes
	// the fresh etag. No-ops silently when the repo isn't in
	// repo_profiles (configured-repos-only invariant). System variant.
	SetPullsPollStateSystem(ctx context.Context, orgID, repoID, etag string, polledAt time.Time) error
}
