package reconcile

import (
	"context"
	"sync"
)

// Runner reconciles one org's non-terminal artifacts against GitHub as a
// background process, mirroring the scorer's Runner (internal/ai/runner.go). It
// exposes a Trigger the system:poll: subscriber signals after a GitHub poll
// completes; a single-flight gate collapses overlapping triggers so a long cycle
// never stacks.
type Runner struct {
	reconciler *Reconciler
	orgID      string

	trigger  chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	running  bool
}

// NewRunner builds a reconcile Runner for one org over the shared Reconciler.
func NewRunner(reconciler *Reconciler, orgID string) *Runner {
	return &Runner{
		reconciler: reconciler,
		orgID:      orgID,
		trigger:    make(chan struct{}, 1),
		stop:       make(chan struct{}),
	}
}

// Trigger signals the runner to reconcile. Non-blocking — if a cycle is already
// pending, the signal merges.
func (r *Runner) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Start launches the runner loop. A ctx derived from r.stop cancels any
// in-flight GitHub calls on Stop so shutdown isn't blocked waiting on the
// network.
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
// stopOnce — matches the scorer/profiler runners so all background runners
// behave alike.
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

	if err := r.reconciler.ReconcileOrg(ctx, r.orgID); err != nil {
		if ctx.Err() != nil {
			// Shutting down: Stop() cancelled ctx and it propagated out through a
			// DB call as context.Canceled — not a real failure. Mirrors the
			// profiler runner's guard.
			return
		}
		reconcileLog.Error("reconcile cycle failed", "org", r.orgID, "error", err)
	}
}
