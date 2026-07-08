package db

import (
	"context"

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

	// PopForEntity returns the oldest pending firing for the entity,
	// or nil if none. Does NOT mutate or reserve the row — the
	// drainer is responsible for marking 'fired' or 'skipped_stale'
	// once it decides the outcome. Callers must serialize draining
	// per entity (the router holds a per-entity mutex around the
	// pop→decide→mark sequence; without it concurrent drains can
	// observe and double-fire the same row).
	PopForEntity(ctx context.Context, orgID, entityID string) (*domain.PendingFiring, error)

	// MarkFired transitions a pending firing to 'fired' and records the
	// blueprint_run that resulted from it (runID is a blueprint_run id — the
	// firing unit — which fired_run_id FKs to blueprint_runs). Guarded by
	// status='pending' so a duplicate drain that lost the per-entity mutex race
	// can't flip a terminal row.
	MarkFired(ctx context.Context, orgID string, firingID int64, runID string) error

	// MarkSkipped transitions a pending firing to 'skipped_stale'
	// with a reason describing a definitive stale outcome (task
	// closed, trigger disabled, breaker tripped, claim changed).
	// Transient fire-time failures stay 'pending' for retry. Skipping
	// doesn't halt the drain loop — the next pending firing for the
	// entity is still considered.
	MarkSkipped(ctx context.Context, orgID string, firingID int64, reason string) error

	// HasPendingForEntity returns true iff the entity has any
	// pending_firings row in 'pending' status. The router composes
	// this with AgentRunStore.HasActiveAutoRunForEntity to enforce
	// FIFO drainage — a new firing must queue behind older pending
	// rows OR an active auto run on the same entity.
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
