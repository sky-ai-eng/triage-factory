package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SandboxStatStore owns the sandbox_stats table — the per-sandbox resource
// series the executor's stat sampler appends while jails are live, and the
// per-run usage-over-time read that renders from it.
//
// Same posture as InstanceStatStore, and deliberately so: this is executor
// telemetry, not tenant content, so it is NOT org-scoped and every method is
// admin-pool-only in Postgres (no "...System" suffix, because there is no
// app-pool counterpart to distinguish from). SQLite is N=1 — and in practice
// records nothing, since local mode never sandboxes.
type SandboxStatStore interface {
	// RecordBatch appends one sample per live jail — the whole tick in a
	// single insert, since a sampler tick produces a row for every active
	// claim at once. An empty batch is a no-op that touches no connection:
	// an idle executor must cost nothing. Duplicate (claim_id, at) rows are
	// ignored rather than erroring, so a retried tick is harmless.
	//
	// Exempt from the returned-row rule: it writes a batch of telemetry
	// samples, so there is no single row a return value could name.
	RecordBatch(ctx context.Context, stats []domain.SandboxStat) error

	// ListForClaim returns one claim's samples, oldest first — the series a
	// per-sandbox usage chart reads. Bounded by the reaper's retention
	// window; a claim that was never sampled yields no rows, not an error.
	ListForClaim(ctx context.Context, claimID string) ([]domain.SandboxStat, error)

	// Reap deletes samples older than `cutoff` and returns how many rows it
	// removed. Same retention policy and same idempotent age-range DELETE as
	// InstanceStatStore.Reap — one family, one policy.
	Reap(ctx context.Context, cutoff time.Time) (int64, error)
}
