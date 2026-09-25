package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrBlueprintStepUnindexed refuses a blueprint conversation that records no
// position in its sequence. The claim gate drives the one step
// current_step_index names, so the row could never be claimed by anyone: a
// refusal at the mint beats a row that sits invisible in the work list.
var ErrBlueprintStepUnindexed = errors.New("db: blueprint conversation has no step index")

// AssertBlueprintStepIndexed is the guard both dialects' conversation mint
// opens with. A conversation with no blueprint parent passes — the gate never
// compares an index for one.
func AssertBlueprintStepIndexed(conv domain.Conversation) error {
	if conv.BlueprintRunID != "" && conv.BlueprintStepIndex == nil {
		return fmt.Errorf("%w (conversation %s, blueprint_run %s)", ErrBlueprintStepUnindexed, conv.ID, conv.BlueprintRunID)
	}
	return nil
}

// OrphanedStepCheck is what ReconcileOrphanedConversations' checker arms found.
// The first is 'running' blueprint_runs holding no conversation at the step their
// current_step_index names. The store counts and samples; the caller logs,
// because the store layer holds no logger and the finding is the caller's to
// report.
//
// The pointer, not merely the presence of any child, because the pointer is
// what the claim gate drives: a run whose current step has no row is one
// nothing can pick up, whether it never got a first step or lost an advance.
// Both shapes hold blueprint_runs_one_active_run_per_task against their task
// forever, and neither is reachable by any arm that joins through
// conversations.
//
// A non-zero Count is a broken invariant, not a backlog: a firing commits its
// run and its first step in one transaction, and an advance commits its
// pointer and the step it names in another, so no transaction can observe one
// without the other. What it can still surface is a row from before that was
// true — an installed local database carries its history, and the forward
// migration that repairs those is what makes a survivor here worth shouting
// about.
type OrphanedStepCheck struct {
	// Count is every matching row, not just the sampled ones.
	Count int
	// Sample is up to OrphanedStepSampleLimit blueprint_run ids, oldest
	// first — enough to go look, bounded so one log line stays a log line.
	Sample []string

	// ClaimDesyncs counts terminal conversations still holding an unreleased
	// claim. Every status write releases its claim on the same transaction as
	// the flip and a request path writes no status, so this is a broken
	// invariant too: reported, never repaired.
	ClaimDesyncs int
	// ClaimDesyncSample is up to OrphanedStepSampleLimit of those
	// conversation ids, oldest first.
	ClaimDesyncSample []string
}

// ClaimRenewal is what a successful lease renewal answers: the new expiry, and
// whether a stop is pending on the conversation the claim holds, with who
// asked ("" for a system stop).
type ClaimRenewal struct {
	ExpiresAt       time.Time
	StopRequested   bool
	StopRequestedBy string
}

// ClaimRef names one claim and the conversation it holds.
type ClaimRef struct {
	ClaimID, OrgID, ConversationID string
}

// RequeueOutcome is the claim outcome RequeueConversation releases with. It
// is typed because the two values spend different budgets: a setup failure
// counts toward the dispatcher's setup budget, and a credentials wait that
// timed out counts toward nothing (see domain.Conversation.SetupFailures).
type RequeueOutcome string

const (
	// RequeueSetupFailure hands back an engagement that failed before its
	// agent ran: a workspace that would not build, a jail that would not
	// start. Counts toward the setup budget.
	RequeueSetupFailure RequeueOutcome = "requeued"
	// RequeueAwaitingCredentials hands back an engagement whose credential
	// bundle never arrived. Nothing about the conversation failed — the
	// brain's provisioner did not answer — so it counts toward no budget.
	RequeueAwaitingCredentials RequeueOutcome = "requeued_credentials"
)

// ErrInvalidRequeueOutcome refuses a RequeueConversation call naming an
// outcome outside the RequeueOutcome vocabulary. claims.outcome carries no
// CHECK in either dialect, so this refusal is the only thing keeping an
// unknown value out of the episode counts.
var ErrInvalidRequeueOutcome = errors.New("db: invalid requeue outcome")

// Valid reports whether o is one of the RequeueOutcome values.
func (o RequeueOutcome) Valid() bool {
	return o == RequeueSetupFailure || o == RequeueAwaitingCredentials
}

// StrandedRun names a running blueprint run whose current step concluded
// without its reactor running, with the step conversation the reactor has to
// be replayed for.
type StrandedRun struct {
	OrgID, BlueprintRunID, ConversationID string
}

// SettledStop is one conversation SettleUnclaimedStopsSystem settled.
// BlueprintRunID names a run this settlement cancelled, on exactly one of the
// conversations it settled under that run, so a caller cleaning up after the
// cancel does it once. It is "" on every other row: a conversation with no
// run, a plain stop, a run already concluded, or a second conversation under
// a run already reported.
type SettledStop struct {
	OrgID, ConversationID string
	BlueprintRunID        string
	StepIndex             *int
}

// OrphanedStepSampleLimit caps OrphanedStepCheck.Sample and
// OrphanedStepCheck.ClaimDesyncSample.
const OrphanedStepSampleLimit = 20

// ClaimPlacement configures the placement-aware, two-tier claim (TFAC-587,
// spec §6.2). The ZERO VALUE (Enabled=false) selects the original
// global-oldest claim — the whole placement layer is advisory, so a disabled
// config leaves ClaimNextConversation byte-identical to the pre-placement dispatcher.
// This is the "dropping the entire placement layer leaves all tests green"
// contract: every existing caller passes ClaimPlacement{} and sees no change.
type ClaimPlacement struct {
	// Enabled turns on the two-tier claim: tier 1 (preferred_executor_id =
	// the claiming executor) plus tier 2 (any conversation aged past
	// AgingInterval, or whose stamped preferred executor is not a live
	// claimant). Disabled → the claim ignores preferred_executor_id entirely.
	Enabled bool

	// AgingInterval is the tier-2 spillover delay: how long a queued
	// conversation waits for its preferred owner before ANY executor may
	// claim it (spec §6.2's ~15-30s). A conversation with no preferred (NULL)
	// is claimable immediately regardless of this — NULL is "unowned", not
	// "aging".
	AgingInterval time.Duration

	// Liveness is the heartbeat-staleness window past which a conversation's
	// stamped preferred executor is treated as no longer a live claimant
	// (dead), so the conversation spills immediately without waiting out
	// AgingInterval. Draining and dispatch-gated preferreds spill the same
	// way, read from the instances row.
	Liveness time.Duration
}

// DefaultClaimLease is how long a minted or renewed claim's authority lasts
// on database time, and the only place the number is spelled — the executor's
// renewal cadence and self-fence deadline sit beside it in internal/delegate,
// which reads this one. The holder renews well inside it and fences itself
// before it lapses, so a lease that actually expires means the holder is
// gone.
const DefaultClaimLease = 75 * time.Second

// ConversationQueueStore owns the claim loop — the ONE scan that finds
// conversations needing to be driven, on every surface. It is the sibling of
// EventQueueStore: where the event queue feeds the router, this feeds the
// dispatcher. It writes no conversations row of its own: a delegation is
// minted by the BlueprintStore door that commits the run or the pointer
// implying it, and this store picks the row up from there.
//
// There is no "queued" column. A conversation needs driving when nobody is
// driving it and it is either mid-flight (no outcome written — a fresh mint,
// or a claim that released without concluding) or parked and woken by new
// input. A worker claims it (Postgres: FOR UPDATE SKIP LOCKED over that
// predicate, with idx_claims_one_active as the actual mutual exclusion;
// SQLite: a plain single-statement claim — N=1, no contention), drives it,
// and releases the claim. A type-conditional gate rides alongside the shared
// predicate: a delegation conversation's blueprint parent must still be
// running.
//
// This is a system-service store: the dispatcher runs as a background worker
// with no per-user identity, so the Postgres impl wires against the admin pool
// (BYPASSRLS) and keeps org_id bound where it is known, defense in depth.
// SQLite collapses onto its single connection and asserts the local sentinel
// org on the org-scoped methods.
//
// The claim fence here (one worker claims one queued conversation) is
// distinct from the replay fence (one blueprint_run per
// (triggering_event_id, trigger_id), at the firing boundary in
// BlueprintStore.CreateRunWithFirstStepSystem). Neither subsumes the other:
// the replay fence decides whether a step row exists at all, this one decides
// who drives the one that does.
type ConversationQueueStore interface {
	// ClaimNextConversation claims the next conversation that needs driving, of ANY
	// surface, and mints the claim that records the engagement (executor id,
	// boot epoch, claimed_at, and — where the surface's unit of work is one
	// queued message — the message id it was minted to drive). Returns
	// (nil, nil) when nothing is claimable. The caller branches on the
	// returned conversation's Type to pick the execution arm.
	//
	// Eligibility is the needs-driving predicate (see the type doc) plus the
	// type-conditional gates. Nothing on the conversation row changes except
	// the un-park: a claim taken on the parked-and-woken arm ends the park by
	// definition, so the row goes back to mid-flight and "parked" and "being
	// driven" stay disjoint at every instant.
	//
	// placement (TFAC-587) selects the claim discipline. Disabled (the zero
	// value): the globally-oldest claimable conversation, ORDER BY
	// started_at, id — the original behavior every non-placement caller
	// relies on. Enabled: the two-tier claim — tier 1 is this executor's own
	// preferred conversations (an indexed preferred_executor_id equality),
	// tier 2 spills any conversation aged past placement.AgingInterval or
	// whose stamped preferred is not a live claimant (dead/gated/draining). A
	// conversation's own preferred owner sorts first
	// (tier 1 before tier 2), so a fresh conversation with a live owner is exclusively
	// its owner's until it ages — the warm-cache property — while a saturated
	// or dead owner never head-of-line-blocks its shard. SQLite is N=1 and
	// ignores placement entirely (one executor always self-hits tier 1).
	//
	// executorID/bootEpoch (the caller's persistent instance-registry
	// identity, TFAC-577) are stamped atomically in the same claim statement
	// — not later, once the engagement goes live — so there is never a window where
	// a 'running' row's ownership is unknown to ResetProcessingConversations (TFAC-578):
	// a crash during workspace setup, before the process ever goes live,
	// still leaves the row correctly self-attributed.
	//
	// Cross-org by design: the dispatcher is a single system worker draining
	// every tenant (the claimed conversation carries its org_id, which scopes all
	// downstream work). Postgres uses FOR UPDATE SKIP LOCKED so a future
	// multi-worker dispatcher never double-claims; SQLite is single-worker. A
	// queued step of a cancel-requested or already-terminal blueprint is
	// deliberately never claimed — the sequence-level cancel is honored here
	// (decision: a queued-not-started step cancels with zero work).
	//
	// The returned Attempts, SetupFailures and LostEngagements are scoped to
	// the conversation's current queue episode rather than its lifetime — see
	// the dialects' EpisodeSetupFailuresSQL / EpisodeLostEngagementsSQL for
	// the model, which the SQL is the definition of. The last two are the
	// dispatcher's two budgets.
	//
	// lease is how long the minted claim's authority lasts before the holder
	// must have renewed it: lease_expires_at = database now + lease, stamped
	// in the same statement as the claim row, so there is no window in which
	// a live claim carries no lease.
	ClaimNextConversation(ctx context.Context, executorID string, bootEpoch int64, placement ClaimPlacement, lease time.Duration) (*domain.Conversation, error)

	// RenewClaimLeaseSystem pushes the named claim's expiry out to database
	// now plus lease. It never extends the old timestamp, so a renewal that
	// arrives late cannot resurrect authority that already lapsed. Refused
	// with ErrClaimReleased when the claim is released, expired, or not the
	// one holding conversationID — one answer for every way this caller is
	// not the owner, exactly as the fence gives. Returns the new expiry on
	// database time.
	//
	// One statement, and it must stay one: the guard and the write cannot be
	// split without opening a window where an expired lease renews. Bookkeeping
	// rather than a domain write, so it is a documented exemption from the
	// returned-row rule — the expiry it returns IS what it persisted.
	//
	// The same statement reads the conversation's stop intent back, so the
	// holder learns of a pending stop within one renewal even when the local
	// cancel handle and the cross-pod signal both missed it.
	//
	// idle and op are the engagement's activity as its stall tracker reads
	// it: last_activity_at is stamped as database now minus idle, so the
	// executor's monotonic reading becomes a database timestamp without the
	// executor's wall clock entering it, and current_op as op, "" clearing it.
	RenewClaimLeaseSystem(ctx context.Context, orgID, conversationID, claimID string, lease, idle time.Duration, op string) (ClaimRenewal, error)

	// SettleUnclaimedStopsSystem is the dispatcher's settlement pass over
	// conversations no live claim holds. It settles two shapes, in one
	// transaction:
	//
	//   - A stop-requested conversation parks `open` with the reason its
	//     intent implies, a cancel-requested blueprint run behind it is
	//     cancelled, and the intent is cleared. A terminal conversation with
	//     a stale intent has only the intent cleared.
	//   - A non-terminal conversation (mid-flight or `open`) under a running,
	//     cancel-requested run, with no intent at all, parks `open` as
	//     blueprint_cancelled and the run is cancelled with the columns a
	//     reactor's cancel writes (abort_reason 'cancelled'). This is the
	//     step a cancel never reached — minted after the cancel listed the
	//     run's steps, or left behind by a cascade that failed after its
	//     commit — which the claim gate refuses and nothing else would end.
	//
	// An intent on the row wins over the second shape. Returns the
	// conversations settled, with the blueprint runs cancelled, for the
	// caller's worktree cleanup and its firing wake.
	//
	// A plain stop leaves the blueprint running: that is what keeps the parked
	// step resumable.
	//
	// "No live claim" is released_at alone, not the lease: a claim whose
	// holder is gone is released first (ReleaseExpiredClaimSystem by the
	// executor that minted it, TakeOverExpiredClaimsSystem by any other),
	// and the release leaves the row for this pass.
	//
	// Lock order is run before conversation, the order a run's terminal
	// write (BlueprintStore.MarkRunStatus) takes them in, so the two cannot
	// deadlock; a run another writer holds is skipped and settled on the
	// next pass. Cross-org system sweep on the admin pool; concurrent passes
	// skip each other's rows.
	SettleUnclaimedStopsSystem(ctx context.Context) ([]SettledStop, error)

	// SettleUnclaimedStopsForTaskSystem is SettleUnclaimedStopsSystem over
	// one task's conversations. It is what lets a route that stops a task's
	// runs and then mints the next one settle the stops it just requested
	// instead of waiting for the dispatcher: the same statement, so the
	// status still has one writer and the claim race is resolved the same way.
	SettleUnclaimedStopsForTaskSystem(ctx context.Context, orgID, taskID string) ([]SettledStop, error)

	// ExpiredClaimsSystem counts live claims past their expiry across every
	// org and reports how far past expiry the oldest is. Zero and 0 when
	// none. The brain's gauge reads it; nothing in the dispatcher does —
	// collecting it outside the dispatcher loop is what keeps a stuck
	// dispatcher from suppressing its own alarm.
	ExpiredClaimsSystem(ctx context.Context) (count int, oldestPastExpiry time.Duration, err error)

	// OldestIdleClaimSystem reports the longest any live claim with an
	// unexpired lease has gone without activity, measured from the
	// last_activity_at its renewal stamped to database now. Zero when no such
	// claim has stamped one. Cross-org, for the brain's gauge, like
	// ExpiredClaimsSystem beside it.
	OldestIdleClaimSystem(ctx context.Context) (time.Duration, error)

	// ExpiredClaimsOfExecutorSystem lists the live claims one executor boot
	// minted whose lease has lapsed on database time, oldest first. Only the
	// executor that minted a claim can tell whether an engagement is still
	// driving it, so this is the list that executor checks against its own
	// live engagements before releasing anything.
	ExpiredClaimsOfExecutorSystem(ctx context.Context, executorID string, bootEpoch int64) ([]ClaimRef, error)

	// ReleaseExpiredClaimSystem releases one claim with outcome 'reaped', and
	// only while it is still live and its lease has lapsed; false when either
	// no longer holds. The release is the requeue: a conversation with no live
	// claim matches the needs-driving predicate again, and a stop pending on
	// it is settled by the next settlement pass. The caller must have seen
	// that no engagement of its own drives the claim — the guard here proves
	// the lease lapsed, not that its holder has finished tearing down.
	ReleaseExpiredClaimSystem(ctx context.Context, orgID, conversationID, claimID string) (released bool, err error)

	// TakeOverExpiredClaimsSystem releases, as 'reaped', up to limit live claims
	// whose lease has lapsed on database time and which this executor boot did
	// not mint. The release is the takeover: a conversation with no live claim
	// matches the needs-driving predicate again, and a stop or a cancel pending
	// on it is settled by the settlement pass that follows. Claims under a
	// holder's fence read are skipped and taken on a later pass. Returns the
	// conversations released.
	//
	// Every expired claim qualifies, whatever its conversation's state: a
	// parked or terminal row's lapsed claim holds nothing up but the gauge,
	// and releasing it is the only thing that clears it. This executor boot's
	// own claims are excluded because only this process can tell a finished
	// engagement from one still tearing down — ReleaseExpiredClaimSystem is
	// their release. The released conversations' preferred_executor_id is
	// cleared in the same transaction: the stamp names the executor that just
	// lost them.
	TakeOverExpiredClaimsSystem(ctx context.Context, executorID string, bootEpoch int64, limit int) ([]ClaimRef, error)

	// LiveClaimsOfExecutorSystem lists every unreleased claim one executor
	// boot minted, lease lapsed or not, oldest first. The clean-shutdown
	// release reads it to find the claims whose engagements have returned.
	LiveClaimsOfExecutorSystem(ctx context.Context, executorID string, bootEpoch int64) ([]ClaimRef, error)

	// ReleaseOwnClaimsOnShutdownSystem releases, as 'requeued_shutdown', the
	// live claims this executor boot minted on the named conversations: the
	// ones whose engagements the caller has seen return. A deliberate stop is
	// not a loss: it spends neither budget, and the conversation is claimable
	// at once rather than after its lease lapses. The released conversations'
	// preferred_executor_id is cleared in the same transaction, since the
	// stamp names an executor that is leaving. Returns the count released.
	ReleaseOwnClaimsOnShutdownSystem(ctx context.Context, executorID string, bootEpoch int64, conversationIDs []string) (int, error)

	// ReleaseClaimOnShutdownSystem is one engagement handing its own claim
	// back because its executor is shutting down: the claim is released as
	// 'requeued_shutdown' and the conversation is left mid-flight, so it is
	// claimable at once and the next claim continues it. Fenced on claimID
	// like every holder write: ErrClaimReleased when the claim is no longer
	// live. preferred_executor_id is cleared in the same transaction, for the
	// reason ReleaseOwnClaimsOnShutdownSystem clears it.
	ReleaseClaimOnShutdownSystem(ctx context.Context, orgID, conversationID, claimID string) error

	// StrandedBlueprintRunsSystem returns running blueprint runs whose current
	// step's conversation reached completed or failed more than grace ago and
	// holds no unreleased claim. The reactor that should have advanced or ended
	// each one never ran. A claim released inside the grace keeps its run out
	// too, so a conversation resumed and concluded again is measured from its
	// latest engagement, not from a completion stamp an earlier one left.
	// `open` steps are never stranded: a plain stop leaves the run running on
	// purpose. Cross-org system read, oldest run first.
	StrandedBlueprintRunsSystem(ctx context.Context, grace time.Duration, limit int) ([]StrandedRun, error)

	// RequeueConversation hands a claimed conversation back after a transient
	// dispatcher failure — a workspace setup hiccup, a runtime that failed to
	// launch, a credentials wait that timed out — recording lastErr for
	// visibility. Releasing the claim IS the requeue: the conversation is
	// mid-flight, so the moment it has no claim it matches the needs-driving
	// predicate again. The claim releases with outcome, which keeps the
	// current queue episode open either way; RequeueSetupFailure is what the
	// next claim's SetupFailures counts, so the dispatcher can stop retrying
	// a conversation that fails the same way every time. An outcome outside
	// the vocabulary is refused with ErrInvalidRequeueOutcome and nothing is
	// written. Guarded on a mid-flight conversation with a live claim, so a
	// stale call can't act on a terminal or parked row.
	//
	// Returns the requeued row (ConversationStore.Get/GetSystem's
	// projection), or nil when the guard declined — nothing mid-flight with a
	// live claim to hand back (already terminal, parked, or claimless). The
	// EntityStore.Close shape: a second requeue of an already-requeued
	// conversation is not an error, and the caller that wants to know whether
	// this call was the one that requeued it now can.
	RequeueConversation(ctx context.Context, orgID, conversationID string, outcome RequeueOutcome, lastErr string) (*domain.Conversation, error)

	// ResetProcessingConversations is the boot reset: every claim this
	// executor minted in a strictly earlier boot (executor_id = executorID AND
	// boot_epoch < bootEpoch) and never released is released as 'reaped',
	// whatever its conversation's state, and the conversation's
	// preferred_executor_id is cleared. The instance-id flock proves the
	// process that minted them is gone, and a clean shutdown releases its own
	// claims on the way out, so everything this finds belongs to a process
	// that died — which is why it counts toward the loss budget: a
	// conversation that kills its process is failed after
	// TF_MAX_CLAIM_ATTEMPTS boots rather than crash-looping forever. A
	// mid-flight conversation it releases is claimable at once; a parked or
	// terminal one only stops holding a claim it should not.
	//
	// A live sibling instance's claims carry a different executor_id and are
	// never touched, which is what makes a rolling deploy or a two-replica
	// boot safe. The caller skips this entirely when the prior boot's cells
	// could not be confirmed torn down: those claims then lapse and are taken
	// over after their lease. Returns the count released.
	ResetProcessingConversations(ctx context.Context, executorID string, bootEpoch int64) (int, error)

	// MarkAwaitingCredentials parks a freshly-claimed conversation's ACTIVE claim in
	// phase='awaiting_credentials' (the conversation row is untouched)
	// and — Postgres only — fires the tf_ctl cred_request doorbell so the
	// brain's credential provisioner (internal/credprovision) resolves and
	// seals this claim's bundle without waiting for the backstop sweep.
	// credPubKey (base64 X25519) is the per-run sidecar public key the
	// brain seals the bundle to — recorded here, in the same statement as
	// the phase stamp, so the provisioner never sees a parked claim without
	// the key it needs; empty stores NULL (a caller that has no sidecar
	// key yet). Guarded on phase IS NULL, which gives the same protection
	// window the former stored-status guard did: a stale/duplicate call
	// can't re-park a claim that is already parked or mid-setup, so a late
	// duplicate never overwrites the key the brain may already have sealed
	// to. matched is false when the guard didn't hold (no active claim, or
	// one already carrying a phase).
	MarkAwaitingCredentials(ctx context.Context, orgID, conversationID, credPubKey string) (matched bool, err error)

	// GetClaim returns the conversation's current claim identity (team, claiming
	// executor, boot epoch) regardless of status — the brain's targeted,
	// single-conversation read on a cred_request notification (TFAC-614): the
	// notification carries only (org, conversation) IDs, so the brain re-reads the
	// live claim rather than trusting a payload that could be stale by
	// the time it's handled. Returns ok=false when conversationID is unknown.
	GetClaim(ctx context.Context, orgID, conversationID string) (claim AwaitingCredentialsConversation, ok bool, err error)

	// ClaimExecutorSystem resolves one claim id to the executor that took it.
	// ok=false when no such claim exists in the org, which is an answer rather
	// than an error: a claim row can be gone (its conversation purged) while
	// something still names its id.
	//
	// It exists for the resume ladder's liveness question. A workspace-snapshot
	// state row names its writer by CLAIM, deliberately — the write belongs to
	// one engagement, not to whichever process holds the conversation now — so
	// "is that persist still coming" needs this hop before the instance
	// registry can be asked, and the writing claim is usually released by then,
	// out of reach of any live-claim read.
	//
	// Org-scoped rather than cross-org: the caller is an executor resolving its
	// own conversation's history, not an operator surface.
	ClaimExecutorSystem(ctx context.Context, orgID, claimID string) (executorID string, ok bool, err error)

	// ListAwaitingCredentials returns every conversation — of EVERY surface
	// — whose active claim is currently parked in
	// phase='awaiting_credentials'. One scan serves the whole provisioner:
	// the phase column is the substrate and ConversationType is how the
	// caller routes. The brain-side backstop sweep's input; primary
	// provisioning happens synchronously off the executor's cred_request
	// tf_ctl notification, and this recovers whatever the lossy relay
	// dropped.
	ListAwaitingCredentials(ctx context.Context) ([]AwaitingCredentialsConversation, error)

	// ListActiveNeedingCredentialRefresh returns every conversation with a
	// live engagement (an unreleased claim not parked in
	// phase='awaiting_credentials') whose sealed bundle is older than
	// olderThan — the brain-side refresh sweep's input: GitHub
	// installation tokens are hour-lived, engagements aren't, so a
	// long-lived claim's git token needs periodic re-minting.
	ListActiveNeedingCredentialRefresh(ctx context.Context, olderThan time.Time) ([]AwaitingCredentialsConversation, error)

	// FleetQueueShares returns the conversation-queue occupancy of every org with any
	// active or queued work, newest-pressure first — the per-org shares the
	// operator fleet queue view (GET /api/fleet/queue) surfaces and
	// the claim's fairness ordering acts on. Active counts unreleased claims
	// (an engagement IS the occupied slot); Queued counts conversations
	// matching the needs-driving predicate; MaxConcurrentRuns is the org's
	// configured cap (nil = unlimited, also nil for a non-positive value). An
	// org with neither active claims nor queued conversations is omitted. Admin-pool,
	// cross-org: this is a fleet/operator read with no per-user identity, the
	// same posture as ClaimNextConversation (SQLite is N=1 — at most the one local org).
	FleetQueueShares(ctx context.Context) ([]OrgQueueShare, error)

	// ReconcileOrphanedConversations is the boot self-heal beside
	// ResetProcessingConversations: every child conversation left non-terminal under a
	// blueprint_run that is already terminal (completed/aborted/failed/
	// cancelled) is flipped to 'cancelled' with a completed_at stamp. Such a
	// child is unreachable by the dispatcher — the claim only takes rows
	// under a running parent, and the reset releases claims without writing
	// a status — so it would otherwise sit mid-flight forever, keeping the dispatcher on phantom
	// work and pinning its feature branch in a worktree (any sibling fetch
	// then requeues forever). The atomic cancel in
	// BlueprintStore.MarkRunStatus prevents the desync going forward; this
	// heals rows already broken at boot.
	//
	// And it runs two CHECKERS, which repair nothing. The first counts
	// terminal conversations still holding an unreleased claim — a shape no
	// writer produces, since every status write releases its claim on the
	// same transaction and a request path writes no status. The second: a 'running'
	// blueprint_run holding no conversation at the step its current_step_index
	// names is counted and logged at error with a sample of ids. That shape is
	// unreachable now that a firing commits its run and its first step in one
	// transaction and an advance commits its pointer and the step it names in
	// another, so observing one means an invariant broke — and a repair would
	// hide it. It is a check rather than nothing at all because the shape is
	// invisible to every other arm: they drive or heal the step the pointer
	// names, and this one has no such step to reach.
	//
	// Cross-org system sweep; returns the total count of rows healed — the
	// checker's count is not in it, because counting is not healing.
	ReconcileOrphanedConversations(ctx context.Context) (healed int, check OrphanedStepCheck, err error)

	// CountQueuedSystem returns how many conversations currently match the
	// needs-driving predicate across the whole deployment — the fleet-wide
	// backlog the sampler records as instance_stats.queued_visible (the depth
	// any executor could claim). A count, not a placement-eligibility filter:
	// placement is advisory, so the global depth is the honest "work waiting"
	// number. Cross-org system read on the admin pool.
	CountQueuedSystem(ctx context.Context) (int, error)

	// RecentConversationTimingsSystem returns the timing projection of every
	// conversation started at-or-after `since`, newest first, capped at
	// `limit` rows — the fleet dashboard's source for queue-wait and
	// run-duration percentiles and failure-kind rates (TFAC-589). Percentiles
	// are computed Go-side (portable across SQLite/Postgres, matching the usage
	// handler's aggregation style), so this returns rows, not aggregates.
	// Cross-org system read.
	RecentConversationTimingsSystem(ctx context.Context, since time.Time, limit int) ([]domain.ConversationTiming, error)

	// QueuedConversationAgesSystem returns every conversation currently matching the
	// needs-driving predicate: its org + enqueue time (+ any placement
	// preference), for the fleet queue view's oldest-waiting age and per-org
	// share. Unwindowed on purpose — work that has waited a long time is what
	// is worth surfacing. Cross-org system read.
	QueuedConversationAgesSystem(ctx context.Context) ([]domain.QueuedConversation, error)

	// RecentConversationTimingsForOrgSystem is RecentConversationTimingsSystem narrowed to one
	// org (WHERE org_id = orgID) — the org-scoped operations subset an org
	// admin sees on /usage (their queue waits + run durations), SaaS-safe with
	// no cross-tenant machine truth. The window is half-open [since, until):
	// unlike the fleet console's live "recent up to now" read this one honors an
	// explicit upper bound so a past-window /usage query doesn't leak newer
	// conversations (a zero until drops the upper clause). Admin pool with org bound by
	// argument, same posture as SpendByCategorySystem; the HTTP org-admin gate
	// is the authorization to read it.
	RecentConversationTimingsForOrgSystem(ctx context.Context, orgID string, since, until time.Time, limit int) ([]domain.ConversationTiming, error)

	// QueuedConversationAgesForOrgSystem is QueuedConversationAgesSystem narrowed to one org —
	// the org-scoped queue depth + oldest wait for the /usage ops subset.
	QueuedConversationAgesForOrgSystem(ctx context.Context, orgID string) ([]domain.QueuedConversation, error)

	// RecentClaimsForExecutorSystem returns the `limit` most recently claimed
	// engagements one executor drove, newest first, joined to their
	// conversation's terminal state — the fleet console's per-executor
	// sandbox breakdown, the operator answer to "which sandboxes are eating
	// this box" that whole-host instance_stats structurally cannot give.
	//
	// Deliberately NOT filtered to claims that carry measured actuals: a
	// released claim with NULL columns is a real engagement whose measurement
	// is missing (unsandboxed, an old kernel, a crashed teardown), and hiding
	// it would misreport the box's occupancy as sparser than it was. The
	// caller renders NULL as a dash.
	//
	// CROSS-ORG SYSTEM READ, admin pool — no org_id filter at all. Per the
	// read-scoping standing rule this is the operator-surface arm: the only
	// caller is the deployment-operator-gated fleet console (ee/fleet), which
	// is authorized for a deployment-wide view by construction and cannot
	// name an org to scope to. Any org-facing caller must scope instead.
	RecentClaimsForExecutorSystem(ctx context.Context, executorID string, limit int) ([]domain.ExecutorClaim, error)

	// ClaimByIDSystem returns one claim in the same projection, or (nil, nil)
	// when no such claim exists. It exists so a per-claim operator surface can
	// tell "unknown claim" (404) from "known claim that was never sampled"
	// (an empty series, which is ordinary — a sub-minute engagement or pre-sampler
	// history). Same cross-org operator-surface posture as
	// RecentClaimsForExecutorSystem.
	ClaimByIDSystem(ctx context.Context, claimID string) (*domain.ExecutorClaim, error)
}

// OrgQueueShare is one org's conversation-queue occupancy from FleetQueueShares — the
// shape the fleet queue view renders and the fairness claim reasons about.
// Active + Queued are live counts at read time; a zero-of-both org is never
// returned.
type OrgQueueShare struct {
	OrgID string
	// Active is the org's unreleased claims — every engagement occupying a
	// live executor slot (a claim parked awaiting credentials still holds
	// its slot). The value the per-org cap and the fairness ordering compare
	// against.
	Active int
	// Queued is the org's conversations still waiting to be claimed.
	Queued int
	// MaxConcurrentRuns is the org's configured concurrency cap, nil when
	// unlimited (a NULL or non-positive max_concurrent_runs). When non-nil and
	// Active >= *MaxConcurrentRuns the org is at its ceiling and its queued
	// conversations are invisible to claims until an active engagement finishes.
	MaxConcurrentRuns *int
}

// AwaitingCredentialsConversation is one row from ListAwaitingCredentials /
// ListActiveNeedingCredentialRefresh / GetClaim — the narrow shape the
// brain's credential provisioner needs to resolve and seal a claim's bundle:
// enough to look up the org's credentials, the conversation's authorized repo set
// (via TeamID), and the key to seal to (CredPubKey, with the claiming
// executor's published instance pubkey reachable via ExecutorID).
type AwaitingCredentialsConversation struct {
	ConversationID string
	OrgID          string
	// ConversationType is the owning surface ('delegation' | …) — what the
	// unified provisioner sweep routes on, since different surfaces may
	// resolve different credential sets for the same parked claim.
	ConversationType string
	TeamID           string
	TaskID           string
	ExecutorID       string
	BootEpoch        int64
	ClaimedAt        time.Time
	// CredPubKey is the per-run sidecar public key recorded by
	// MarkAwaitingCredentials; empty when the claim was parked without one.
	CredPubKey string
}
