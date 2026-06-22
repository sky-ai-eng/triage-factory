package repoprofile

import (
	"context"
	"sync"
)

// cycleFunc profiles one org's repos for a single cycle. force bypasses the
// 3-day TTL. The returned bool reports whether the cycle wrote any profile
// row (so the caller can skip a no-op post-cycle bootstrap). The Manager
// wires this to Profiler.RunOrg; tests inject a fake.
type cycleFunc func(ctx context.Context, orgID string, force bool) (bool, error)

// Runner drives repo profiling for a single org as a background loop. It
// mirrors ai.Runner: a buffered trigger channel coalesces signals
// (single-flight) and a stop channel cancels any in-flight profiling cycle
// on shutdown so a slow GitHub fetch doesn't block server teardown. The
// cycle body is Profiler.RunOrg, inheriting its TTL skip, per-repo client
// resolution + skip-on-error, and the "fetch error leaves the row untouched
// for retry" contract.
type Runner struct {
	cycle           cycleFunc
	orgID           string
	onCycleComplete func(orgID string)

	trigger  chan struct{}
	stop     chan struct{}
	stopOnce sync.Once

	mu           sync.Mutex
	running      bool
	forcePending bool // a force trigger arrived; consumed at the next cycle start
}

func newRunner(cycle cycleFunc, orgID string, onCycleComplete func(orgID string)) *Runner {
	return &Runner{
		cycle:           cycle,
		orgID:           orgID,
		onCycleComplete: onCycleComplete,
		trigger:         make(chan struct{}, 1),
		stop:            make(chan struct{}),
	}
}

// Trigger signals the runner to profile. Non-blocking — if a cycle is
// already pending, the signal is merged. A force=true trigger sets a sticky
// flag the next cycle consumes, so a forced re-profile (bypass the TTL) is
// never downgraded to a TTL-gated pass by a coincident non-forced trigger
// landing in the same window.
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

// Start launches the runner's loop. The derived ctx cancels when Stop()
// closes r.stop, so any in-flight profiling agent (a Haiku batch call) gets
// killed on shutdown rather than blocking teardown until it times out.
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

// Stop cancels the runner's loop and any in-flight cycle. Idempotent — safe
// to call independently of Manager.Stop (which may also reach it), since
// closing an already-closed channel would otherwise panic.
func (r *Runner) Stop() { r.stopOnce.Do(func() { close(r.stop) }) }

func (r *Runner) run(ctx context.Context) {
	r.mu.Lock()
	// Defence-in-depth single-flight guard. The Start loop calls run()
	// synchronously and the trigger channel is buffered to 1, so the loop
	// can never re-enter run() concurrently — this is always false from the
	// loop. It's kept (mirroring ai.Runner) so an accidental direct caller
	// can't overlap two cycles.
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

	changed, err := r.cycle(ctx, r.orgID, force)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; don't chase the cycle-complete chain
		}
		repoprofileLog.Error("profile cycle failed", "org", r.orgID, "error", err)
	}

	// Chain the post-profile bootstrap (warm bare clones from the freshly
	// populated clone_url) only when this cycle actually wrote a row — a
	// steady-state TTL-skip cycle changed no clone_url, so re-running the
	// bootstrap would be wasted work and spurious per-repo WS broadcasts.
	// Fire async so a slow clone pass doesn't stall the next poll-driven
	// profiling cycle; BootstrapBareClones serializes per-repo internally, so
	// overlapping invocations don't double-clone.
	if changed && r.onCycleComplete != nil {
		go r.onCycleComplete(r.orgID)
	}
}
