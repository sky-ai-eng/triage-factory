package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunQueueStore owns the run queue — the work list the dispatcher drains to
// drive blueprints through their steps. It is the sibling of EventQueueStore:
// where the event queue feeds the router, the run queue feeds the delegation
// dispatcher. A blueprint step that needs to run is enqueued as a runs row in
// status='queued'; a worker claims it (Postgres: FOR UPDATE SKIP LOCKED,
// mirroring event_queue.go; SQLite: a plain single-statement claim — N=1, no
// contention), runs the agent, and on terminal the reactor advances the owning
// blueprint_run.
//
// This is a system-service store: the dispatcher runs as a background worker
// with no per-user identity, so the Postgres impl wires against the admin pool
// (BYPASSRLS) and keeps org_id bound where it is known, defense in depth.
// SQLite collapses onto its single connection and asserts the local sentinel
// org on the org-scoped methods.
//
// The claim fence here (one worker claims one queued run) is distinct from the
// replay fence (one blueprint_run per (triggering_event_id, trigger_id), at the
// firing boundary in BlueprintStore.CreateRunIfNotFiredSystem). The queue does
// not subsume the replay fence — by the time a step is enqueued the blueprint_run
// already exists.
// ClaimPlacement configures the placement-aware, two-tier claim (TFAC-587,
// spec §6.2). The ZERO VALUE (Enabled=false) selects the original
// global-oldest claim — the whole placement layer is advisory, so a disabled
// config leaves ClaimNextRun byte-identical to the pre-placement dispatcher.
// This is the "dropping the entire placement layer leaves all tests green"
// contract: every existing caller passes ClaimPlacement{} and sees no change.
type ClaimPlacement struct {
	// Enabled turns on the two-tier claim: tier 1 (preferred_executor_id =
	// the claiming executor) plus tier 2 (any run aged past AgingInterval, or
	// whose stamped preferred executor is not a live claimant). Disabled →
	// the claim ignores preferred_executor_id entirely.
	Enabled bool

	// AgingInterval is the tier-2 spillover delay: how long a queued run
	// waits for its preferred owner before ANY executor may claim it (spec
	// §6.2's ~15-30s). A run with no preferred (NULL) is claimable
	// immediately regardless of this — NULL is "unowned", not "aging".
	AgingInterval time.Duration

	// Liveness is the heartbeat-staleness window past which a run's stamped
	// preferred executor is treated as no longer a live claimant (dead), so
	// the run spills immediately without waiting out AgingInterval. Draining
	// and dispatch-gated preferreds spill the same way, read from the
	// instances row.
	Liveness time.Duration
}

type RunQueueStore interface {
	// EnqueueRun inserts a runs row in status='queued' for a blueprint step.
	// It is the work-list write the dispatcher later claims. run carries the
	// step's identity: ID, TaskID, PromptID, Model, TriggerType,
	// CreatorUserID, TriggerID, BlueprintRunID (required), BlueprintStepIndex.
	// PreferredExecutorID (TFAC-587) is the rendezvous placement stamp, empty
	// for no affinity (placement disabled, local N=1, or a non-repo key).
	// Routes through the admin pool — the dispatcher/reactor mint work items
	// with no JWT-claims context; the row's creator_user_id is still stamped
	// for audit and later RLS-scoped reads. The schema CHECK pairing
	// trigger_type with creator_user_id nullability is the caller's contract.
	EnqueueRun(ctx context.Context, orgID string, run domain.Conversation) error

	// ClaimNextRun claims a queued run whose owning blueprint_run is still
	// 'running' and not cancel-requested, flips it queued -> running, stamps
	// claimed_at + executor_id + boot_epoch, increments attempts, and returns
	// it. Returns (nil, nil) when nothing is claimable.
	//
	// placement (TFAC-587) selects the claim discipline. Disabled (the zero
	// value): the globally-oldest claimable run, ORDER BY started_at, id — the
	// original behavior every non-placement caller relies on. Enabled: the
	// two-tier claim — tier 1 is this executor's own preferred runs (an
	// indexed preferred_executor_id equality), tier 2 spills any run aged past
	// placement.AgingInterval or whose stamped preferred is not a live
	// claimant (dead/gated/draining). A run's own preferred owner sorts first
	// (tier 1 before tier 2), so a fresh run with a live owner is exclusively
	// its owner's until it ages — the warm-cache property — while a saturated
	// or dead owner never head-of-line-blocks its shard. SQLite is N=1 and
	// ignores placement entirely (one executor always self-hits tier 1).
	//
	// executorID/bootEpoch (the caller's persistent instance-registry
	// identity, TFAC-577) are stamped atomically in the same claim statement
	// — not later, once the run goes live — so there is never a window where
	// a 'running' row's ownership is unknown to ResetProcessingRuns (TFAC-578):
	// a crash during workspace setup, before the process ever goes live,
	// still leaves the row correctly self-attributed.
	//
	// Cross-org by design: the dispatcher is a single system worker draining
	// every tenant (the claimed run carries its org_id, which scopes all
	// downstream work). Postgres uses FOR UPDATE SKIP LOCKED so a future
	// multi-worker dispatcher never double-claims; SQLite is single-worker. A
	// queued step of a cancel-requested or already-terminal blueprint is
	// deliberately never claimed — the sequence-level cancel is honored here
	// (decision: a queued-not-started step cancels with zero work).
	ClaimNextRun(ctx context.Context, executorID string, bootEpoch int64, placement ClaimPlacement) (*domain.Conversation, error)

	// RequeueRun returns a claimed run to status='queued' after a transient
	// dispatcher failure (e.g. workspace setup hiccup), recording lastErr for
	// visibility. attempts is left as-is (the claim already counted it), so the
	// dispatcher can fail the run out once attempts crosses its budget. Guarded
	// by status='running' so a stale call can't resurrect a terminal row.
	RequeueRun(ctx context.Context, orgID, runID, lastErr string) error

	// ResetProcessingRuns is the boot reconcile sweep: every run left
	// mid-flight by a crash (status='running' — claimed, whether mid-setup
	// or executing; setup progress is claim phase, not status) is flipped
	// back to 'queued' so the dispatcher re-claims and re-runs it. Dormant
	// runs (open, pending_approval) are intentionally left parked — they
	// resume through their own paths, not the queue. attempts is retained
	// (mirrors
	// EventQueue.ResetProcessing) so a run that keeps hard-crashing the process
	// eventually fails out rather than crash-looping the boot.
	//
	// Ownership-scoped (TFAC-578): only rows stamped executor_id = executorID
	// AND boot_epoch < bootEpoch are reset — i.e. this instance's own orphans
	// from a strictly earlier boot of itself. A live sibling instance's
	// claimed/running rows (a different executor_id) are never touched, which
	// is what makes a rolling deploy / two-replica boot safe: the booting
	// process only ever sweeps its own prior-boot mess, never work another
	// still-live process owns. Returns the count reset.
	ResetProcessingRuns(ctx context.Context, executorID string, bootEpoch int64) (int, error)

	// MarkAwaitingCredentials parks a freshly-claimed run's ACTIVE claim in
	// phase='awaiting_credentials' (the conversation stays status='running')
	// and — Postgres only — fires the tf_ctl cred_request doorbell so the
	// brain's credential provisioner (internal/credprovision) resolves and
	// seals this run's bundle without waiting for the backstop sweep.
	// credPubKey (base64 X25519) is the per-run sidecar public key the
	// brain seals the bundle to — recorded here, in the same statement as
	// the phase stamp, so the provisioner never sees a parked run without
	// the key it needs; empty stores NULL (a caller that has no sidecar
	// key yet). Guarded on phase IS NULL, which gives the same protection
	// window the former status='running' guard did: a stale/duplicate call
	// can't re-park a claim that is already parked or mid-setup, so a late
	// duplicate never overwrites the key the brain may already have sealed
	// to. matched is false when the guard didn't hold (no active claim, or
	// one already carrying a phase).
	MarkAwaitingCredentials(ctx context.Context, orgID, runID, credPubKey string) (matched bool, err error)

	// GetClaim returns the run's current claim identity (team, claiming
	// executor, boot epoch) regardless of status — the brain's targeted,
	// single-run read on a cred_request notification (TFAC-614): the
	// notification carries only (org, run) IDs, so the brain re-reads the
	// live claim rather than trusting a payload that could be stale by
	// the time it's handled. Returns ok=false when runID is unknown.
	GetClaim(ctx context.Context, orgID, runID string) (claim AwaitingCredentialsRun, ok bool, err error)

	// RequeueAwaitingCredentials releases a run whose active claim is
	// parked in phase='awaiting_credentials' back to 'queued', clearing
	// ownership — the executor-side timeout path. Guarded on that parked
	// claim so a stale/duplicate timeout can't resurrect a row that
	// already moved on. Returns matched=false when the guard didn't hold
	// (bundle arrived just after the deadline check, or the run was
	// reaped in the meantime).
	RequeueAwaitingCredentials(ctx context.Context, orgID, runID string) (matched bool, err error)

	// ListAwaitingCredentials returns every run whose active claim is
	// currently parked in phase='awaiting_credentials' — the brain-side
	// provisioner's backstop-sweep input. Primary provisioning happens
	// synchronously off the executor's cred_request tf_ctl notification;
	// this recovers any run whose notification the lossy relay dropped.
	ListAwaitingCredentials(ctx context.Context) ([]AwaitingCredentialsRun, error)

	// ListActiveNeedingCredentialRefresh returns every actively-running
	// run (status='running', active claim not parked in
	// phase='awaiting_credentials') whose sealed bundle is older than
	// olderThan — the brain-side refresh sweep's input: GitHub
	// installation tokens are hour-lived, runs aren't, so a long-running
	// run's git token needs periodic re-minting.
	ListActiveNeedingCredentialRefresh(ctx context.Context, olderThan time.Time) ([]AwaitingCredentialsRun, error)

	// FleetQueueShares returns the run-queue occupancy of every org with any
	// active or queued work, newest-pressure first — the per-org shares the
	// operator fleet queue view (GET /api/fleet/queue) surfaces and
	// the claim's fairness ordering acts on. Active counts runs occupying a
	// live executor slot (status='running'); Queued counts
	// runs still waiting to be claimed; MaxConcurrentRuns is the org's
	// configured cap (nil = unlimited, also nil for a non-positive value). An
	// org with neither active nor queued runs is omitted. Admin-pool,
	// cross-org: this is a fleet/operator read with no per-user identity, the
	// same posture as ClaimNextRun (SQLite is N=1 — at most the one local org).
	FleetQueueShares(ctx context.Context) ([]OrgQueueShare, error)

	// ReconcileOrphanedRuns is the boot self-heal mirror of ResetProcessingRuns:
	// every child run left non-terminal (queued/claimed/running/
	// open/pending_approval/...) under a blueprint_run that is already terminal
	// (completed/aborted/failed/cancelled) is flipped to 'cancelled' with a
	// completed_at stamp. Such a child is unreachable by the dispatcher —
	// ClaimNextRun only claims under a running parent, and ResetProcessingRuns
	// only requeues under a running parent — so it would otherwise sit 'running'
	// forever, keeping the dispatcher on phantom work and pinning its feature
	// branch in a worktree (any sibling fetch then requeues forever). The atomic
	// cancel in BlueprintStore.MarkRunStatus prevents the desync going forward;
	// this heals rows already broken at boot.
	//
	// It also runs the two claim-desync janitor arms (Postgres:
	// healClaimDesyncs; SQLite mirrors them) for the shapes the app-pool
	// terminal writes can strand — a terminal conversation with a dangling
	// active claim is released (outcome mapped from status), and an in-flight
	// delegation conversation with no active claim under a running parent
	// goes back to 'queued' for re-claim. The leader reaper repeats the same
	// two arms periodically; here they run at boot in both modes.
	//
	// Cross-org system sweep; returns the total count of rows healed across
	// all arms.
	ReconcileOrphanedRuns(ctx context.Context) (int, error)

	// CountQueuedSystem returns how many runs are currently in status='queued'
	// across the whole deployment — the fleet-wide backlog the sampler records
	// as instance_stats.queued_visible (the depth any executor could claim).
	// A count, not a placement-eligibility filter: placement is advisory, so
	// the global queued depth is the honest "work waiting" number. Cross-org
	// system read on the admin pool.
	CountQueuedSystem(ctx context.Context) (int, error)

	// RecentRunTimingsSystem returns the timing projection of every run started
	// at-or-after `since`, newest first, capped at `limit` rows — the fleet
	// dashboard's source for queue-wait and run-duration percentiles and
	// failure-kind rates (TFAC-589). Percentiles are computed Go-side (portable
	// across SQLite/Postgres, matching the usage handler's aggregation style),
	// so this returns rows, not aggregates. Cross-org system read.
	RecentRunTimingsSystem(ctx context.Context, since time.Time, limit int) ([]domain.RunTiming, error)

	// QueuedRunAgesSystem returns every currently-queued run's org + enqueue
	// time (+ any placement preference), for the fleet queue view's
	// oldest-waiting age and per-org share. Unwindowed on purpose — a run that
	// has waited a long time is the one worth surfacing. Cross-org system read.
	QueuedRunAgesSystem(ctx context.Context) ([]domain.QueuedRun, error)

	// RecentRunTimingsForOrgSystem is RecentRunTimingsSystem narrowed to one
	// org (WHERE org_id = orgID) — the org-scoped operations subset an org
	// admin sees on /usage (their queue waits + run durations), SaaS-safe with
	// no cross-tenant machine truth. The window is half-open [since, until):
	// unlike the fleet console's live "recent up to now" read this one honors an
	// explicit upper bound so a past-window /usage query doesn't leak newer
	// runs (a zero until drops the upper clause). Admin pool with org bound by
	// argument, same posture as SpendByCategorySystem; the HTTP org-admin gate
	// is the authorization to read it.
	RecentRunTimingsForOrgSystem(ctx context.Context, orgID string, since, until time.Time, limit int) ([]domain.RunTiming, error)

	// QueuedRunAgesForOrgSystem is QueuedRunAgesSystem narrowed to one org —
	// the org-scoped queue depth + oldest wait for the /usage ops subset.
	QueuedRunAgesForOrgSystem(ctx context.Context, orgID string) ([]domain.QueuedRun, error)
}

// OrgQueueShare is one org's run-queue occupancy from FleetQueueShares — the
// shape the fleet queue view renders and the fairness claim reasons about.
// Active + Queued are live counts at read time; a zero-of-both org is never
// returned.
type OrgQueueShare struct {
	OrgID string
	// Active is the org's runs currently occupying a live executor slot
	// (status='running' — a claim parked awaiting credentials still holds
	// its slot) — the value the per-org cap and the fairness ordering
	// compare against.
	Active int
	// Queued is the org's runs still waiting to be claimed.
	Queued int
	// MaxConcurrentRuns is the org's configured concurrency cap, nil when
	// unlimited (a NULL or non-positive max_concurrent_runs). When non-nil and
	// Active >= *MaxConcurrentRuns the org is at its ceiling and its queued
	// runs are invisible to claims until an active run finishes.
	MaxConcurrentRuns *int
}

// AwaitingCredentialsRun is one row from ListAwaitingCredentials /
// ListActiveNeedingCredentialRefresh / GetClaim — the narrow shape the
// brain's credential provisioner needs to resolve and seal a run's bundle:
// enough to look up the org's credentials, the run's authorized repo set
// (via TeamID), and the key to seal to (CredPubKey, with the claiming
// executor's published instance pubkey reachable via ExecutorID).
type AwaitingCredentialsRun struct {
	RunID      string
	OrgID      string
	TeamID     string
	TaskID     string
	ExecutorID string
	BootEpoch  int64
	ClaimedAt  time.Time
	// CredPubKey is the per-run sidecar public key recorded by
	// MarkAwaitingCredentials; empty when the run was parked without one.
	CredPubKey string
}
