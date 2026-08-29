package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=EntityStore --output=./mocks --case=underscore --with-expecter

// EntityStore owns the entities table — the long-lived "source
// object" (PR, Jira issue, Linear ticket, Slack message) that every
// event, task, and conversation hangs off. Lives from first poll until the
// poller observes the upstream as closed/merged.
//
// Audiences:
//
//   - Poller / tracker (internal/tracker) — FindOrCreate on every
//     poll cycle, then UpdateSnapshot / UpdateTitle / UpdateDescription
//     when the snapshot diff shows a drift, and MarkClosed / Close /
//     Reactivate on lifecycle transitions.
//   - Delegation memory + resume + materialize (internal/delegate) —
//     Get to fetch entity context attached to a task.
//   - AI scorer (internal/ai) — Descriptions to bulk-load the
//     flattened body text for prompt context without pulling
//     snapshot_json.
//   - Server panels (factory snapshot, dashboard) — Get / GetBySource /
//     ListActive for the entity views the UI renders.
//
// Wired against the app pool in Postgres (RLS-active): every
// consumer is request-equivalent or runs inside a server-side
// goroutine that already operates within the org's identity scope
// (tracker is started at server boot and is currently single-org;
// multi-mode org fan-out lands in a later milestone).
//
// SQLite has one connection; assertLocalOrg pins orgID to
// runmode.LocalDefaultOrgID so a confused caller fails loudly rather
// than silently reading a different tenant's row (which can't
// actually exist in local mode, but the assertion catches misuse
// during the multi-mode migration).
//
// Return convention: Get / GetBySource return (nil, nil) when no
// row matches — a missing entity is a normal read outcome, not an
// error. List* methods return an empty slice on no rows, never nil.
// Reactivate exposes a distinct success signal (boolean) because its callers
// need to know whether the row changed.
//
// # Every single-row write returns the row it persisted
//
// UpdateSnapshot, PatchSnapshot, UpdateTitle, UpdateDescription, UpdateURL,
// MarkClosed and Close — and each one's ...System twin — hand back the stored
// row, read off RETURNING on the write statement itself rather than from a
// follow-up SELECT, projecting the point read's column list and scanner so
// the write shape cannot drift from the read shape. Several of these stamp a
// column the caller never supplied (last_polled_at, closed_at), and the row
// is the only place those are visible.
//
// A miss is sql.ErrNoRows — this store's existing miss sentinel, which every
// id-keyed write's callers already branch on — rather than a new typed error.
// Close and CloseSystem are the exception in shape,
// not in rule: their guard makes "not active" a legitimate outcome, so they
// answer (nil, nil) the way GetBySource does.
//
// Exempt, each said so at the method: MarkPolledSystem (fire-and-forget
// bookkeeping), UpdateSnapshotCASSystem and Reactivate / ReactivateSystem and
// StampOwningTeamIfUnsetSystem (compare-and-swap guards whose bool already
// answers whether the write landed), and RekeyOrMergeSystem (a composition
// across tables that answers with the surviving id). FindOrCreate /
// FindOrCreateSystem already return the row.
type EntityStore interface {
	// --- Lookup ---

	// Get returns a single entity by ID, or (nil, nil) if no row
	// matches.
	Get(ctx context.Context, orgID, id string) (*domain.Entity, error)

	// GetBySource returns the entity for (source, source_id) — the
	// poller-side natural key — or (nil, nil) if not yet recorded.
	GetBySource(ctx context.Context, orgID, source, sourceID string) (*domain.Entity, error)

	// Descriptions returns the flattened description body for each
	// of the given entity IDs as a map keyed by ID. Empty
	// descriptions are omitted from the result map. Used by the
	// scorer to enrich TaskInput without pulling snapshot_json
	// through the call. Dedupes IDs and chunks the IN clause in
	// the SQLite impl to respect the variable-bind limit.
	Descriptions(ctx context.Context, orgID string, ids []string) (map[string]string, error)

	// ListActive returns every state='active' entity for the given
	// source ("github" / "jira"), ordered by last_polled_at ASC so
	// the freshest items rotate out of the head of the list each
	// poll cycle.
	ListActive(ctx context.Context, orgID, source string) ([]domain.Entity, error)

	// ListActiveJiraTeamScoped is the discovery-read variant of
	// ListActive(orgID, "jira") that backs the Jira stock / carry-over
	// swipe deck (internal/server/stock.go). It surfaces *untasked*
	// entities, so the task-membership semi-join can't scope
	// it — by definition the deck shows tickets before any task
	// exists. Instead it scopes by the Jira projects attached to the
	// viewer's teams.
	//
	// Multi-mode (Postgres): returns only active Jira entities whose
	// project key (the prefix of source_id, e.g. "PROJ" in "PROJ-123")
	// matches a jira_project_status_rules row — the table a team
	// populates to declare which Jira projects it tracks. That table's
	// RLS (jira_rules_select) is team-membership-scoped, so under the
	// app pool (tf_app) the semi-join auto-scopes to the projects
	// attached to the viewer's teams with no explicit team_id in the
	// query — the same RLS-does-the-scoping trick as the factory belt.
	// An org-wide poller produces Jira entities across every team's
	// projects; this keeps a user from swiping another team's
	// untriaged backlog.
	//
	// teamID is the optional per-page read filter (the carry-over deck's
	// team selector). Empty = the union of the viewer's teams' projects
	// (the RLS-scoped default above). When set, the deck narrows to the
	// projects THAT team tracks — without it, switching the deck to team
	// B would still surface team A's tickets (and a claim would then
	// stamp a task on B for a project B doesn't track). Postgres-only
	// effect; SQLite is N=1 and ignores it.
	//
	// Local mode (SQLite, N=1): returns the full active Jira set,
	// identical to ListActive(orgID, "jira"). There is one user and
	// one team, the Jira poller only discovers the configured
	// projects, and gating here would just hide the deck's own
	// contents — so the local impl applies no scoping, mirroring the
	// FactoryReadStore.Entities asymmetry.
	ListActiveJiraTeamScoped(ctx context.Context, orgID, teamID string) ([]domain.Entity, error)

	// --- Mutation ---

	// FindOrCreate is the poller's "is this row known?" entry
	// point: it returns the existing entity if (source, source_id)
	// already maps to one, and inserts a new row otherwise.
	// Returns (entity, created, error). Idempotent under a race:
	// concurrent first-discovery callers re-read on insert
	// failure so they each see a populated entity.
	FindOrCreate(ctx context.Context, orgID, source, sourceID, kind, title, url string) (*domain.Entity, bool, error)

	// UpdateSnapshot writes the new snapshot_json and stamps
	// last_polled_at. Called by the tracker after every successful
	// poll that found a row diff.
	//
	// Returns the updated row — carrying the last_polled_at it stamped — or
	// sql.ErrNoRows.
	UpdateSnapshot(ctx context.Context, orgID, id, snapshotJSON string) (domain.Entity, error)

	// PatchSnapshot writes the new snapshot_json **without** touching
	// last_polled_at — deliberately distinct from UpdateSnapshot. Used
	// by handlers that mutate an entity via an external API and want
	// the local copy to match the just-pushed state, but still expect
	// the next poll cycle to reconcile (so last_polled_at must stay
	// stale enough for the poll gate to refresh the row). Race
	// window: a concurrent in-flight poll can overwrite our patch
	// with its pre-mutation snapshot. Caller accepts that as the cost
	// of in-place patching.
	//
	// Returns the patched row, or sql.ErrNoRows.
	PatchSnapshot(ctx context.Context, orgID, id, snapshotJSON string) (domain.Entity, error)

	// UpdateTitle writes a new entity title (e.g. a PR title was
	// edited upstream). No snapshot or last_polled_at change.
	//
	// Returns the updated row, or sql.ErrNoRows.
	UpdateTitle(ctx context.Context, orgID, id, title string) (domain.Entity, error)

	// UpdateDescription writes the flattened issue/PR body. Stored
	// out of snapshot_json because it's large and not part of the
	// diff scope — keeping it off the snapshot keeps diff reads
	// small even for multi-KB bodies.
	//
	// Returns the updated row, or sql.ErrNoRows.
	UpdateDescription(ctx context.Context, orgID, id, description string) (domain.Entity, error)

	// MarkClosed unconditionally sets state='closed' and stamps
	// closed_at = now. Used at discovery time when the initial
	// snapshot is already terminal (merged PR / done issue) — the
	// entity was never active, so there are no tasks to cascade.
	//
	// Returns the closed row — carrying the closed_at it stamped — or
	// sql.ErrNoRows.
	MarkClosed(ctx context.Context, orgID, id string) (domain.Entity, error)

	// Close transitions an active entity to closed, but only when
	// state='active' — idempotent against double-fire from the
	// entity-lifecycle handler.
	//
	// Returns the closed row, or nil when the guard declined because the
	// entity was not active (including because no row carries the id). That
	// nil is the idempotency: a second close is not an error, and the caller
	// that wants to know whether this call was the one that closed it can now
	// tell, which a bare error never let it.
	Close(ctx context.Context, orgID, id string) (*domain.Entity, error)

	// Reactivate flips a closed entity back to active and clears
	// closed_at. Called when a previously-terminal entity reappears
	// as open (reopened PR, reopened Jira issue). Returns true
	// when the row actually changed.
	//
	// Exempt from the returned-row rule: a compare-and-swap guard whose bool
	// is already the write's own answer about whether it landed, which is
	// what every caller branches on. No caller renders the row.
	Reactivate(ctx context.Context, orgID, id string) (bool, error)

	// --- Admin-pool variants (`...System`) ---
	//
	// These mirror the per-method shape of the corresponding app-pool
	// methods but route through the admin pool (BYPASSRLS) in
	// Postgres. They exist for system services that need to operate
	// on every user's entities without impersonating any one of them
	// — the tracker, which writes entities for every polled repo
	// regardless of which user configured it. Same admin/app split
	// pattern as ConversationStore's.
	//
	// Behavior contract is identical to the non-System variants:
	// org_id is still filtered in every WHERE clause as defense in
	// depth, return shapes are identical. The only difference is
	// which Postgres pool the statement runs on; SQLite has one
	// connection and the two variants collapse.

	GetSystem(ctx context.Context, orgID, id string) (*domain.Entity, error)

	// GetBySourceSystem mirrors GetBySource on the admin pool — the
	// system-ingest-path variant a claims-free caller (the Slack ingest
	// pipeline's future engaged-thread lookup) needs to resolve an entity
	// by its natural key with no request JWT claims to route through the
	// app pool.
	GetBySourceSystem(ctx context.Context, orgID, source, sourceID string) (*domain.Entity, error)
	ListActiveSystem(ctx context.Context, orgID, source string) ([]domain.Entity, error)

	// ListActiveTerminalCandidatesSystem returns active entities whose
	// STORED snapshot already reads terminal — the divergence the routing
	// package's terminal-state reconciliation sweep repairs (a merged PR
	// whose entity row never flipped, so it is polled forever and its
	// tasks never clear). Normal operation returns nothing: the snapshot
	// and the state flip land together, so a row here means the close
	// write was lost.
	//
	// The two sources answer "is this snapshot terminal?" differently, so
	// the filter is exact for one and a superset for the other:
	//
	//   - github: exact. Terminality is self-contained in the snapshot
	//     (merged, or state CLOSED/MERGED), the same predicate the tracker
	//     applies at discovery.
	//   - jira: a superset. Terminality is per-project configuration —
	//     jira_project_status_rules' done statuses — so this filters on
	//     jiraDone, the caller's FLAT UNION across projects, and the caller
	//     re-checks each row against its own project's set. Union membership
	//     alone would over-close (a status that is done in one project but
	//     live in another), which is why the decision stays with the caller
	//     and this is only a narrowing read. An empty jiraDone excludes Jira
	//     entirely.
	//
	//     A ref matches on EITHER half: its id against the snapshot's
	//     status_id, or its name against the snapshot's status. Both arms are
	//     required because either side may be missing one — a rule seeded from
	//     the headless env vars carries no ids, and a snapshot captured before
	//     status ids were recorded carries none until its next poll refresh.
	//
	// Rows with no stored snapshot never match — an unseeded stub has said
	// nothing about whether its subject is finished. limit caps the batch
	// (<= 0 means no cap); ordering is created_at ASC, so a capped sweep
	// works through the oldest divergences first and reaches the rest on
	// later passes. System-only (admin pool): the sweep is a background
	// job with no JWT claims, and the invariant it enforces is org-wide.
	// org_id stays in the WHERE clause as defense in depth.
	ListActiveTerminalCandidatesSystem(ctx context.Context, orgID string, jiraDone []domain.JiraStatusRef, limit int) ([]domain.Entity, error)

	FindOrCreateSystem(ctx context.Context, orgID, source, sourceID, kind, title, url string) (*domain.Entity, bool, error)

	// UpdateSnapshotCASSystem writes the tracker snapshot under a
	// compare-and-swap on poll_seq (TFAC-579): the write only lands when
	// the row's current poll_seq still equals expectedPollSeq (the value
	// the caller last read the entity with), and it bumps poll_seq by 1
	// on success. Leadership (P1) is the primary guarantee that only one
	// process polls a given (org, source) at a time; this CAS is the
	// belt-and-suspenders — a straggler ex-leader's late write (stale
	// expectedPollSeq, e.g. mid-poll during a lease handoff) becomes a
	// harmless ok=false no-op instead of silently overwriting a newer
	// snapshot and losing a transition. Returns ok=false (no error) on a
	// CAS miss; the caller drops the write AND suppresses the transitions
	// it diffed (the snapshot-diff is the sole re-emit prevention — see
	// the tracker) and lets the next poll cycle reconcile. There is no
	// blind-write variant: every snapshot write goes through the CAS.
	//
	// Exempt from the returned-row rule: a compare-and-swap guard. ok is the
	// write's own answer about whether it landed, and it is the whole point of
	// the method — the caller suppresses its diffed transitions on a miss and
	// re-reads the row on the next cycle either way.
	UpdateSnapshotCASSystem(ctx context.Context, orgID, id, snapshotJSON string, expectedPollSeq int64) (ok bool, err error)

	// ClearSnapshotsForSourceSystem blanks snapshot_json for every ACTIVE
	// entity of one source in one org, bumping poll_seq on each row, and
	// returns how many it cleared.
	//
	// It exists for the event-source pause. The tracker's snapshot-diff is the
	// sole re-emit guard, so a source paused for a sprint would otherwise leave
	// every known entity holding a sprint-old snapshot and the first cycle
	// after re-enabling would diff stale->now: every CI result, every review,
	// every status move, all at once, minting tasks for a period the org had
	// deliberately turned off. Cleared, those rows take the tracker's existing
	// quiet-seed path instead — the same one a snapshot-less stub takes — which
	// writes the fresh snapshot and emits nothing, terminal states included.
	// Pausing and re-enabling is then the same shape as untracking and
	// re-tracking, which is what the forward-only rule already specifies.
	//
	// ACTIVE only, deliberately: a closed entity is never diffed again, so
	// clearing it would buy nothing and would blank the snapshot the personal
	// dashboard renders its merged PRs from.
	//
	// The poll_seq bump is what makes this safe against a cycle already in
	// flight: that cycle diffed against the snapshot being cleared, and its
	// write is a CAS on the poll_seq it read, so it now misses and drops its
	// transitions rather than restoring a stale snapshot behind the clear.
	//
	// Exempt from the returned-row rule: a bulk write. The count is for the
	// caller's log line; no row is read back.
	ClearSnapshotsForSourceSystem(ctx context.Context, orgID, source string) (int, error)

	// MarkPolledSystem stamps last_polled_at without touching the
	// snapshot or poll_seq — the row was read from the source, but
	// nothing about it was diffed. Distinct from UpdateSnapshot* (which
	// carries a snapshot) and from PatchSnapshot (which deliberately
	// leaves last_polled_at stale so the next cycle still refreshes).
	//
	// It exists for reads that confirm an entity's state outside the
	// snapshot-diff, where the column's age is itself an input: the
	// tracker's Jira gone-confirmation selects candidates by how long
	// last_polled_at has been stale, so an entity it successfully
	// confirms must advance or it stays a candidate forever — and
	// since candidates are ordered oldest-first against a per-cycle
	// budget, one such entity would consume that budget every cycle and
	// starve every other candidate behind it.
	//
	// Exempt from the returned-row rule: fire-and-forget bookkeeping. It
	// stamps a wall-clock column the next cycle's candidate query reads and
	// nothing else, and no caller looks at the answer.
	MarkPolledSystem(ctx context.Context, orgID, id string) error

	// RekeyOrMergeSystem follows an external object's changed natural key.
	// When newSourceID is free, the existing entity is re-keyed in place.
	// When that key already belongs to another entity, that canonically-keyed row survives
	// and every entity-id referent is moved to it. The operation is atomic.
	// Returns the surviving entity id and whether a merge occurred.
	//
	// Exempt from the returned-row rule: it is a composition, not a single-row
	// write — the merge arm moves every referent across tables and deletes the
	// loser, so which row survived is the outcome and that is what it returns.
	RekeyOrMergeSystem(ctx context.Context, orgID, id, newSourceID string) (survivorID string, merged bool, err error)

	UpdateTitleSystem(ctx context.Context, orgID, id, title string) (domain.Entity, error)
	UpdateDescriptionSystem(ctx context.Context, orgID, id, description string) (domain.Entity, error)

	// UpdateURLSystem sets the entity's url. Admin pool; no-op update
	// semantics (row must exist). Added for detached external-link
	// enrichment (first consumer: Slack thread permalinks — TFAC-595) —
	// entities.url is otherwise written only at insert (FindOrCreate*),
	// with no prior update path. System-only by design: the only caller,
	// ee/slack's permalink resolver, runs detached from any request
	// (best-effort, off context.Background()) with no JWT claims context.
	//
	// Returns the updated row, or sql.ErrNoRows — the same miss the rest of
	// this store's id-keyed writes report. The caller resolved the entity
	// moments earlier, so a miss means the row went away underneath it, which
	// is worth its best-effort log line rather than a silent success.
	UpdateURLSystem(ctx context.Context, orgID, id, url string) (domain.Entity, error)
	MarkClosedSystem(ctx context.Context, orgID, id string) (domain.Entity, error)
	CloseSystem(ctx context.Context, orgID, id string) (*domain.Entity, error)
	ReactivateSystem(ctx context.Context, orgID, id string) (bool, error)

	// DescriptionsSystem mirrors Descriptions for the AI scorer —
	// a background/system service that bulk-loads entity body text
	// to enrich task prompt context. The scorer has no JWT-claims
	// context (it runs in a singleton goroutine triggered by event-
	// bus sentinels), so the system variant routes through the
	// admin pool. Failing this read under app-pool RLS would degrade
	// every scored task to title-only context, materially changing
	// prioritization; the System variant prevents that.
	DescriptionsSystem(ctx context.Context, orgID string, ids []string) (map[string]string, error)

	// OwningTeamForEntitySystem resolves an entity's *structural* owning
	// team for author-centric task routing: entities.owning_team_id, an
	// explicit override set by the transfer op / the TF-origin PR stamp.
	//
	// Returns "" when it is unset so the router falls through to its
	// prior-task and author-identity tiers. Admin-pool (BYPASSRLS): the
	// router resolves ownership on the eventbus goroutine with no JWT claims;
	// org_id stays in the WHERE clause as defense in depth.
	OwningTeamForEntitySystem(ctx context.Context, orgID, entityID string) (string, error)

	// StampOwningTeamIfUnsetSystem writes entities.owning_team_id, but only
	// onto a row that does not have one yet — the write half of tier 1 above.
	// Returns stamped=false (no error) when the row already carries an owner,
	// when teamID is empty, and when no such entity exists.
	//
	// Stamp-if-NULL is the whole contract, and it is what lets two unordered
	// writers converge on one answer. A PR the bot opens has no TF author to
	// resolve, so the commissioning team is recorded from the conversation
	// that opened it; that write races the poller, which mints the same entity by natural
	// key whenever it next sees the PR. Either order lands the same owner —
	// the conversation's write back-fills a row the poller already created, or
	// it creates the row the poller later enriches — and neither can overwrite an
	// owner some *other* writer chose, because the only value this can replace
	// is NULL. An explicit owner (an operator's transfer) is therefore
	// permanent as far as this path is concerned, and re-delivery of the same
	// PR-open write is a no-op rather than a second opinion.
	//
	// Admin-pool (BYPASSRLS): the caller is the exec recording funnel, which
	// runs on an executor with no JWT claims. There is no app-pool variant
	// because ownership is never stamped from a request. Postgres keeps org_id
	// in the WHERE clause as defense in depth, since BYPASSRLS means nothing
	// else is scoping the statement; SQLite scopes at the assertLocalOrg gate
	// like every other method in that store, where a WHERE predicate would
	// match every row by construction and assert nothing.
	StampOwningTeamIfUnsetSystem(ctx context.Context, orgID, entityID, teamID string) (stamped bool, err error)

	// StampCommissionedByIfUnsetSystem writes entities.commissioned_by_user_id
	// under the identical contract, for the other half of the same moment: the
	// human who ASKED for the run that opened this pull request (the
	// conversation's creator). Returns stamped=false (no error) when the row
	// already names one, when userID is empty — an event-triggered run, where
	// nobody asked — and when no such entity exists.
	//
	// It is provenance, not ownership, and the two columns answer to different
	// readers: routing reads the owning team, the personal pull-request list
	// reads this. They are written side by side because the funnel is the one
	// place both values are in hand, not because either implies the other.
	//
	// Admin-pool, and stamp-if-NULL for the same convergence reason as its
	// neighbour: the funnel races the poller's mint of the same entity, first
	// commit wins, and re-delivery of the same PR-open write is a no-op.
	StampCommissionedByIfUnsetSystem(ctx context.Context, orgID, entityID, userID string) (stamped bool, err error)
}
