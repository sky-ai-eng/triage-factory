package marketplacestats

import (
	"context"
	"sync"
)

// Runner recomputes one org's listing stats as a background process,
// mirroring reconcile.Runner. It exposes a Trigger the system:poll:
// subscriber signals after any poll completes; a single-flight gate
// collapses overlapping triggers so a cycle never stacks.
type Runner struct {
	aggregator *Aggregator
	orgID      string

	trigger  chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	running  bool
}

// NewRunner builds a stats Runner for one org over the shared Aggregator.
func NewRunner(aggregator *Aggregator, orgID string) *Runner {
	return &Runner{
		aggregator: aggregator,
		orgID:      orgID,
		trigger:    make(chan struct{}, 1),
		stop:       make(chan struct{}),
	}
}

// Trigger signals the runner to recompute. Non-blocking — if a cycle is
// already pending, the signal merges.
func (r *Runner) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Start launches the runner loop. A ctx derived from r.stop cancels any
// in-flight query on Stop so shutdown isn't blocked.
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

// Stop cancels the runner's loop and any in-flight cycle. Idempotent via
// stopOnce — matches the scorer/profiler/reconcile runners so every
// background runner behaves alike.
func (r *Runner) Stop() { r.stopOnce.Do(func() { close(r.stop) }) }

func (r *Runner) run(ctx context.Context) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	if _, err := r.aggregator.RunOrg(ctx, r.orgID); err != nil {
		if ctx.Err() != nil {
			// Shutting down: Stop() cancelled ctx and it propagated out through
			// a DB call as context.Canceled — not a real failure.
			return
		}
		marketplaceStatsLog.Error("stats recompute cycle failed", "org", r.orgID, "error", err)
	}
}
