// Package marketplacestats computes the TFAC-540 run-derived listing stats
// (teams using / total runs / success rate / last run) across every copy of
// each within-org marketplace listing. Multi-mode only, mirroring the
// scorer/profiler/classifier/reconciler shape: a per-org Manager/Runner
// kicked off the system:poll: sentinel (internal/app/subscribers.go), TTL-
// gated so steady state is ~one no-op check per org per cycle.
//
// The aggregation itself has no per-request read path — it's entirely owned
// by MarketplaceStore.RecomputeStatsSystem (admin pool), which is the only
// writer of marketplace_listing_stats. Browse/detail reads join that table
// under ordinary RLS, so there is never an RLS-crossing read at request
// time — the load-bearing reason this has to be a system job in the first
// place (CLAUDE.md's multi-mode read-scoping standing rule, exemption iv).
package marketplacestats

import (
	"context"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ttl is generous by design — "daily-ish freshness is plenty" per the
// TFAC-540 ticket. The gate is an in-memory per-org timestamp rather than a
// DB round trip (unlike repoprofile's per-repo computed_at check): the
// aggregation query is cheap and org-scale N is small, so there's no
// staleness signal worth persisting — a process restart just costs one
// extra immediate recompute, an acceptable trade for not adding a new
// schema column or scheduler primitive.
const ttl = 24 * time.Hour

// Aggregator recomputes one org's listing stats at a time, TTL-gated. It
// wraps db.MarketplaceStore rather than a narrower interface because
// RecomputeStatsSystem is the only method it needs and the store already
// exists on every caller's dependency graph.
type Aggregator struct {
	store db.MarketplaceStore

	mu      sync.Mutex
	lastRun map[string]time.Time
}

// NewAggregator builds an Aggregator over store.
func NewAggregator(store db.MarketplaceStore) *Aggregator {
	return &Aggregator{store: store, lastRun: make(map[string]time.Time)}
}

// RunOrg recomputes orgID's listing stats unless a cycle for this org
// completed within ttl. The returned bool reports whether it actually
// recomputed (false on a TTL skip), mirroring repoprofile.Profiler.RunOrg's
// shape — useful for logging/tests, not required by the Runner.
func (a *Aggregator) RunOrg(ctx context.Context, orgID string) (bool, error) {
	a.mu.Lock()
	if last, ok := a.lastRun[orgID]; ok && time.Since(last) < ttl {
		a.mu.Unlock()
		return false, nil
	}
	a.mu.Unlock()

	if err := a.store.RecomputeStatsSystem(ctx, orgID); err != nil {
		return false, err
	}

	a.mu.Lock()
	a.lastRun[orgID] = time.Now()
	a.mu.Unlock()
	return true, nil
}
