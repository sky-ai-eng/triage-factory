package reachcache

import (
	"context"
	"sync"
)

// Runner drives the reachable-mirror refresh for a single org as a background
// loop. It mirrors repoprofile.Runner: a buffered trigger channel coalesces
// signals (single-flight) and a stop channel cancels any in-flight cycle on
// shutdown so a slow GitHub enumeration doesn't block server teardown. The cycle
// body is Refresher.RunOrg, inheriting its TTL gate and its
// "a failed or partial fetch leaves the previous answer standing" contract.
type Runner struct {
	cycle cycleFunc
	orgID string

	trigger  chan struct{}
	stop     chan struct{}
	stopOnce sync.Once

	mu           sync.Mutex
	running      bool
	forcePending bool // a force trigger arrived; consumed at the next cycle start
}

func newRunner(cycle cycleFunc, orgID string) *Runner {
	return &Runner{
		cycle:   cycle,
		orgID:   orgID,
		trigger: make(chan struct{}, 1),
		stop:    make(chan struct{}),
	}
}

// Trigger signals the runner to refresh. Non-blocking — if a cycle is already
// pending, the signal is merged, which is what makes N picker opens inside one
// TTL cost one enumeration. A force=true trigger sets a sticky flag the next
// cycle consumes, so a forced refresh (bypass the TTL) is never downgraded to a
// TTL-gated pass by a coincident non-forced trigger landing in the same window.
func (r *Runner) Trigger(force bool) {
	if force {
		r.mu.Lock()
		r.forcePending = true
		r.mu.Unlock()
	}
	select {
	case r.trigger <- struct{}{}:
	default:
		// already triggered, skip
	}
}

// Start launches the runner's loop. The derived ctx cancels when Stop() closes
// r.stop, so an in-flight enumeration gets killed on shutdown rather than
// blocking teardown until it times out.
func (r *Runner) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-r.stop
		cancel()
	}()
	go func() {
		for {
			select {
			case <-r.trigger:
				r.run(ctx)
			case <-r.stop:
				return
			}
		}
	}()
}

// Stop cancels the runner's loop and any in-flight cycle. Idempotent — safe to
// call independently of Manager.Stop (which may also reach it), since closing an
// already-closed channel would otherwise panic.
func (r *Runner) Stop() { r.stopOnce.Do(func() { close(r.stop) }) }

func (r *Runner) run(ctx context.Context) {
	r.mu.Lock()
	// Defence-in-depth single-flight guard. The Start loop calls run()
	// synchronously and the trigger channel is buffered to 1, so the loop can
	// never re-enter run() concurrently — this is always false from the loop.
	// It's kept (mirroring repoprofile.Runner) so an accidental direct caller
	// can't overlap two cycles, which for this cycle would mean two concurrent
	// full enumerations of the same account.
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	force := r.forcePending
	r.forcePending = false
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	if _, err := r.cycle(ctx, r.orgID, force); err != nil {
		if ctx.Err() != nil {
			return // shutting down
		}
		reachLog.ErrorContext(ctx, "reachable-cache refresh failed", "org", r.orgID, "error", err)
	}
}
