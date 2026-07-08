package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=PendingFiringsStore --output=./mocks --case=underscore --with-expecter

// PendingFiringsStore owns the pending_firings table — the FIFO queue
// of "intent to auto-delegate" rows the router enqueues whenever an
// event matches a trigger but the entity already has an active auto
// run (or earlier queued firings ahead of it). The drain loop pops
// them in queue order as auto runs terminate.
//
// All methods take orgID; local mode passes runmode.LocalDefaultOrgID.
// Postgres impl runs against the admin pool (system-service: the
// router has no per-user identity and must operate across the org
// without impersonating any one user) and filters on org_id alongside
// the RLS policy as defense in depth. SQLite impl asserts orgID
// equals the local sentinel and otherwise ignores it (single-tenant
// by design).
//
// The per-entity firing gate is composed at the call site (router)
// from HasPendingForEntity here + AgentRunStore.HasActiveAutoRunForEntity
// — strict ownership rather than threading a runs-shaped predicate
// through this store.
type PendingFiringsStore interface {
	// Enqueue inserts a pending firing for (entity, task, trigger).
	// The partial unique index on (task_id, trigger_id) WHERE
	// status='pending' enforces dedup: a second enqueue for the same
	// combo while the first is still pending becomes a no-op via ON
	// CONFLICT DO NOTHING. Keeping the oldest queued_at preserves
	// FIFO fairness — a firing that has been waiting longer doesn't
	// get pushed to the back of the line by a duplicate event.
	//
	// Returns true if a row was newly inserted, false if the conflict
	// path fired. Callers use this to log enqueue vs collapse.
	//
	// userID populates creator_user_id (NOT NULL in the Postgres
	// schema). The router passes runmode.LocalDefaultUserID today;
	// a later milestone retrofits the call site to pass the request user
	// once handler-level claims are wired. SQLite impl ignores
	// userID — the local schema has no creator column.
	Enqueue(ctx context.Context, orgID, userID, entityID, taskID, triggerID, triggeringEventID string) (bool, error)

	// PopForEntity is a CLAIMING pop (TFAC-579): it atomically flips the
	// oldest 'pending' row for the entity to 'draining' (stamping
	// claimed_at) and returns it, or nil if none. Postgres implements
	// this as one UPDATE ... WHERE id = (SELECT ... FOR UPDATE SKIP
	// LOCKED) statement, so two concurrent drains (different processes,
	// or a leader-failover overlap within one) can never observe and pop
	// the same row — the second caller's claim simply finds the row
	// already 'draining' (excluded from the 'pending' predicate) and
	// moves on to the next one, or nil.
	//
	// The caller owns resolving a popped row to a terminal state
	// (MarkFired / MarkSkipped) or, on a transient failure, releasing it
	// back to 'pending' via Release so a future drain retries it. A
	// 'draining' row whose claimant crashed before resolving it is not
	// lost: the drain sweeper requeues rows whose claim has gone stale
	// via RequeueStaleDraining — a drain is a handful of DB round-trips
	// (Delegate is a pure enqueue), so a minutes-old claim is a dead
	// drainer, not a slow one. The per-entity in-process mutex the router
	// already holds around the whole pop→decide→mark sequence is still
	// worth keeping (it avoids needless round-trips reclaiming rows
	// across goroutines in one process), but is no longer load-bearing
	// for correctness.
	PopForEntity(ctx context.Context, orgID, entityID string) (*domain.PendingFiring, error)

	// Release reverts a 'draining' row back to 'pending' (clearing
	// claimed_at) after a transient failure (DB read, spawner.Delegate
	// error, an entity-busy fire race) downstream of PopForEntity — the
	// claim didn't pan out, but the intent is still valid and should be
	// retried by a future drain or the periodic sweeper. Guarded by
	// status='draining' so it's a no-op on a row that's since reached a
	// terminal state some other way.
	Release(ctx context.Context, orgID string, firingID int64) error

	// RequeueStaleDraining releases every 'draining' row whose claim was
	// stamped before the cutoff (or that has no claimed_at at all — a
	// legacy wedge) back to 'pending', returning how many it recovered.
	// This is the crash recovery for the claiming pop: a drainer that
	// died between PopForEntity and MarkFired/MarkSkipped/Release leaves
	// a row nothing else will ever touch. Deliberately staleness-based
	// rather than ownership-scoped (contrast runs/event_queue, TFAC-578):
	// a firing claim is a milliseconds-scale DB transaction, not
	// long-lived owned work, and redelivery is safe — the (event,
	// trigger) fence and the one-active-per-entity index absorb a
	// duplicate drain as a clean skip/defer. Called by the drain sweeper
	// each pass with a generous cutoff.
	RequeueStaleDraining(ctx context.Context, orgID string, before time.Time) (int, error)

	// MarkFired transitions a 'draining' firing to 'fired' and records the
	// blueprint_run that resulted from it (runID is a blueprint_run id — the
	// firing unit — which fired_run_id FKs to blueprint_runs). Guarded by
	// status='draining' — only a row PopForEntity actually claimed can be
	// resolved this way — so a stray call against a 'pending' or already-
	// terminal row is a no-op rather than a silent double-transition.
	MarkFired(ctx context.Context, orgID string, firingID int64, runID string) error

	// MarkSkipped transitions a 'draining' firing to 'skipped_stale'
	// with a reason describing a definitive stale outcome (task
	// closed, trigger disabled, breaker tripped, claim changed).
	// Transient fire-time failures release back to 'pending' via Release
	// instead. Skipping doesn't halt the drain loop — the next pending
	// firing for the entity is still considered.
	MarkSkipped(ctx context.Context, orgID string, firingID int64, reason string) error

	// HasPendingForEntity returns true iff the entity has any
	// pending_firings row in 'pending' OR 'draining' status. The router
	// composes this with AgentRunStore.HasActiveAutoRunForEntity to
	// enforce FIFO drainage — a new firing must queue behind older
	// queued rows OR an active auto run on the same entity. 'draining'
	// counts as queued intent: a drain mid-flight (popped but not yet
	// fired) must still close the gate, or a fresh event in that window
	// would fire immediately and jump the queue.
	HasPendingForEntity(ctx context.Context, orgID, entityID string) (bool, error)

	// ListEntitiesWithPending returns the distinct entity IDs that
	// have at least one pending_firings row in 'pending' status. Used
	// by the background drain sweeper to bound its work to entities
	// that actually need draining.
	ListEntitiesWithPending(ctx context.Context, orgID string) ([]string, error)

	// ListForEntity returns all pending_firings rows for an entity in
	// queue order (oldest first), regardless of status. Used by
	// debug/audit views and test assertions to see the full queue
	// history for an entity.
	ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.PendingFiring, error)
}
