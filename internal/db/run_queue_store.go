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
type RunQueueStore interface {
	// EnqueueRun inserts a runs row in status='queued' for a blueprint step.
	// It is the work-list write the dispatcher later claims. run carries the
	// step's identity: ID, TaskID, PromptID, Model, TriggerType,
	// CreatorUserID, TriggerID, BlueprintRunID (required), BlueprintStepIndex.
	// Routes through the admin pool — the dispatcher/reactor mint work items
	// with no JWT-claims context; the row's creator_user_id is still stamped
	// for audit and later RLS-scoped reads. The schema CHECK pairing
	// trigger_type with creator_user_id nullability is the caller's contract.
	EnqueueRun(ctx context.Context, orgID string, run domain.AgentRun) error

	// ClaimNextRun claims the globally-oldest queued run whose owning
	// blueprint_run is still 'running' and not cancel-requested, flips it
	// queued -> running, stamps claimed_at + executor_id + boot_epoch,
	// increments attempts, and returns it. Returns (nil, nil) when nothing is
	// claimable.
	//
	// executorID/bootEpoch (the caller's persistent instance-registry
	// identity, TFAC-577) are stamped atomically in the same claim statement
	// — not later, once the run goes live — so there is never a window where
	// a 'running' row's ownership is unknown to ResetProcessingRuns (TFAC-578):
	// a crash during workspace setup, before the process ever goes live,
	// still leaves the row correctly self-attributed.
	//
	// Cross-org by design: the dispatcher is a single system worker draining
	// every tenant in insertion order (the claimed run carries its org_id,
	// which scopes all downstream work). Postgres uses FOR UPDATE SKIP LOCKED
	// so a future multi-worker dispatcher never double-claims; SQLite is
	// single-worker. A queued step of a cancel-requested or already-terminal
	// blueprint is deliberately never claimed — the sequence-level cancel is
	// honored here (decision: a queued-not-started step cancels with zero work).
	ClaimNextRun(ctx context.Context, executorID string, bootEpoch int64) (*domain.AgentRun, error)

	// RequeueRun returns a claimed run to status='queued' after a transient
	// dispatcher failure (e.g. workspace setup hiccup), recording lastErr for
	// visibility. attempts is left as-is (the claim already counted it), so the
	// dispatcher can fail the run out once attempts crosses its budget. Guarded
	// by status='running' so a stale call can't resurrect a terminal row.
	RequeueRun(ctx context.Context, orgID, runID, lastErr string) error

	// ResetProcessingRuns is the boot reconcile sweep: every run left
	// mid-flight by a crash (claimed/running/setup statuses — non-terminal and
	// non-dormant, but not already 'queued') is flipped back to 'queued' so the
	// dispatcher re-claims and re-runs it. Dormant runs (open,
	// pending_approval) are intentionally left parked — they resume through
	// their own paths, not the queue. attempts is retained (mirrors
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

	// MarkAwaitingCredentials flips a freshly-claimed run (status=
	// 'running', stamped by ClaimNextRun) to status='awaiting_credentials'
	// and — Postgres only — fires the tf_ctl cred_request doorbell so the
	// brain's credential provisioner (internal/credprovision) resolves and
	// seals this run's bundle without waiting for the backstop sweep.
	// Guarded on 'running' so a stale/duplicate call can't reopen a run
	// that already moved past this gate. matched is false when the guard
	// didn't hold.
	MarkAwaitingCredentials(ctx context.Context, orgID, runID string) (matched bool, err error)

	// GetClaim returns the run's current claim identity (team, claiming
	// executor, boot epoch) regardless of status — the brain's targeted,
	// single-run read on a cred_request notification (TFAC-614): the
	// notification carries only (org, run) IDs, so the brain re-reads the
	// live claim rather than trusting a payload that could be stale by
	// the time it's handled. Returns ok=false when runID is unknown.
	GetClaim(ctx context.Context, orgID, runID string) (claim AwaitingCredentialsRun, ok bool, err error)

	// RequeueAwaitingCredentials releases a run parked in status=
	// 'awaiting_credentials' back to 'queued', clearing ownership — the
	// executor-side timeout path (TFAC-614). Guarded on
	// 'awaiting_credentials' so a stale/duplicate timeout can't resurrect
	// a row that already moved on. Returns matched=false when the guard
	// didn't hold (bundle arrived just after the deadline check, or the
	// run was reaped in the meantime).
	RequeueAwaitingCredentials(ctx context.Context, orgID, runID string) (matched bool, err error)

	// ListAwaitingCredentials returns every run currently parked in
	// status='awaiting_credentials' (TFAC-614) — the brain-side
	// provisioner's backstop-sweep input. Primary provisioning happens
	// synchronously off the executor's cred_request tf_ctl notification;
	// this recovers any run whose notification the lossy relay dropped.
	ListAwaitingCredentials(ctx context.Context) ([]AwaitingCredentialsRun, error)

	// ListActiveNeedingCredentialRefresh returns every non-terminal,
	// actively-running (claimed, not awaiting_credentials) run whose
	// sealed bundle is older than olderThan (TFAC-614) — the brain-side
	// refresh sweep's input: GitHub installation tokens are hour-lived,
	// runs aren't, so a long-running run's git token needs periodic
	// re-minting.
	ListActiveNeedingCredentialRefresh(ctx context.Context, olderThan time.Time) ([]AwaitingCredentialsRun, error)

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
	// this heals rows already broken at boot. Cross-org system sweep; returns the
	// count cancelled.
	ReconcileOrphanedRuns(ctx context.Context) (int, error)
}

// AwaitingCredentialsRun is one row from ListAwaitingCredentials /
// ListActiveNeedingCredentialRefresh (TFAC-614) — the narrow shape the
// brain's credential provisioner needs to resolve and seal a run's bundle:
// enough to look up the org's credentials, the run's authorized repo set
// (via TeamID), and the claiming executor's published pubkey (via
// ExecutorID).
type AwaitingCredentialsRun struct {
	RunID      string
	OrgID      string
	TeamID     string
	TaskID     string
	ExecutorID string
	BootEpoch  int64
	ClaimedAt  time.Time
}
