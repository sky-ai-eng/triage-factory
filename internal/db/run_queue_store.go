package db

import (
	"context"

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
	// queued -> running, stamps claimed_at, increments attempts, and returns
	// it. Returns (nil, nil) when nothing is claimable.
	//
	// Cross-org by design: the dispatcher is a single system worker draining
	// every tenant in insertion order (the claimed run carries its org_id,
	// which scopes all downstream work). Postgres uses FOR UPDATE SKIP LOCKED
	// so a future multi-worker dispatcher never double-claims; SQLite is
	// single-worker. A queued step of a cancel-requested or already-terminal
	// blueprint is deliberately never claimed — the sequence-level cancel is
	// honored here (decision: a queued-not-started step cancels with zero work).
	ClaimNextRun(ctx context.Context) (*domain.AgentRun, error)

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
	// eventually fails out rather than crash-looping the boot. Cross-org system
	// sweep; returns the count reset.
	ResetProcessingRuns(ctx context.Context) (int, error)
}
