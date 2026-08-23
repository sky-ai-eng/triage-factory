package db

import (
	"context"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=RepositoryStore --output=./mocks --case=underscore --with-expecter

// Errors RenameSystem refuses a rewrite with. Both describe stored state a
// rename cannot be applied over without losing a row, so both are terminal for
// the attempt: the caller logs and moves on, and the next observation of the
// same rename fails the same way until the conflicting row is gone.
var (
	// ErrRepoSlugOccupied means something already answers to the slug the
	// rename would move onto. Two causes, one remedy:
	//
	//   - A different repository row — one with a different provider id. GitHub
	//     cannot serve two live repositories under one name, so the occupant is
	//     a repository TF still has a row for and GitHub no longer does.
	//   - A durable record keyed on that name: an entity ("owner/repo#18") or
	//     an artifact dedup key. Untracking a repository deletes its
	//     repositories row and deliberately keeps its entities, tasks and
	//     artifacts (tracking is forward-only), so a freed name can still be
	//     spoken for long after the repository row is gone.
	//
	// Neither is resolved by deleting the occupant. The first would drop a
	// repository row; the second would destroy durable work — an entity carries
	// tasks, conversations and memory, and an artifact is the audit record of a
	// real external object. Retiring stale untracked state is a user-initiated
	// action, never a side effect of a rename.
	ErrRepoSlugOccupied = errors.New("target repository slug is already taken")

	// ErrRepoIdentityAmbiguous means more than one repository row carries the
	// provider id the rename keys on, so "the repository being renamed" does
	// not name a single row. Nothing that ships can produce this — the id is
	// written from provider payloads, one repository at a time — which is why
	// it is refused rather than resolved by picking one.
	ErrRepoIdentityAmbiguous = errors.New("more than one repository row carries this provider id")

	// ErrNoSuchRepository means an id-keyed method was handed a registry id no
	// row answers to. Only the id-keyed methods return it, and that asymmetry
	// is the point of splitting them from the *ByRef* ones:
	//
	//   - An id is a handle. It is minted by this store, it is what every
	//     stored reference to a repository holds, and GitHub cannot change it.
	//     One that stops resolving is a broken invariant, so the caller is
	//     made to handle it rather than shown an empty answer that reads
	//     exactly like "this repository was never configured".
	//   - A name is not a handle. "Nothing answers to owner/repo" is a
	//     legitimate, expected answer at every edge that holds one — an
	//     agent's argv, a bundle's pinned list, a git remote — and those
	//     callers already render it. So the *ByRef* lookups keep returning
	//     (nil, nil) and say so in their own docs.
	ErrNoSuchRepository = errors.New("no repository with that id")
)

// RepositoryStore owns the repositories table — the registry of repositories
// TF works with, each carrying the provider identity a rename does not move,
// the AI-generated profile cache, the clone-attempt state and the poller's
// conditional-request cursor.
//
// The registry is not the tracked set. A row is created when a repository is
// first tracked and survives the last team untracking it, because
// team_github_repos and conversation_worktrees both
// reference this row by id and a reference must not outlive what it names.
// ListTrackedNamesSystem is the read that means "what does TF poll".
//
// All methods take orgID; local mode passes runmode.LocalDefaultOrgID.
// Postgres impl filters on org_id alongside the
// (org_id = current_org_id()) RLS policy as defense in depth; SQLite
// impl asserts orgID equals the local sentinel and otherwise ignores
// it (single-tenant by design).
//
// # A slug is an edge format; the row id is the handle
//
// Both dialects key the row on a uuid plus a folded
// UNIQUE(org_id, source, owner, repo) natural key, and that uuid is what
// domain.Repository.ID carries and what every table referencing a repository
// stores. It is the handle: a rename moves owner/repo and leaves it alone.
//
// So every method here is keyed one of two ways, and which one it is, is in
// its name:
//
//   - id-keyed (Get, UpdateBaseBranch) takes the registry
//     id. A miss is ErrNoSuchRepository, never a silent no-op — see that
//     error's doc for why the two halves differ.
//   - ref-keyed (…ByRef) takes a domain.RepoRef, the provider's own name
//     for the repository. Matching folds case on owner/repo (GitHub
//     identifiers are case-insensitive) and pins ref.Source, so a ref names
//     one repository rather than merely matching one. A miss is (nil, nil) or
//     a no-op, because a name that resolves to nothing is an answer.
//
// No method takes a bare "owner/repo" string. domain.RepoRefFromSlug is the
// edge parser for the surfaces that still speak one — HTTP path segments, an
// agent's argv — and each of those resolves once, at its own boundary, next
// to whatever access gate it already runs.
//
// # Every single-row write returns the row it persisted
//
// Upsert, UpdateBaseBranch and UpdateCloneStatusByRef all hand
// back the stored row, read off RETURNING on the write statement itself rather
// than from a follow-up SELECT. This store is the reason the rule exists:
//
//   - The writes deliberately do not persist what the caller passed. Upsert
//     COALESCEs external_id, preserves the user's base_branch and the clone
//     status, and keeps the stored casing of owner/repo. A caller's input
//     struct is designed to be wrong as a picture of the row, so anything
//     that renders, publishes or caches it after the write is holding a lie.
//   - RETURNING is atomic where a read-back races. The row that comes back is
//     the one the statement produced, not whatever answers the same key a
//     moment later.
//
// The RETURNING list and the scan function are the point read's, shared rather
// than restated, so the write shape cannot drift from the read shape as columns
// are added. Miss semantics are unchanged: an id-keyed write that matches
// nothing is ErrNoSuchRepository, a ref-keyed one is (nil, nil).
//
// A caller may ignore the returned row. A caller must never re-read to learn
// what the write already returned.
//
// Exempt, each said so at the method: SetConfigured (set reconciliation, no
// single row to return), FillMissingExternalIDsSystem (bulk), and
// SetPullsPollStateByRefSystem (per-repo-per-cycle bookkeeping nobody reads
// back). GetOrCreateSystem returns a row already, but by a re-read, because
// its INSERT is ON CONFLICT DO NOTHING and returns nothing when it loses.
type RepositoryStore interface {
	// Upsert inserts or updates a repository row. On conflict it
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
	//
	// Returns the persisted row, which is the only way to see any of the
	// above: p is an input, and every rule in this doc is a rule about how
	// the row differs from it. The row also carries the id p never had — a
	// caller that upserts a repository it has not read holds no other handle
	// to it.
	Upsert(ctx context.Context, orgID string, p domain.Repository) (domain.Repository, error)

	// List returns one page of the org's configured repos plus the unpaged
	// total, including repos without profile text. Ordered by (owner, repo)
	// with an id tiebreaker, so the order is total and the pages partition
	// it. A zero ListOpts.Limit (db.Unwindowed) means "no window" — the
	// internal callers that need the whole registry to resolve a ref pass
	// that; a list route never does.
	List(ctx context.Context, orgID string, opts ListOpts) ([]domain.Repository, int, error)

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
	ListTeamScoped(ctx context.Context, orgID string, opts ListOpts) ([]domain.Repository, int, error)

	// SetConfigured syncs the repositories table with the given
	// "owner/repo" list. New entries get skeleton rows (no profile
	// text); entries no longer in the list are deleted. Single
	// transaction so the table can't observe a partial mid-sync
	// state.
	//
	// Exempt from the returned-row rule: it reconciles a set, so there is no
	// single row a return value could name. What it wrote is read back through
	// ListConfiguredNames.
	SetConfigured(ctx context.Context, orgID string, repoNames []string) error

	// ListConfiguredNames returns just the "owner/repo" IDs of
	// every configured repo. Ordered.
	ListConfiguredNames(ctx context.Context, orgID string) ([]string, error)

	// CountConfigured returns the number of configured repos. Used
	// by the settings endpoint to short-circuit a "no repos
	// configured yet" UI state without paying the full SELECT cost.
	CountConfigured(ctx context.Context, orgID string) (int, error)

	// UpdateBaseBranch sets the user-configured base branch for the
	// repository with this registry id and returns the updated row. Empty
	// string stores SQL NULL → falls back to the detected default_branch at
	// use-site.
	//
	// ErrNoSuchRepository when no row carries the id, rather than the silent
	// zero-rows the slug-keyed predecessor produced. The caller is the PATCH
	// handler, which resolved the id from its path segment moments earlier
	// and would otherwise answer "updated" for a write that did nothing — and
	// which answers with the returned row, so the response is the resource
	// rather than a status stub or an echo of the request body.
	UpdateBaseBranch(ctx context.Context, orgID, id, baseBranch string) (domain.Repository, error)

	// Get returns the repository with this registry id, or
	// ErrNoSuchRepository. Its ref-keyed sibling is GetByRef.
	Get(ctx context.Context, orgID, id string) (*domain.Repository, error)

	// GetByRef returns the repository the provider currently calls
	// ref.Owner/ref.Repo, or (nil, nil) when nothing does. That nil is the
	// answer every edge holding a name already renders — "repo is not
	// configured in Triage Factory", "no repository row for pinned repo,
	// skipping", the universal protected-branch set — so it stays an answer
	// rather than becoming an error.
	//
	// Matching folds case on owner/repo and pins ref.Source (empty means
	// GitHub, via domain.NormalizeRepoSource); ref.ExternalID is ignored, as
	// this asks what a name resolves to now, not what an identity was called.
	GetByRef(ctx context.Context, orgID string, ref domain.RepoRef) (*domain.Repository, error)

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
	// Postgres role holds SELECT and UPDATE on repositories, not INSERT, so
	// a repository is brought into the table by the side that polls and
	// tracks, never by a running agent's pod.
	//
	// Concurrent creators of the same repository resolve to one row, whatever
	// casing each of them spelled it with. That is enforced by the create
	// itself and not by anything the caller has to hold — the unique index is
	// case-sensitive and so cannot see the case-differing race, which is
	// precisely why the impls serialize on the case-folded identity instead of
	// leaning on it.
	GetOrCreateSystem(ctx context.Context, orgID string, ref domain.RepoRef) (*domain.Repository, error)

	// UpdateCloneStatusByRef records the outcome of an EnsureBareClone
	// attempt for the named repo. status is "ok" | "failed" |
	// "pending"; errMsg and errKind are stored as TEXT (empty
	// string serializes to NULL) — kind is "ssh" when our SSH
	// preflight has confirmed the SSH side is the cause, "other"
	// when the failure is on the git/transport side, and empty for
	// status="ok".
	//
	// Ref-keyed because the caller is a clone callback that knows which
	// checkout it just attempted, not which row it belongs to. No-ops
	// silently when nothing answers to the name: clone hooks fire after repo
	// selection, so the row is normally there, and a repo dropped between the
	// hook firing and this UPDATE landing is a race whose loser has nothing
	// left to record against.
	//
	// Returns the stamped row, or nil for that no-op — the ref-keyed miss
	// shape, same as GetByRef. The caller is a websocket emit site keyed on
	// the registry id, and the row is where the id comes from; nil is exactly
	// the case with nothing for a client to merge into.
	UpdateCloneStatusByRef(ctx context.Context, orgID string, ref domain.RepoRef, status, errMsg, errKind string) (*domain.Repository, error)

	// --- Admin-pool variants (`...System`) ---
	//
	// Same pattern as EntityStore: routed through the admin
	// pool in Postgres for system-service callers that have no JWT
	// claims in scope. Consumers are the poller bootstrap (reads
	// every configured repo for every org at server boot) and the
	// startup clone-status writes (records EnsureBareClone outcomes
	// before any request can have arrived). Behavior is identical
	// to the non-System variants — same SQL, same return shape.

	ListSystem(ctx context.Context, orgID string) ([]domain.Repository, error)

	// ListTrackedNamesSystem returns the "owner/repo" of every repository at
	// least one team in the org tracks, ordered and deduplicated — the set the
	// GitHub poller enumerates, the profiler profiles, and the dashboard
	// backfill scopes a search to.
	//
	// It reads through team_github_repos rather than listing the registry,
	// because the two are no longer the same set. A registry row is durable: it
	// outlives the tracking decision that created it, so a worktree ledger
	// entry or a task can go on naming the repository after a team untracks it.
	// "Which repositories does TF work on" is a question about tracking, and
	// this is the read that asks tracking.
	//
	// Names, not ids, and deliberately so: every consumer spends the result at
	// an edge. The poller turns each into a /repos/{owner}/{repo} path and
	// case-folds it against the installation grant's own FullName strings; the
	// profiler fetches GitHub metadata under it; the dashboard backfill builds
	// a `repo:` search qualifier from it; the round-robin resume cursor stores
	// one. Handing back ids would make all four resolve straight back to the
	// name, and the ids would be dead weight in the poll loop's hot path.
	ListTrackedNamesSystem(ctx context.Context, orgID string) ([]string, error)
	UpdateCloneStatusByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef, status, errMsg, errKind string) (*domain.Repository, error)
	CountConfiguredSystem(ctx context.Context, orgID string) (int, error)
	GetSystem(ctx context.Context, orgID, id string) (*domain.Repository, error)
	GetByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef) (*domain.Repository, error)
	UpsertSystem(ctx context.Context, orgID string, p domain.Repository) (domain.Repository, error)

	// FillMissingExternalIDsSystem records the provider's repository id for
	// tracked repos that do not have one yet, and returns how many rows it
	// filled. Refs whose repository has no row, or whose row already carries
	// an id, are skipped — this only ever turns a NULL into a value.
	//
	// It exists because an id nobody records is an id nobody has: rename
	// detection keys on (source, external_id) and treats a NULL as "not
	// renamable", so a repository stays undetectable for as long as its id is
	// missing. Filling it only when a repository is profiled leaves that
	// window open for the length of the profile TTL.
	//
	// The caller is the poller, which already enumerates each installation's
	// repo grant every cycle to compute the tracked∩granted intersection —
	// that response carries the ids, so this costs no GitHub request. Matching
	// is case-insensitive, like every other slug lookup here. A ref with an
	// empty ExternalID is ignored rather than written: absent is not an id.
	//
	// Exempt from the returned-row rule: it writes a batch, and the count is
	// what its caller reports. Returning the rows would hand a poll cycle every
	// repository it filled, which nothing reads and the steady state makes
	// empty anyway.
	FillMissingExternalIDsSystem(ctx context.Context, orgID string, refs []domain.RepoRef) (int, error)

	// ListIdentitiesSystem returns the provider identity — source, slug, and
	// the provider's own id — of every repository row that carries an id.
	// Rows without one are omitted rather than returned with an empty
	// ExternalID: a repository TF has not learned an id for is not renamable
	// in either direction, so it is not part of the comparison at all.
	//
	// The read half of rename detection. It is the small side of the join:
	// TF holds tens of repositories per org where a grant listing may hold
	// thousands, so the caller reads this once and matches the provider's
	// enumeration against it in memory (domain.DetectRepoRenames) instead of
	// asking the database once per observed repository.
	ListIdentitiesSystem(ctx context.Context, orgID string) ([]domain.RepoRef, error)

	// RenameSystem makes TF's stored slug for one repository match the slug
	// the provider currently reports for it, rewriting every stored reference
	// to the old slug in ONE transaction. observed carries the identity a
	// rename does not move (Source + ExternalID) alongside the slug the
	// provider now uses.
	//
	// Detection happens here, not in the caller: the method re-reads the
	// repository row under a lock (SELECT … FOR UPDATE in Postgres; SQLite's
	// single writer makes it moot) and decides for itself, so a candidate that
	// went stale between a caller's read and this call is a no-op rather than
	// a wrong write. Two concurrent detections of the same rename therefore
	// cannot both rewrite: the loser re-reads the slug the winner already
	// wrote and returns Renamed=false. Losing is terminal and needs no retry.
	//
	// Every no-op is a nil error, never a sentinel:
	//
	//   - observed carries no ExternalID — nothing to key on.
	//   - no repository row carries that (source, id) — nothing to rename.
	//   - the stored slug already matches, case-insensitively — the second run
	//     of a rename that already applied, which is the steady state.
	//
	// Two states are refused instead, because writing through either would
	// destroy a row rather than move one: ErrRepoSlugOccupied when anything
	// already answers to the target slug — another repository row, or a durable
	// entity/artifact keyed on that name — and ErrRepoIdentityAmbiguous when
	// more than one row claims the id. Both are terminal for the attempt, and
	// a caller that retries them will fail identically until a human retires
	// whatever holds the name.
	//
	// System (claims-free) variant only, and admin-pool in Postgres: the
	// rewrite spans tables belonging to every team in the org (tracked sets,
	// projects, entities, artifacts), which no request-scoped identity can
	// see all of, and its callers are the poller and the profiler.
	RenameSystem(ctx context.Context, orgID string, observed domain.RepoRef) (domain.RepoRenameOutcome, error)

	// GetPullsPollStateByRefSystem returns the stored conditional-request
	// state for a repo's open-PR listing: the last ETag and the last
	// successful poll time. Both are zero values ("" / nil) when the repo has
	// never been listed, and likewise when nothing answers to the name.
	//
	// Ref-keyed, and the pair below with it, because the caller is the GitHub
	// tracker iterating ListTrackedNamesSystem: it holds the same name it is
	// about to put in the request path, and resolving each one to an id per
	// repo per cycle would buy nothing — the state is a cache of one HTTP
	// conditional request, and a name that stopped resolving mid-cycle costs
	// exactly one unconditional re-list.
	//
	// System (claims-free) variant — the poller goroutine has no JWT claims,
	// same convention as ListTrackedNamesSystem.
	GetPullsPollStateByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef) (etag string, polledAt *time.Time, err error)

	// SetPullsPollStateByRefSystem records the conditional-request state after
	// a successful open-PR list. On a 304 the caller passes the stored
	// etag back unchanged (only polledAt advances); on a 200 it passes
	// the fresh etag. No-ops silently when nothing answers to the name.
	//
	// Exempt from the returned-row rule: this fires once per tracked repo per
	// poll cycle to stamp a cache cursor the poller reads back through
	// GetPullsPollStateByRefSystem on the next cycle and nowhere else. Handing
	// a whole row to a write nobody reads the answer of is cost without a
	// reader.
	SetPullsPollStateByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef, etag string, polledAt time.Time) error
}
