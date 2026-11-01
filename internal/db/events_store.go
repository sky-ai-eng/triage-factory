package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EventStore is the per-resource store for the events audit log.
// Lifted out of the pre-D2 package-level functions in
// internal/db/events.go so multi-mode Postgres callers route through
// $N placeholders + explicit org_id + JSONB metadata.
//
// Method naming follows the dual-pool convention:
//
//   - Plain methods (Record, LatestForEntityTypeAndDedupKey) run on
//     the app pool in Postgres (RLS-active). Callers are request-
//     handler equivalents and must be inside WithTx in multi-mode
//     so JWT claims (org_id, sub) are set for RLS evaluation.
//   - `...System` methods (RecordSystem, GetMetadataSystem) run on
//     the admin pool (BYPASSRLS) for background goroutines without
//     a JWT-claims context — router, delegate post-run cleanup.
//     org_id stays in the INSERT/SELECT as defense in depth.
//
// SQLite collapses both pools onto the single connection. The
// `...System` methods are thin wrappers around their non-System
// counterparts; assertLocalOrg gates every entry point.
type EventStore interface {
	// Record inserts evt into the events audit log and returns its
	// UUID. Empty evt.ID is generated as a v4; non-empty ID is bound
	// as-is so test fixtures can pin specific values. App-pool
	// variant — callers are request-handler equivalents.
	Record(ctx context.Context, orgID string, evt domain.Event) (string, error)

	// RecordSystem is the admin-pool variant for background
	// goroutines without JWT-claims context (router subscriber,
	// delegate post-run metadata enrichment). org_id stays bound in
	// the INSERT as defense in depth.
	RecordSystem(ctx context.Context, orgID string, evt domain.Event) (string, error)

	// LatestForEntityTypeAndDedupKey returns the most recent event
	// row matching (entityID, eventType, dedupKey), or (nil, nil) if
	// none. dedupKey is pushed into the WHERE clause so a
	// discriminator event type (label_added, status_changed) that
	// has multiple recent rows with different dedup_keys still
	// resolves to the right anchor — picking by event_type alone
	// and rejecting a mismatch after the fact would incorrectly
	// 400 whenever a sibling discriminator fired more recently.
	// Empty dedupKey filters to non-discriminator events (the
	// common case).
	//
	// Used by the factory drag-to-delegate handler to anchor a
	// synthesized task's primary_event_id on a real event row.
	// App-pool variant — handler-side caller.
	LatestForEntityTypeAndDedupKey(
		ctx context.Context, orgID, entityID, eventType, dedupKey string,
	) (*domain.Event, error)

	// GetMetadataSystem returns the metadata_json string for a
	// single event by ID. Returns "" when the event doesn't exist
	// or its metadata is NULL — the caller (re-derive predicate
	// matching, delegate placeholder substitution) treats both as
	// "no metadata to match against," which is the right behavior.
	//
	// Admin-pool only: today's consumers are the router re-derive
	// pass and the delegate post-run prompt builder, both system
	// services. No handler caller exists, so the speculative app-
	// pool variant is omitted per convention.
	GetMetadataSystem(ctx context.Context, orgID, eventID string) (string, error)

	// GetSystem returns the full event row by id, or (nil, nil) if the
	// event doesn't exist. The event-queue drain worker loads the event it
	// claimed from event_queue to route it (the queue row carries only
	// the id + denormalized routing fields). Admin-pool: the worker is a
	// background system service. org_id is bound in the WHERE as defense
	// in depth and stamped onto the returned event so HandleEvent has the
	// tenant context it needs.
	GetSystem(ctx context.Context, orgID, eventID string) (*domain.Event, error)
}
