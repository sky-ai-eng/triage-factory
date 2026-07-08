package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InstanceStore owns the instances table — the fleet membership registry
// every TF process registers into at boot and refreshes via periodic
// heartbeat. This is the substrate ownership-scoped recovery, placement,
// and the fleet dashboard read from later in the horizontal-scaling epic.
//
// The table is deliberately NOT org-scoped — a fleet member isn't tenant
// data — so every method here is admin-pool-only in Postgres (no app-pool
// counterpart, hence no "...System" suffix; same shape as RunQueueStore /
// EventQueueStore). SQLite is N=1: one process, one row, epoch bumping per
// restart.
type InstanceStore interface {
	// Register performs the atomic boot-registration upsert: insert a fresh
	// row with boot_epoch=1, or — on a restart with the same id — bump
	// boot_epoch by one and refresh role/version/started_at/
	// last_heartbeat_at. Returns the minted/bumped boot_epoch. Called once
	// at process boot, before the heartbeat loop starts.
	Register(ctx context.Context, id, role, version string) (bootEpoch int64, err error)

	// Heartbeat renews last_heartbeat_at and overwrites the capacity +
	// admission snapshot, fenced on (id, boot_epoch) so a stale/superseded
	// boot of the same id can't clobber a newer boot's row. matched is
	// false when the fence matched no row — a later boot of this same id
	// has already re-registered (the split-identity signal); callers log
	// that as a warning, not an error.
	Heartbeat(ctx context.Context, id string, bootEpoch int64, hb domain.InstanceHeartbeat) (matched bool, err error)

	// Get returns one instance row, or nil if id is unknown. For tests and
	// the deferred fleet-dashboard read.
	Get(ctx context.Context, id string) (*domain.Instance, error)
}
