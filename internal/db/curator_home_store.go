package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// CuratorHomeStore owns the curator_homes table — the durable
// (org, project) -> home executor mapping that makes a curator session a
// placement-stable singleton (spec §6.3). The rendezvous hash
// (internal/placement) is the initial map; this table pins the winner until the
// home dies, so a benign fleet reshuffle never thrashes a warm cache, and the
// next turn after a home's heartbeat goes stale re-mints the row.
//
// Admin-pool-only in Postgres, same posture as InstanceStore /
// PlacementOverrideStore: pure placement coordination, never a browsable RLS
// surface, so every method takes an explicit orgID bound by argument rather
// than an app-pool RLS policy — hence no "...System" suffix. SQLite is N=1 and
// unscoped; the row is inert there (the hash always returns self).
type CuratorHomeStore interface {
	// Get returns the home row for (orgID, projectID), or (nil, nil) when the
	// project has never been homed — the first-turn case that mints one.
	Get(ctx context.Context, orgID, projectID string) (*domain.CuratorHome, error)

	// Upsert sets the home for (orgID, projectID) to (instanceID, bootEpoch),
	// stamping updated_at (and homed_at on first insert). Idempotent re-home:
	// re-writing the same home just refreshes updated_at. Called on the
	// control pod at turn dispatch, never on the executor's claim path.
	//
	// Returns the persisted row, read off RETURNING on the write statement
	// itself rather than from a follow-up SELECT and projecting Get's column
	// list and scanner. homed_at is the point: the conflict arm preserves the
	// original, so only the row says when this project was first homed.
	Upsert(ctx context.Context, orgID, projectID, instanceID string, bootEpoch int64) (domain.CuratorHome, error)

	// Clear removes the home row for (orgID, projectID). Idempotent — a missing
	// row is not an error. Used when a project is deleted / its curator session
	// reset so a stale mapping can't outlive the project.
	//
	// Exempt from the returned-row rule: it is a delete.
	Clear(ctx context.Context, orgID, projectID string) error
}
