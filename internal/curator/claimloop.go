package curator

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// claimStore is the narrow read the executor claim loop needs — the
// claimable queued turns homed to this executor. Satisfied by
// db.CuratorStore.
type claimStore interface {
	ListClaimableTurnsForHomeSystem(ctx context.Context, homeInstanceID string) ([]domain.CuratorTurn, error)
}

// DefaultClaimScanInterval is the claim loop's backstop poll cadence: a
// doorbell-missed turn waits at most this long before the loop finds it. Kept
// short (curator turns are user-facing and latency-sensitive) but not tight —
// the doorbell carries the common case, this is only the fallback for a dropped
// tf_ctl notification.
const DefaultClaimScanInterval = 2 * time.Second

// HomeClaimLoop is the executor-side half of curator homing (spec §6.3): it
// scans for curator conversations holding an undelivered plain user message
// with no active claim, whose project's curator_homes row points at THIS
// executor, and hands each turn to its per-project session goroutine (which
// materializes the shared-RO worktrees + cwd cache and runs the turn under
// the sandbox, exactly as an in-process turn does on role=all today).
//
// It is the "equivalent short-job claim" in place of riding the run queue: a
// curator turn is an undelivered messages row, not a queued conversation,
// and one home owns a project (the per-project session serializes turns), so
// no cross-executor SKIP LOCKED contention exists — the curator_homes
// mapping already partitions the work; the claims row minted at dispatch is
// what fences a duplicate engagement. Delivery is durable (the undelivered
// row); the tf_ctl doorbell (Wake) only cuts the latency of the backstop
// poll.
//
// Built ONLY on multi-mode executor pods. role=all / local run curator turns
// in-process from SendMessage and never construct one.
type HomeClaimLoop struct {
	// enqueue hands one claimable turn to its per-project session, returning
	// false when it could not be handed off (curator shut down / queue full)
	// so the loop leaves the turn queued and retries. Production wires
	// Curator.EnqueueClaimed; tests inject a fake.
	enqueue func(orgID, projectID, conversationID string, messageID int64, creatorUserID string) bool
	store   claimStore
	selfID  string

	interval time.Duration
	wake     chan struct{}

	// tracked is the set of turn message ids this loop has already handed to
	// a session this scan-cycle horizon, so a duplicate doorbell or a
	// scan-vs-dispatch race doesn't re-feed a turn. Touched only from the
	// Run goroutine (scan is serialized there), so it needs no lock. It
	// self-prunes: an id that has left the claimable set (claimed → running,
	// or delivered) is dropped, keeping the map bounded to in-flight-queued
	// work.
	tracked map[int64]struct{}
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
		tracked:  map[int64]struct{}{},
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

// scan lists the claimable turns homed to me and feeds any not already
// handed off to their per-project sessions, then prunes tracked ids that
// have moved on.
func (l *HomeClaimLoop) scan(ctx context.Context) {
	rows, err := l.store.ListClaimableTurnsForHomeSystem(ctx, l.selfID)
	if err != nil {
		curatorLog.Warn("curator claim scan failed; retrying next tick", "error", err)
		return
	}
	queued := make(map[int64]struct{}, len(rows))
	for _, r := range rows {
		queued[r.MessageID] = struct{}{}
		if _, seen := l.tracked[r.MessageID]; seen {
			continue
		}
		if l.enqueue(r.OrgID, r.ProjectID, r.ConversationID, r.MessageID, r.CreatorUserID) {
			l.tracked[r.MessageID] = struct{}{}
		}
		// A false return (shut down / queue full) leaves the id untracked so the
		// next scan retries it.
	}
	// Self-prune: an id no longer claimable has been claimed (now running) or
	// delivered; drop it so tracked stays bounded to in-flight-queued work. A
	// pruned id that reappears (the rare duplicate-feed race) is fenced by
	// the dispatch's BeginTurn delivered check.
	for id := range l.tracked {
		if _, still := queued[id]; !still {
			delete(l.tracked, id)
		}
	}
}
