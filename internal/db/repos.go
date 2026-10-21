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
	// rename would move onto. FindOrCreateByExternalID refuses with it too,
	// for the first cause: the name it would register a repository under is
	// held by a row with a different provider id. Two causes:
	//
	//   - A repository row the rename cannot prove is the same repository: one
	//     with a different provider id, or one without an id that a team
	//     tracks or a worktree ledger row names. GitHub cannot serve two live
	//     repositories under one name, so an occupant with another id is a
	//     repository TF still holds under a name GitHub has given away. It is
	//     resolved by its own id (reporename.Resolve): renamed when GitHub
	//     answers for the id under another name, retired (RetireSystem) when it
	//     does not answer at all, and either one frees the name.
	//   - A durable record keyed on that name: an active entity
	//     ("owner/repo#18"). Entity keys spell the slug themselves, so a name
	//     can be spoken for by records alone. A retired row's entities are
	//     closed, and a closed entity holds no key. An artifact's dedup key
	//     names no repository, so no artifact speaks for a name.
	//
	// Neither is resolved by deleting the occupant. The first would drop a
	// repository row that holds a provider id, a team's tracking decision or a
	// run's record of a checkout; the second would destroy durable work — an
	// entity carries tasks, conversations and memory.
	//
	// The one occupant a rename does absorb is a bare row (no provider id, no
	// tracking, no ledger row) that a GitHub entity create minted under the
	// new name before the rename was observed. The provider has just said the
	// name belongs to the renamed row's id, so that row is the same repository
	// and is folded in rather than refused over; see RenameSystem.
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

	// ErrRepoHostRequired means a ref-keyed or listing method was called
	// without the GitHub host the repository lives on. A name and a provider
	// id are each unique only within one host, so a lookup without one names
	// no repository, and a write without one would key a row under a host no
	// read asks for. An org with no GitHub URL has no host and no GitHub
	// rows, so whether the caller forgot the host or is acting for such an
	// org, nothing is read or written.
	ErrRepoHostRequired = errors.New("repository host is required")

	// ErrInstallationHostRequired refuses an App installation written without
	// the GitHub host it lives on (GitHubAppsStore.UpsertInstallation).
	ErrInstallationHostRequired = errors.New("github app installation host is required")
)

// RequireRepoHost refuses an empty GitHub host with ErrRepoHostRequired. Both
// dialects call it at the door of every host-scoped method, so the refusal and
// its wording cannot differ between them.
func RequireRepoHost(host string) error {
	if host == "" {
		return ErrRepoHostRequired
	}
	return nil
}

// RepositoryStore owns the repositories table — the registry of repositories
// TF works with, each carrying the provider identity a rename does not move,
// the AI-generated profile cache, the clone-attempt state and the poller's
// conditional-request cursor.
//
// The registry is not the tracked set. It holds every repository TF records
// something in: a row is created when a caller registers a repository by its
// GitHub id (FindOrCreateByExternalID — what a team picks from the reachable
// list before it can track it) or when TF first records a GitHub entity in it
// (EntityStore's create, through the get-or-create FindOrCreateByRefSystem
// exposes as the door). A row survives the last team
// untracking it, because team_github_repos, conversation_worktrees and
// entities all reference it by id and a reference must not outlive what it
// names. So a row says nothing about whether any team works with the
// repository: an agent commenting on a pull request in a repository no team
// tracks gives that repository a row. ListTrackedNamesSystem is the read that
// means "what does TF poll", and ListTrackedSystem / ListTrackedByTeamSystem
// are the row-shaped reads of the same set for the consumers that need more
// than a name. List and ListSystem read the whole registry, untracked rows
// included.
//
// # Scoped by GitHub host
//
// A repository's identity is (org, source, host, owner/repo), and its provider
// id is unique per (org, source, host). host is db.EffectiveGitHubHost of the
// org's GitHub base URL — the scope GitHub entities are keyed under too. An org
// that moves to another host keeps its old rows; every ref-keyed and listing
// method takes the host it asks about, so those rows are invisible to the new
// host's reads, polls and renames, and come back if the org moves back. A
// same-named repository on the new host is another row. The id-keyed methods
// (Get, UpdateBaseBranch) take none: a row id names one repository on one host
// already. A missing host is ErrRepoHostRequired.
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
// UNIQUE(org_id, source, host, owner, repo) natural key, and that uuid is what
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
//     identifiers are case-insensitive) and pins ref.Source and ref.Host, so
//     a ref names one repository rather than merely matching one. A miss is
//     (nil, nil) or a no-op, because a name that resolves to nothing is an
//     answer.
//
// No method takes a bare "owner/repo" string. domain.RepoRefFromSlug is the
// edge parser for the surfaces that still speak one — HTTP path segments, an
// agent's argv — and each of those resolves once, at its own boundary, next
// to whatever access gate it already runs.
//
// # A retired row holds no name
//
// RetireSystem retires a row when GitHub has given its name to another
// repository and its own id no longer answers (domain.Repository.RetiredAt).
// The row keeps its id, its name and everything that references it, and from
// then on it answers only to its id: every ref-keyed method, every list and
// the tracked-name reads skip it, so the repository GitHub now serves under
// the name resolves to a row of its own, and nothing polls, profiles or clones
// the retired one. The identity index covers only rows that are not retired,
// which is what lets the two share a name. Get and GetSystem still return a
// retired row, and so does the team tracked-set read
// (TeamGitHubReposStore.ListForTeam): untracking is a team's decision, and a
// retire is not one.
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
// Exempt, each said so at the method: FillMissingExternalIDsSystem (bulk) and
// SetPullsPollStateByRefSystem (per-repo-per-cycle bookkeeping nobody reads
// back).
type RepositoryStore interface {
	// Upsert inserts or updates a repository row. On conflict it
	// refreshes profiling metadata (description, has_readme,
	// has_claude_md, has_agents_md, profile_text, clone_url,
	// default_branch, profiled_at) but PRESERVES user-configured
	// base_branch and clone-status fields — those are mutated by
	// dedicated methods and shouldn't be clobbered by a re-profile.
	//
	// p.Source normalizes through domain.NormalizeRepoSource (empty means
	// GitHub) and, with p.Host (required), is a create-time identity column:
	// the conflict target is (source, host, folded owner/repo), so a
	// same-named repository on another host is another row and never this
	// one. p.ExternalID refreshes the stored id when non-empty and leaves it
	// alone when empty — the profiler reads the id off the same
	// /repos/{owner}/{repo} response it takes default_branch and clone_url
	// from, and a caller with no id has learned nothing to write. An id
	// another row on the host already carries is not written either: that is
	// a rename the poller has not applied yet (or one it refused), and moving
	// the id onto this row would leave two rows claiming one repository.
	//
	// Returns the persisted row, which is the only way to see any of the
	// above: p is an input, and every rule in this doc is a rule about how
	// the row differs from it. The row also carries the id p never had — a
	// caller that upserts a repository it has not read holds no other handle
	// to it.
	Upsert(ctx context.Context, orgID string, p domain.Repository) (domain.Repository, error)

	// List returns one page of the org's configured repos on host plus the
	// unpaged total, including repos without profile text. Ordered by
	// (owner, repo) with an id tiebreaker, so the order is total and the pages
	// partition it. A zero ListOpts.Limit (db.Unwindowed) means "no window" —
	// the internal callers that need the whole registry to resolve a ref pass
	// that; a list route never does.
	List(ctx context.Context, orgID, host string, opts ListOpts) ([]domain.Repository, int, error)

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
	// Local mode (SQLite, N=1): returns the same set as ListTrackedSystem —
	// there is no other team to scope away, mirroring the local-mode
	// asymmetry of ListActiveJiraTeamScoped / FactoryReadStore.Entities. A
	// row no team tracks is left out in both dialects.
	//
	// Confined to host, like List.
	ListTeamScoped(ctx context.Context, orgID, host string, opts ListOpts) ([]domain.Repository, int, error)

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

	// ListSystem is the whole registry on host, rows no team tracks included.
	// A consumer asking which repositories TF works with reads
	// ListTrackedSystem instead.
	ListSystem(ctx context.Context, orgID, host string) ([]domain.Repository, error)

	// GetSystem is Get on the admin pool, for the claims-free callers that
	// hold a registry id: a workspace restore resolves the checkouts a
	// snapshot recorded by id, whatever host the org is on now.
	GetSystem(ctx context.Context, orgID, id string) (*domain.Repository, error)

	// ListTrackedNamesSystem returns the "owner/repo" of every repository on
	// host at least one team in the org tracks, ordered and deduplicated — the
	// set the GitHub poller enumerates, the profiler profiles, and the
	// dashboard backfill scopes a search to. Callers pass the org's current
	// host: a repository tracked on a host the org has since moved away from is
	// not asked about on the new one, where its name may belong to another
	// repository.
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
	ListTrackedNamesSystem(ctx context.Context, orgID, host string) ([]string, error)

	// ListTrackedSystem returns one page of the repository rows on host that
	// at least one team in the org tracks, plus the unpaged total — the
	// tracked set ListTrackedNamesSystem names, as rows. Ordered and paged
	// like List; db.Unwindowed returns every row.
	//
	// Its consumers mean "the repositories TF works with": the startup clone
	// warm reads each row's clone URL, the org admin's repository list renders
	// the rows, the setup gate counts them (ListOpts.CountOnly), and the
	// GitHub team lookups take their owners. A row every team has stopped
	// tracking is left out; it stays in the registry for the references that
	// still name it.
	//
	// Admin pool in Postgres, which is what lets "at least one team" reach
	// teams the caller is not on: the app pool's team_github_repos_select
	// policy narrows a team to the viewer's own, which is ListTeamScoped. A
	// request handler returns these rows only behind an org-admin gate. A
	// handler may read them for an org-level answer that returns no row: the
	// setup gate's count, and the owners whose GitHub teams it asks the org's
	// credential for.
	ListTrackedSystem(ctx context.Context, orgID, host string, opts ListOpts) ([]domain.Repository, int, error)

	// ListTrackedByTeamSystem returns one page of the repository rows on host
	// that teamID tracks, ordered by (owner, repo, id), plus the unpaged total.
	// It is the listing half of the gate TeamGitHubReposStore.TracksRepoSystem
	// applies: a delegated run is offered the repositories its team tracks,
	// which are the ones `workspace add` will materialize and the git proxy
	// will push to. Admin pool in Postgres, for callers with no JWT claims (a
	// run's agenthost) and for an org admin reading a team they are not on,
	// which the app pool's team_github_repos_select policy would hide.
	ListTrackedByTeamSystem(ctx context.Context, orgID, teamID, host string, opts ListOpts) ([]domain.Repository, int, error)

	// ListTrackedByTeam is ListTrackedByTeamSystem on the app pool. In
	// Postgres team_github_repos_select admits only the caller's own teams'
	// tracking rows, so for a team the caller is not on it returns nothing.
	// It is the read for a member acting on their team: the repos list's
	// team filter and an event handler's repo_in check, which only a team
	// member can write. SQLite (N=1) answers as the system read does.
	ListTrackedByTeam(ctx context.Context, orgID, teamID, host string, opts ListOpts) ([]domain.Repository, int, error)

	// FindOrCreateByRefSystem returns the row the provider calls
	// ref.Owner/ref.Repo on ref.Host, creating it when nothing answers to the
	// name: the registry's door, for a GitHub object TF records in a
	// repository it may hold no row for yet. It is the same get-or-create
	// EntityStore's create resolves a GitHub entity's repository through
	// inside its own transaction.
	//
	// An existing row is returned exactly as it stands: the lookup folds case
	// on owner/repo, so a ref spelling a registered repository differently
	// gets that row and its stored casing, and nothing cached on it moves. A
	// new row is bare — identity columns only, no provider id (the poller and
	// profiler record it), no profile — and it is not tracked: creating it
	// adds nothing to what TF polls, profiles, clones or lists. Two
	// concurrent creators of one repository resolve to one row; the folded
	// identity index is what refuses the second insert.
	//
	// The row returned is the one the call's transaction resolved, created or
	// found. It is read in that transaction rather than off RETURNING: the
	// insert is ON CONFLICT DO NOTHING, which returns nothing when the row
	// already exists or a concurrent creator won, and those are the common
	// cases. ref.Host is required (ErrRepoHostRequired) and ref.Source
	// normalizes like Upsert's; ref.ExternalID is ignored, since the row is
	// created bare.
	//
	// Admin pool in Postgres, and callable from an executor: the executor's
	// role may insert a repository's identity columns and nothing else, which
	// is what lets a run record a pull request in a repository no team tracks.
	FindOrCreateByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef) (domain.Repository, error)

	// FindOrCreateByExternalID registers the repository GitHub identifies as
	// ref.ExternalID on ref.Host, and returns its row: the door a repository
	// enters the registry through when a caller names it by GitHub id, which
	// is how a team picks one from the reachable list before tracking it.
	// ref.Owner/ref.Repo is the name the caller read alongside the id (the
	// reachable mirror's), used only when no row carries the id yet.
	//
	// The id is looked up before the name, so a rename GitHub has made and TF
	// has not applied yet resolves to the row that already carries the id,
	// under the name it stores, rather than to a second row under the new
	// name. Three outcomes:
	//
	//   - A row on the host carries the id: it is returned exactly as it
	//     stands. Its stored name is not moved here; RenameSystem applies a
	//     rename when the poller observes one.
	//   - No row carries the id and a row without a provider id holds the
	//     name (folded case): the id is written onto that row, which is
	//     returned. The provider says the name belongs to the id now, so the
	//     row is that repository — the same inference Upsert and
	//     FillMissingExternalIDsSystem make.
	//   - Nothing answers to either: a row is created carrying the id from
	//     the start. Bare otherwise — no profile — and not tracked.
	//
	// A row holding the name under a different provider id is refused with
	// ErrRepoSlugOccupied, and nothing is written: GitHub serves one live
	// repository per name, so that row is a repository TF still holds under a
	// name GitHub has given to another, and re-identifying it would move its
	// tracking, entities and worktrees onto a repository they never named.
	//
	// Two concurrent registrations of one repository resolve to one row; the
	// identity indexes refuse the second insert, and the loser returns the
	// winner's row. ref.Host and ref.ExternalID are required; ref.Source
	// normalizes like Upsert's.
	//
	// App pool in Postgres, under the caller's claims: it is a request
	// handler's write. repositories carries no team, so RLS admits any member
	// of the org; who may register is decided by the handler.
	FindOrCreateByExternalID(ctx context.Context, orgID string, ref domain.RepoRef) (domain.Repository, error)

	UpdateCloneStatusByRefSystem(ctx context.Context, orgID string, ref domain.RepoRef, status, errMsg, errKind string) (*domain.Repository, error)
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
	//
	// Each ref names its host, and a row is filled only on that host, and only
	// with an id no other row on the host carries — the same rule Upsert
	// follows, for the same reason.
	FillMissingExternalIDsSystem(ctx context.Context, orgID string, refs []domain.RepoRef) (int, error)

	// ListIdentitiesSystem returns the provider identity — source, host, slug,
	// and the provider's own id — of every repository row on host that carries
	// an id, retired rows included and marked. Rows without an id are omitted
	// rather than returned with an empty ExternalID: a repository TF has not
	// learned an id for is not renamable in either direction, so it is not
	// part of the comparison at all. A retired row is: GitHub answering for its
	// id again is what restores it.
	//
	// The read half of rename and reassignment detection. It is the small side
	// of the join: TF holds tens of repositories per org where a grant listing
	// may hold thousands, so the caller reads this once and matches the
	// provider's enumeration against it in memory (domain.DetectRepoRenames,
	// domain.DetectReassignedRepoNames) instead of asking the database once
	// per observed repository.
	ListIdentitiesSystem(ctx context.Context, orgID, host string) ([]domain.RepoIdentity, error)

	// RetireSystem retires the row stored under held's slug that carries
	// held's provider id, because GitHub has given the slug to another
	// repository and no longer answers for the id: the repository was deleted,
	// or the org's credential can no longer see it. The caller has asked
	// GitHub (reporename.Resolve); a retire is never inferred from a failed
	// request.
	//
	// In one transaction it sets retired_at and closes the row's active
	// entities, which frees both the name (the identity index skips retired
	// rows) and the keys the entities spell ("owner/repo#18", which the
	// repository now under the name has pull requests of its own under).
	// Nothing else moves: the row keeps its id, name and provider id, its
	// tracking rows, worktree ledger rows, events, artifacts and tasks. Its
	// artifacts are keyed on ids — a pull request's node id, a comment's id,
	// the row's own id — and reference its entities by id, so the repository
	// now under the name records its own under keys and entities of its own.
	//
	// The row is matched on (source, host, external_id) and re-checked under a
	// lock to still be stored under held's slug and not retired, so a candidate
	// that went stale — renamed since the caller read it, or retired by a
	// concurrent caller — is a no-op, returned as (nil, nil). Otherwise it
	// returns the row it retired. held.Host and held.ExternalID are required.
	//
	// System (claims-free) variant only, admin-pool in Postgres: a retire
	// closes entities every team in the org may have tasks on, which no
	// request-scoped identity can see all of.
	RetireSystem(ctx context.Context, orgID string, held domain.RepoRef) (*domain.Repository, error)

	// RenameSystem makes TF's stored slug for one repository match the slug
	// the provider currently reports for it, rewriting every stored reference
	// to the old slug in ONE transaction. observed carries the identity a
	// rename does not move (Source + Host + ExternalID) alongside the slug the
	// provider now uses.
	//
	// Everything it rewrites belongs to that host: the repository row is
	// matched on (source, host, external_id), entities are the row's own (by
	// repository_id, so a retired row that shares the old slug keeps its
	// history), and so are the artifacts: those about its entities
	// (artifacts.entity_id), which take their entity's new key, and its
	// branches, found by the registry row id their dedup keys carry. Their
	// targets move and their keys do not. Audit-ledger rows move only where
	// their scope is the host (with a link or without one), placement pins
	// only on the host, and a stored link only when it is on the host
	// (domain.RewriteRepoURL). A repository on another host with the same name
	// or id is never touched.
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
	//   - no repository row carries that (source, host, id) — nothing to
	//     rename.
	//   - the stored slug already matches, case-insensitively, on a row that
	//     is not retired — the second run of a rename that already applied,
	//     which is the steady state.
	//
	// A retired row carrying the id is restored: the provider has answered for
	// it, so it takes the slug it was observed under and clears retired_at, in
	// the same transaction, under the same occupant rules as a rename
	// (Restored, and Renamed too when the slug moved). Only its own entities
	// and artifacts move with it. The records matched by the slug alone —
	// placement pins, audit-ledger pointers — stay where they are, because the
	// name it was retired under is no longer its own, and whatever answers to
	// it now may be another repository's.
	//
	// The occupant of the target slug is a row that holds it: a retired row
	// under the same slug is history, not an occupant. One occupant is
	// absorbed rather than refused: a bare
	// row, with no provider id, no tracking row and no worktree ledger row,
	// which a GitHub entity create minted under the new name before TF
	// observed the rename (an agent touch through GitHub's redirect, or a pull
	// request read off GraphQL, which keys by the current name). The provider
	// has just reported that the name belongs to the renamed row's id, so the
	// bare row is the same repository: its entities are re-pointed to the
	// renamed row and it is deleted, in the rename's transaction.
	//
	// Two states are refused instead, because writing through either would
	// destroy a row rather than move one: ErrRepoSlugOccupied when anything
	// else already answers to the target slug — another repository row, or a
	// durable entity keyed on that name — and ErrRepoIdentityAmbiguous
	// when more than one row claims the id. Both are terminal for the attempt,
	// and a caller that retries them will fail identically until whatever
	// holds the name is gone: renamed or retired once it is resolved by its own
	// id (reporename does this for an occupant with one), or never, for a
	// durable record. A refusal rolls back the absorb too.
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
