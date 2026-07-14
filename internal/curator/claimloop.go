package curator

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// claimStore is the narrow read the executor claim loop needs — the queued
// curator turns homed to this executor. Satisfied by db.CuratorStore.
type claimStore interface {
	ListQueuedRequestsForHomeSystem(ctx context.Context, homeInstanceID string) ([]domain.HomedCuratorRequest, error)
}

// DefaultClaimScanInterval is the claim loop's backstop poll cadence: a
// doorbell-missed turn waits at most this long before the loop finds it. Kept
// short (curator turns are user-facing and latency-sensitive) but not tight —
// the doorbell carries the common case, this is only the fallback for a dropped
// tf_ctl notification.
const DefaultClaimScanInterval = 2 * time.Second

// HomeClaimLoop is the executor-side half of curator homing (spec §6.3): it
// pulls the queued curator turns homed to THIS executor off the durable
// curator_requests queue and hands each to its per-project session goroutine
// (which materializes the shared-RO worktrees + cwd cache and runs the turn
// under the sandbox, exactly as an in-process turn does on role=all today).
//
// It is the "equivalent short-job claim" the ticket allows in place of riding
// the run queue: a curator turn is a curator_requests row, not a runs row, and
// one home owns a project (the per-project session serializes turns), so no
// cross-executor SKIP LOCKED contention exists — the home_instance_id stamp
// already partitions the work. Delivery is durable (the queued row); the tf_ctl
// doorbell (Wake) only cuts the latency of the backstop poll.
//
// Built ONLY on multi-mode executor pods. role=all / local run curator turns
// in-process from SendMessage and never construct one.
type HomeClaimLoop struct {
	// enqueue hands one claimed turn to its per-project session, returning
	// false when it could not be handed off (curator shut down / queue full) so
	// the loop leaves the row queued and retries. Production wires
	// Curator.EnqueueClaimed; tests inject a fake.
	enqueue func(orgID, projectID, requestID, creatorUserID string) bool
	store   claimStore
	selfID  string

	interval time.Duration
	wake     chan struct{}

	// tracked is the set of request ids this loop has already handed to a
	// session this scan-cycle horizon, so a duplicate doorbell or a
	// scan-vs-MarkRunning race doesn't re-feed a turn. Touched only from the
	// Run goroutine (scan is serialized there), so it needs no lock. It
	// self-prunes: an id that has left the queued set (picked up → running, or
	// terminal) is dropped, keeping the map bounded to in-flight-queued work.
	tracked map[string]struct{}
}

// NewHomeClaimLoop builds the loop over the curator runtime and the homed-turn
// read. selfID is this executor's instance id (the home stamp it claims).
func NewHomeClaimLoop(cur *Curator, store claimStore, selfID string) *HomeClaimLoop {
	return &HomeClaimLoop{
		enqueue:  cur.EnqueueClaimed,
		store:    store,
		selfID:   selfID,
		interval: DefaultClaimScanInterval,
		wake:     make(chan struct{}, 1),
		tracked:  map[string]struct{}{},
	}
}

// SetScanInterval overrides the backstop poll cadence (tests / tuning). Must be
// called before Run.
func (l *HomeClaimLoop) SetScanInterval(d time.Duration) {
	if d > 0 {
		l.interval = d
	}
}

// Wake is the doorbell: a coalescing nudge to scan now rather than wait for the
// backstop tick. Safe to call from any goroutine (the tf_ctl dispatcher); a
// pending wake is never queued twice.
func (l *HomeClaimLoop) Wake() {
	select {
	case l.wake <- struct{}{}:
	default: // a scan is already pending; it will observe the new work
	}
}

// Run drives the claim loop until ctx is cancelled: an initial scan, then a
// scan on every doorbell or backstop tick. Blocks; start it in a goroutine.
func (l *HomeClaimLoop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	l.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.wake:
			l.scan(ctx)
		case <-ticker.C:
			l.scan(ctx)
		}
	}
}

// scan lists the queued turns homed to me and feeds any not already handed off
// to their per-project sessions, then prunes tracked ids that have moved on.
func (l *HomeClaimLoop) scan(ctx context.Context) {
	rows, err := l.store.ListQueuedRequestsForHomeSystem(ctx, l.selfID)
	if err != nil {
		curatorLog.Warn("curator claim scan failed; retrying next tick", "error", err)
		return
	}
	queued := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		queued[r.ID] = struct{}{}
		if _, seen := l.tracked[r.ID]; seen {
			continue
		}
		if l.enqueue(r.OrgID, r.ProjectID, r.ID, r.CreatorUserID) {
			l.tracked[r.ID] = struct{}{}
		}
		// A false return (shut down / queue full) leaves the id untracked so the
		// next scan retries it.
	}
	// Self-prune: an id no longer queued has been picked up (now running) or is
	// terminal; drop it so tracked stays bounded to in-flight-queued work.
	for id := range l.tracked {
		if _, still := queued[id]; !still {
			delete(l.tracked, id)
		}
	}
}
