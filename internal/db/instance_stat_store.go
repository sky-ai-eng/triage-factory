package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InstanceStatStore owns the instance_stats table — the 1-minute telemetry
// samples each pod's sampler writes and the fleet dashboard reads for its
// timeseries and utilization windows. Like InstanceStore it is deliberately
// NOT org-scoped (a fleet member's telemetry isn't tenant data), so every
// method is admin-pool-only in Postgres — no app-pool counterpart, hence no
// "...System" suffix. SQLite is N=1: the single process samples itself.
type InstanceStatStore interface {
	// Record appends one sample. Called once a minute per pod by the sampler.
	Record(ctx context.Context, stat domain.InstanceStat) error

	// ListSince returns every sample at-or-after `since`, oldest first,
	// across all instances — the fleet-wide timeseries read. Bounded by the
	// caller's window (the reaper keeps the table to ~30 days regardless).
	ListSince(ctx context.Context, since time.Time) ([]domain.InstanceStat, error)

	// Reap deletes samples older than `cutoff` and returns how many rows it
	// removed. Idempotent; safe to run from any pod on a slow timer (the
	// DELETE is a plain age range, concurrency-safe like the ws_outbox reaper).
	Reap(ctx context.Context, cutoff time.Time) (int64, error)
}
