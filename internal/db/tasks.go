package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=TaskStore --output=./mocks --case=underscore --with-expecter

// AgentClaimStamp is the guarded task-claim write that rides *inside*
// another store method's transaction — the bot taking responsibility for a
// task, written in the same durable step as the engagement that commits it
// (a fenced run insert, a queued firing, an event folded into a live run).
//
// It is a parameter type rather than a call of its own because the two
// writes must not be separable. A commitment that lands while its stamp
// silently fails leaves the board showing a free task under a live agent
// run, and the firing fences (the (triggering_event_id, trigger_id) replay
// fence, the pending-unique) mean a replay of the event skips before it
// could ever re-stamp — so nothing repairs it. Coupling them is what makes
// "unclaimed with a live run" mean only what the requeue path intends.
//
// A zero value (empty AgentID) means "no stamp": the commitment writes
// alone. That is the honest degrade for the seam between DB init and agent
// bootstrap where no org agent resolves, and for the drain path, whose
// claim was already stamped by the enqueue that queued the firing (and
// whose task the drain re-validates as still bot-claimed before firing).
//
// ActingTeamID is the team whose trigger fired. On a successful stamp it
// becomes the task's owning team, consolidating the card; empty leaves
// team_id unchanged. It is meaningful ONLY alongside a non-empty AgentID —
// it names a consolidation that happens *as part of* the claim, so a
// team-without-agent value describes a write that cannot occur. Producers
// normalize to the zero value rather than carrying one; impls skip on
// AgentID alone, so a partial value is inert but dishonest (it survives
// serialization on the cross-pod inject path).
//
// Semantics are StampAgentClaimIfUnclaimed's exactly —
// including its three-way refusal (a user claim wins, a same-agent rewrite
// is a no-op, a terminal task refuses) — and a refusal never fails the
// surrounding commitment: the claim race has a winner either way, and the
// run insert (or firing row) must still stand.
type AgentClaimStamp struct {
	AgentID      string
	ActingTeamID string
}

// TaskStore owns the tasks table — lifecycle, claims, dedup,
// swipe-triggered transitions, plus the run-history queries that
// power the auto-delegate breaker.
//
// All methods take orgID. Local mode passes runmode.LocalDefaultOrgID
// (asserted by the SQLite impl). Multi mode passes the per-request
// org context; the Postgres impl includes org_id in WHERE clauses as
// defense in depth alongside RLS.
//
// Method conventions:
//
//   - Get / FindActive* return (nil, nil) when no row matches — a
//     missing task is a normal outcome on the read paths, not an
//     error. Empty list reads return ([]T{}, nil) the same way the
//     pre-D2 raw functions did.
//   - FindOrCreate returns (task, created, err). The dedup path
//     keys off the partial unique index on tasks
//     (entity_id, event_type, dedup_key) WHERE status NOT IN
//     ('done', 'dismissed') — one task per real situation, regardless
//     of how many teams' rules matched. The teams an event is relevant
//     to are recorded as a visibility set via SetVisibilityTeams; the
//     caller-supplied teamID is the owning/attributed team stamped on a
//     newly created row.
//   - Claim mutations return ok=true when the row actually changed.
//     False means a guard tripped (caller doesn't broadcast,
//     usually surfaces 409). HandoffAgentClaim returns the
//     three-state HandoffResult so callers can distinguish no-op
//     from refused.
type TaskStore interface {
	// --- Lookup ---

	// Get returns a single task by ID, joined with its entity for
	// display fields. Returns (nil, nil) when no row matches.
	Get(ctx context.Context, orgID, taskID string) (*domain.Task, error)

	// Queued returns active queue tasks ordered by the matching
	// event_handler rule's sort_order then priority_score DESC.
	// Queue membership is the derived filter:
	// status='queued' AND both claim cols NULL AND not future-snoozed.
	//
	// teamIDs is the optional per-page read filter — a *multi-team* view
	// scope. Empty/nil = the union of the viewer's teams (the RLS-scoped
	// default). When non-empty, the result narrows to tasks any of those
	// teams owns (team_id) or can see (a task_teams visibility row) — the
	// "show A and B together, hide the rest" board contract. The viewer's
	// membership is still RLS-enforced underneath, so teams the caller
	// isn't in contribute nothing rather than leaking.
	Queued(ctx context.Context, orgID string, teamIDs []string) ([]domain.Task, error)

	// QueuedIncludingSnoozed mirrors Queued but drops the snooze-
	// window filter so future-snoozed rows surface too. The
	// Board Queued column uses this when the user toggles "show
	// snoozed"; the default Queued() stays the canonical "what's
	// actually pickable right now" projection. teamIDs is the same
	// optional multi-team read filter as Queued.
	QueuedIncludingSnoozed(ctx context.Context, orgID string, teamIDs []string) ([]domain.Task, error)

	// ByStatus returns tasks with the given lifecycle status,
	// ordered by priority. Two pseudo-values are mapped to claim-
	// axis queries for API back-compat:
	//   "claimed"   → claimed_by_user_id IS NOT NULL + active
	//   "delegated" → claimed_by_agent_id IS NOT NULL + active
	// Other status values are passed through literally. teamIDs is the
	// same optional multi-team read filter as Queued.
	ByStatus(ctx context.Context, orgID, status string, teamIDs []string) ([]domain.Task, error)

	// FindActiveByEntityAndType returns non-terminal tasks for an
	// entity matching the given event type. Used by inline close
	// checks to find sibling tasks to close.
	FindActiveByEntityAndType(ctx context.Context, orgID, entityID, eventType string) ([]domain.Task, error)

	// FindActiveByEntityAndTypeSystem mirrors FindActiveByEntityAndType
	// but routes through the admin pool in Postgres. The consumer is
	// the tracker — a background goroutine that reconciles
	// stale review_requested tasks when the user is no longer in a
	// PR's reviewer list. The tracker has no JWT-claims context, so
	// this read needs to bypass RLS the same way the sibling entity
	// reads do. org_id stays in the WHERE clause as defense in
	// depth; behavior matches the non-System variant.
	FindActiveByEntityAndTypeSystem(ctx context.Context, orgID, entityID, eventType string) ([]domain.Task, error)

	// FindActiveByEntity returns non-terminal tasks for an entity
	// regardless of event type. Used by entity lifecycle to close
	// everything when the underlying entity (PR / ticket) closes.
	FindActiveByEntity(ctx context.Context, orgID, entityID string) ([]domain.Task, error)

	// ListActiveRefsForEntities returns minimal active-task refs
	// (id, entity_id, event_type, dedup_key) for any entity in
	// entityIDs. Used by the factory snapshot to attach pending
	// tasks per entity in a single round-trip — no entity JOIN,
	// no priority columns. Chunks internally on SQLite's
	// variable-bind limit.
	//
	// teamIDs is the factory's per-page team filter (same shape as
	// Queued): empty = the viewer's union, non-empty narrows the refs to
	// those teams via the same RLS-mirroring predicate as Queued. Without
	// it the station drawer would surface (and let you delegate) task
	// refs outside the selected team scope even when the entity belt was
	// filtered. Postgres-only effect; SQLite is N=1 and ignores it.
	ListActiveRefsForEntities(ctx context.Context, orgID string, entityIDs []string, teamIDs []string) ([]domain.PendingTaskRef, error)

	// EntityIDsWithActiveTasks returns the set of entity IDs with
	// at least one non-terminal task, scoped to the given entity
	// source ("github" / "jira"). Used to batch-check active-task
	// membership.
	EntityIDsWithActiveTasks(ctx context.Context, orgID, source string) (map[string]struct{}, error)

	// --- Lifecycle ---

	// FindOrCreate implements the dedup logic via the partial unique
	// index (entity_id, event_type, dedup_key) WHERE status NOT IN
	// ('done', 'dismissed'). teamID is the owning/attributed team
	// stamped on a newly created row — caller-supplied, the store does
	// not synthesize one. Local mode passes runmode.LocalDefaultTeamID;
	// the SQLite impl accepts that directly. Multi mode passes the
	// router's deterministic owner pick among the matched teams; the
	// Postgres impl resolves the LocalDefaultTeamID sentinel to the
	// org's canonical team.
	//
	// If an active task exists for the (entity, event_type,
	// dedup_key) tuple, returns it with created=false regardless of
	// which team it is owned by; otherwise creates a fresh queued row
	// with created=true. Concurrent callers race on the index — the
	// loser re-reads the winner's row. The full set of matched teams
	// is recorded separately via SetVisibilityTeams.
	FindOrCreate(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64) (*domain.Task, bool, error)

	// FindOrCreateAt is FindOrCreate with a caller-supplied
	// createdAt on the new row. Used by initial-discovery backfills
	// where the activity is older than "now" (e.g. a pending review
	// request observed on a 2-week-old PR).
	FindOrCreateAt(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error)

	// SetVisibilityTeams records the visibility set for a task — the
	// teams whose handlers matched the spawning event. Inserts one
	// task_teams row per teamID, idempotent on (task_id, team_id).
	// teamIDs is additive: existing rows survive, so a re-arrival that
	// matches additional teams widens visibility without clearing the
	// originals. An empty slice is a no-op. The owning team_id stamped
	// on the task by FindOrCreate need not be included — it grants
	// visibility on its own — but passing it is harmless.
	SetVisibilityTeams(ctx context.Context, orgID, taskID string, teamIDs []string) error

	// VisibilityTeams returns the team IDs in a task's visibility set.
	// Order is unspecified.
	VisibilityTeams(ctx context.Context, orgID, taskID string) ([]string, error)

	// SetOwnerTeam updates a task's owning/attributed team_id without
	// touching the claim columns. The router uses it to consolidate the
	// owner to the acting team just before an auto-fired run is created,
	// so the run — which inherits runs.team_id from tasks.team_id at
	// insert — is attributed to the team that acted even when the
	// creation-time owner team was skipped (e.g. it had auto-delegation
	// disabled while a lower-priority team fires). The Postgres impl
	// resolves the LocalDefaultTeamID sentinel to the org's canonical
	// team. Empty teamID is a no-op.
	SetOwnerTeam(ctx context.Context, orgID, taskID, teamID string) error

	// Bump records a new matching event on an existing task — if
	// the task is snoozed, un-snoozes it (wake-on-bump). Does NOT
	// update primary_event_id; subsequent events are tracked via
	// the task_events junction (see RecordEvent).
	Bump(ctx context.Context, orgID, taskID, eventID string) error

	// Close sets a task to done with the given close reason.
	// closeEventType may be empty (only set when close_reason is
	// "auto_closed_by_event"). No-op on terminal rows.
	Close(ctx context.Context, orgID, taskID, closeReason, closeEventType string) error

	// SetStatus updates the lifecycle status only — claim cols are
	// unaffected. The only production caller is
	// revertTaskStatus in DrainTask's mark-fired-failure rollback;
	// every other lifecycle write routes through a guarded helper.
	SetStatus(ctx context.Context, orgID, taskID, status string) error

	// AdvanceStatusForUser flips a user-claimed task's lifecycle
	// status forward (board manual transitions). Guards:
	//   - task must be claimed by userID
	//   - current status must be one of {queued, in_progress, in_review}
	//   - newStatus must be one of {in_progress, in_review}
	// Refuses all other shapes — terminal transitions (done /
	// dismissed) go through Close + handleSwipe, requeue clears the
	// claim entirely, and bot-claimed tasks transition via
	// SetStatusSystem from the router. Returns ok=true when the
	// update actually changed a row; false means a guard tripped
	// (caller surfaces 409).
	AdvanceStatusForUser(ctx context.Context, orgID, taskID, userID, newStatus string) (bool, error)

	// RecordEvent inserts into the task_events junction (task_id,
	// event_id, kind). Idempotent on (task_id, event_id).
	RecordEvent(ctx context.Context, orgID, taskID, eventID, kind string) error

	// --- Claim mutations ---

	// SetClaimedByAgent stamps the agent claim with no race-safety
	// guards. Production paths use StampAgentClaimIfUnclaimed
	// (auto-trigger) or HandoffAgentClaim (user-initiated) instead;
	// this primitive survives for test fixtures and migration
	// backfills only.
	SetClaimedByAgent(ctx context.Context, orgID, taskID, agentID string) error

	// SetClaimedByUser is the symmetric unconditional user-claim
	// stamp. Same scope-warning as SetClaimedByAgent.
	SetClaimedByUser(ctx context.Context, orgID, taskID, userID string) error

	// StampAgentClaimIfUnclaimed is the race-safe agent-claim stamp
	// for the auto-trigger path. Guards on (a) no user claim,
	// (b) not already same-agent, (c) row not terminal. Atomically
	// wakes a snoozed task. Returns ok=true when the claim moved.
	//
	// actingTeamID is the team the bot acted for — the firing
	// trigger's team. On a successful claim it becomes the task's
	// owning team_id, consolidating the card to that team. Empty
	// leaves team_id unchanged (test/back-compat paths).
	StampAgentClaimIfUnclaimed(ctx context.Context, orgID, taskID, agentID, actingTeamID string) (bool, error)

	// HandoffAgentClaim is the race-safe "user delegates to bot"
	// helper — accepts unclaimed→bot, same-user→bot, idempotent
	// same-agent→bot; refuses on a different-user claim. See
	// HandoffResult for the discriminator the caller maps. On a
	// changed claim the owning team_id consolidates to the acting
	// team derived from the delegating user (their team in the task's
	// visibility set; the existing owner when that is empty or
	// ambiguous).
	HandoffAgentClaim(ctx context.Context, orgID, taskID, agentID, userID string) (HandoffResult, error)

	// ResolveClaimTeam returns the team a user→bot handoff WOULD
	// consolidate the task onto, without mutating anything — the same
	// derivation HandoffAgentClaim applies: the caller's team in the
	// task's task_teams visibility set (preferring the current owner on a
	// tie), falling back to the task's current team_id. Returns "" when
	// the task doesn't exist (or RLS hides it).
	//
	// The delegate handlers call this BEFORE the bot-enablement gate so
	// the gate checks team_agents for the team the claim will actually
	// land on — not the pre-handoff team_id, which for a multi-team
	// unclaimed task can be a team the caller doesn't belong to (whose
	// team_agents row RLS then hides, wrongly reporting the bot disabled).
	ResolveClaimTeam(ctx context.Context, orgID, taskID, userID string) (string, error)

	// TakeoverClaimFromAgent atomically flips a bot-claimed task
	// to a user claim. Race-safe: guards on the bot still holding
	// the claim AND no other user owning it. Returns ok=true on
	// success; false means the race was lost (caller surfaces 409).
	// On success the owning team_id consolidates to the acting team
	// derived from userID (see ClaimQueuedForUser).
	TakeoverClaimFromAgent(ctx context.Context, orgID, taskID, userID string) (bool, error)

	// ClaimQueuedForUser is the user-claim handler's atomic "take
	// this task off the queue" — succeeds only on (queued|snoozed)
	// + both claim cols NULL. Returns ok=true when the claim landed;
	// false means another claimant won or the task is closed.
	//
	// On success the owning team_id consolidates to the acting team:
	// the claimer's team in the task's visibility set (task_teams),
	// preferring the current owner team when the claimer belongs to it
	// and otherwise the lowest-id such team. The existing owner is kept
	// only when the intersection is empty. The result is always a team
	// the claimer is a member of, which the RLS update check requires.
	ClaimQueuedForUser(ctx context.Context, orgID, taskID, userID string) (bool, error)

	// ReassignClaimToUser (TFAC-561) is the user↔user handoff arm the
	// other claim mutations don't cover: an atomic CAS that requires the
	// task to be presently claimed by fromUserID AND toUserID to belong to
	// a team associated with the task (its task_teams visibility set or its
	// current team_id), moving the claim to toUserID. Returns ok=true when
	// the claim moved; false means either guard tripped — a genuine race
	// (someone else changed the claim/status between the caller's read and
	// this call) or toUserID sharing no team with the task — and the
	// caller re-reads to tell the two apart before surfacing 409. It does
	// NOT accept an unclaimed or bot-claimed task as a valid "from" state —
	// those go through ClaimQueuedForUser / TakeoverClaimFromAgent
	// instead, so the caller pre-checks task.ClaimedByUserID != "" before
	// calling this.
	//
	// The target-team guard exists because a claimed task's visibility is
	// governed solely by team_id (tasks_select RLS's task_teams branch only
	// ever applies to an unclaimed row) — reassigning to someone with no
	// relationship to the task's team(s) would silently hand them a claim
	// they can't even see afterward.
	//
	// On success the owning team_id consolidates to the acting team
	// derived from toUserID — the same task_teams-visibility-set
	// derivation ClaimQueuedForUser applies — so reassigning across teams
	// re-homes the card to the new claimant's team. Run ownership is
	// per-run, not per-claim: this method never touches runs — an active
	// delegated run keeps executing regardless of who now holds the
	// task's claim.
	//
	// Callers should use the admin-pool ReassignClaimToUserSystem variant
	// below rather than this one: unlike every other claim mutation, the
	// acting caller and the new claimant (toUserID) are different people,
	// so Postgres's tasks_update RLS — which requires the ACTING session to
	// hold write access to the resulting team_id — can reject a legitimate
	// cross-team handoff/override even though both guards above already
	// authorized it in Go. This method stays on the interface for
	// conformance testing and same-team callers where RLS naturally holds.
	ReassignClaimToUser(ctx context.Context, orgID, taskID, fromUserID, toUserID string) (bool, error)

	// --- Breaker ---

	// CountConsecutiveFailedRuns counts consecutive non-success
	// auto-runs at the tail of runs for (entity_id, prompt_id),
	// stopping at the first completed row. Chain instances count
	// once, not once-per-step. Used by the router to check the
	// circuit-breaker threshold.
	CountConsecutiveFailedRuns(ctx context.Context, orgID, entityID, promptID string) (int, error)

	// --- Admin-pool variants (`...System`) ---
	//
	// These mirror the per-method shape of the corresponding app-pool
	// methods but route through the admin pool (BYPASSRLS) in
	// Postgres. They exist for the router — a background eventbus
	// subscriber goroutine with no JWT-claims context that drives the
	// full event-to-task path (record event, dedup-create task, bump
	// on re-arrival, inline-close stale siblings, fire auto-delegate
	// triggers, drain pending firings). Same pattern as EntityStore +
	// ConversationStore.
	//
	// Behavior contract is identical to the non-System variants: org_id
	// stays in every WHERE clause as defense in depth, return shapes
	// are identical. The only difference is which Postgres pool the
	// statement runs on; SQLite has one connection and the two
	// variants collapse.
	GetSystem(ctx context.Context, orgID, taskID string) (*domain.Task, error)
	FindActiveByEntitySystem(ctx context.Context, orgID, entityID string) ([]domain.Task, error)
	FindOrCreateAtSystem(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error)

	// FindOrCreateAtUnlessEntityActiveSystem is FindOrCreateAtSystem's
	// twin for the became_atomic belated-discovery path (TFAC-579):
	// suppress minting a new card if the entity already has ANY active
	// task, regardless of event type — otherwise an atomic ticket that
	// gained and then lost subtasks ends up with two cards. That
	// cross-event-type invariant can't be expressed as a partial unique
	// index the way the standard (entity_id, event_type, dedup_key) dedup
	// is, because the general product model deliberately allows several
	// concurrently-active tasks of different event types on one entity —
	// became_atomic's suppression is narrower ("don't pile on top of
	// something already there"), not a general per-entity uniqueness rule.
	//
	// The Postgres impl backs this with a real DB guarantee instead of a
	// bare check-then-act: FindOrCreateAtSystem's underlying select-or-
	// insert runs under the SAME per-entity pg_advisory_xact_lock this
	// method takes for its active-check, so a normal event's task creation
	// on the entity can never land in the window between this method's
	// active-check and its own insert. suppressed=true means the check
	// found an active task and no row was created or returned (task is
	// nil); suppressed=false behaves exactly like FindOrCreateAtSystem.
	FindOrCreateAtUnlessEntityActiveSystem(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (task *domain.Task, created, suppressed bool, err error)
	SetVisibilityTeamsSystem(ctx context.Context, orgID, taskID string, teamIDs []string) error
	VisibilityTeamsSystem(ctx context.Context, orgID, taskID string) ([]string, error)
	SetOwnerTeamSystem(ctx context.Context, orgID, taskID, teamID string) error
	BumpSystem(ctx context.Context, orgID, taskID, eventID string) error
	CloseSystem(ctx context.Context, orgID, taskID, closeReason, closeEventType string) error
	SetStatusSystem(ctx context.Context, orgID, taskID, status string) error
	RecordEventSystem(ctx context.Context, orgID, taskID, eventID, kind string) error

	// MarkEventInjectedSystem flips the (task_id, event_id) timeline row's
	// kind to "injected" in place — the event bumped the task AND was folded
	// into the live run, and the fold is the fact worth surfacing over the
	// row RecordEventSystem/upsertTaskForEvent already wrote for that same
	// (task, event) pair (RecordEventSystem itself is INSERT-only and would
	// silently no-op on that PK collision). No-op if the row is absent.
	//
	// claim rides the same transaction: folding an event into a live run is
	// the bot committing to that task, so the claim is written with the fold
	// or not at all (see AgentClaimStamp). Returns claimed=true only when the
	// stamp actually moved the claim — a refusal commits the fold anyway.
	MarkEventInjectedSystem(ctx context.Context, orgID, taskID, eventID string, claim AgentClaimStamp) (claimed bool, err error)
	CountConsecutiveFailedRunsSystem(ctx context.Context, orgID, entityID, promptID string) (int, error)
	StampAgentClaimIfUnclaimedSystem(ctx context.Context, orgID, taskID, agentID, actingTeamID string) (bool, error)

	// ReassignClaimToUserSystem is ReassignClaimToUser routed through the
	// admin pool (BYPASSRLS) in Postgres — see the doc comment on
	// ReassignClaimToUser for why the reassign handler needs this instead
	// of the app-pool variant every other request-path caller uses.
	ReassignClaimToUserSystem(ctx context.Context, orgID, taskID, fromUserID, toUserID string) (bool, error)

	// OwnerTeamForLatestTaskInTypesSystem returns the team_id of the most
	// recent task on the entity whose event_type is in eventTypes AND whose
	// owner (team_id) is non-NULL, or "" when none matches. It is tier 3 of
	// the router's author-centric owning-team ladder: the caller passes the
	// author-centric event-type set so a later CI/conflict/feedback event
	// anchors to the team that already owns the entity's PR lifecycle.
	//
	// Two exclusions fall out by construction: review_requested tasks never
	// confer ownership (the caller omits that type from eventTypes — the
	// review-first trap), and an unowned (team_id NULL) task can't confer
	// ownership (the team_id IS NOT NULL filter). All statuses are
	// considered — a prior CI task that already closed (CI went green) still
	// anchors the entity's owner. Empty eventTypes returns "". Admin-pool:
	// the router runs on the eventbus goroutine with no JWT claims.
	OwnerTeamForLatestTaskInTypesSystem(ctx context.Context, orgID, entityID string, eventTypes []string) (string, error)
}

// HandoffResult discriminates the three outcomes HandoffAgentClaim
// can produce, so the caller can decide whether to broadcast on the
// claim axis, skip broadcast (no-op), or surface a refusal as 409.
type HandoffResult int

const (
	// HandoffChanged — claim actually moved (unclaimed → bot, or
	// same-user → bot transfer). Caller broadcasts task_claimed.
	HandoffChanged HandoffResult = iota
	// HandoffNoOp — same agent already owns the task. Idempotent;
	// caller skips the broadcast (and any sibling work like a
	// duplicate run spawn).
	HandoffNoOp
	// HandoffRefused — a different user owns the task (or the task
	// vanished / is terminal). Caller returns 409 — the gesture
	// shouldn't steal.
	HandoffRefused
)
