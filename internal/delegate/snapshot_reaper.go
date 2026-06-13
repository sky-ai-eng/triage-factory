// Workspace-snapshot retention reaper. Every parked/aborted run carries a
// durable workspace blob (the cold-resume backstop). Resume value decays fast —
// week-old WIP against a moved-on main is rarely worth rehydrating — so the
// blobs are bounded by a retention TTL: the reaper enumerates resumable-state
// runs whose parked/terminal timestamp predates the TTL and drops their
// snapshots. It enumerates from the DB (the blob store has no List), keyed by
// blueprint_run_id, and only reaps a key once ALL of that blueprint's resumable
// runs are past the TTL — blueprint steps share one blob.

package delegate

import (
	"context"
	"log"
	"time"
)

// DefaultSnapshotRetentionTTL is how long a parked/aborted run's durable
// workspace snapshot is kept before the reaper discards it. Capacity-driven:
// with the snapshot blob compressed (typically sub-MB–2 MB), 1,000 concurrently
// parked snapshots is low-GB, and a fortnight is well past the point where a
// resume against a moved-on base is worth rehydrating. Tunable via
// SetSnapshotRetentionTTL.
const DefaultSnapshotRetentionTTL = 14 * 24 * time.Hour

// DefaultSnapshotReapInterval is how often the retention reaper sweeps. The TTL
// is in days, so an hourly sweep is plenty frequent and each pass is a cheap
// indexed query that usually finds nothing.
const DefaultSnapshotReapInterval = time.Hour

// SetSnapshotRetentionTTL overrides the snapshot retention window. Call once at
// startup (tests inject a short value to drive the reaper deterministically). A
// non-positive value is ignored so a misconfiguration can't reap every snapshot
// immediately.
func (s *Spawner) SetSnapshotRetentionTTL(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshotRetentionTTL = d
}

// snapshotRetention returns the effective retention window, falling back to the
// default when unset.
func (s *Spawner) snapshotRetention() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshotRetentionTTL > 0 {
		return s.snapshotRetentionTTL
	}
	return DefaultSnapshotRetentionTTL
}

// RunSnapshotReaper runs the retention sweep at startup and then on a ticker
// until ctx is cancelled. Wired alongside the other startup/periodic sweeps in
// internal/app. A nil store or blob store makes it a logged no-op.
func (s *Spawner) RunSnapshotReaper(ctx context.Context, interval time.Duration) {
	if s.agentRuns == nil || s.Storage() == nil {
		log.Printf("[delegate] snapshot reaper not started: no store / blob store wired")
		return
	}
	// time.NewTicker panics on a non-positive duration; default rather than
	// crash if a caller / future refactor passes one.
	if interval <= 0 {
		interval = DefaultSnapshotReapInterval
	}
	s.ReapExpiredSnapshots(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ReapExpiredSnapshots(ctx)
		}
	}
}

// ReapExpiredSnapshots drops the durable workspace snapshot of every
// blueprint_run all of whose resumable-state runs (open / pending_approval /
// completed+abort) parked/terminated before the retention TTL. Enumerates the
// reapable keys from the DB (the blob store has no List) and discards each via
// the idempotent discardWorkspaceSnapshot, so a key already gone is a no-op.
// Best-effort: errors are logged, never fatal.
func (s *Spawner) ReapExpiredSnapshots(ctx context.Context) {
	blobs := s.Storage()
	if blobs == nil || s.agentRuns == nil {
		return
	}
	cutoff := time.Now().Add(-s.snapshotRetention())
	keys, err := s.agentRuns.ListReapableSnapshotKeysSystem(ctx, cutoff)
	if err != nil {
		log.Printf("[delegate] snapshot reaper: list reapable keys: %v", err)
		return
	}
	for _, k := range keys {
		s.discardWorkspaceSnapshot(ctx, k.OrgID, k.BlueprintRunID)
	}
	if len(keys) > 0 {
		log.Printf("[delegate] snapshot reaper: discarded %d expired workspace snapshot(s)", len(keys))
	}
}
