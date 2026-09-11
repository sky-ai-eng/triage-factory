package app

import (
	"context"
	"time"
)

// shutdownDrainTimeout bounds how long shutdown waits for in-flight dispatches
// before the pools close under them. A backstop, not a budget: the wait is for
// already-cancelled goroutines to unwind and land their terminal write, which
// takes seconds, so reaching this deadline means something is wrong rather than
// something is slow.
//
// Bounded rather than indefinite, because the orchestrator's grace period is
// the real ceiling — a SIGKILL truncates the write whether or not we are still
// waiting — so an unbounded wait buys nothing and costs the chance to log why
// the drain did not finish. It shares that ceiling with the two deadlines that
// follow it on an executor (5s to stop the healthz listener, 5s to flush
// traces), so 15s keeps the whole sequence inside the common 30s default.
const shutdownDrainTimeout = 15 * time.Second

// drainDispatches is the shutdown join: it stops this process answering ready,
// latches the drain flag, and blocks until every dispatch goroutine has
// returned or the deadline expires. Run calls it after the blocking listener
// unwinds and before main's deferred Close releases the pools.
//
// Only dispatches are joined — see startWorkers for why the rest of the worker
// set deliberately is not.
func (a *App) drainDispatches() {
	// A role that never claims has nothing in flight to finish, and latching
	// drain/shutting-down on one would only mislabel a control pod's healthz.
	if !a.plan.dispatcher || a.spawner == nil {
		return
	}
	a.awaitDispatches(a.spawner.WaitForDispatches, shutdownDrainTimeout)
}

// awaitDispatches performs the drain sequence against an injected join and
// deadline, reporting whether every dispatch finished in time. The order is
// load-bearing, not incidental:
//
//  1. Stop reporting ready, so a rolling deploy routes away from a pod that is
//     draining rather than one that has already stopped answering.
//  2. Latch draining — the same quiesce the operator drain verb sets — so what
//     the wait is bounded by, work already claimed, is also what the pod
//     reports. The two compose: drain means claim nothing new, this means
//     finish what is already claimed. Cancelling the dispatcher's context is
//     what actually stops the claiming; the flag is how a reader of the
//     healthz can tell that from a stall.
//  3. Join, on a detached context: the run context is cancelled already, so an
//     inherited deadline would expire the drain before it began.
func (a *App) awaitDispatches(wait func(context.Context) bool, timeout time.Duration) bool {
	a.shuttingDown.Store(true)
	a.spawner.SetDraining(true)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	started := time.Now()
	if wait(ctx) {
		appLog.Info("shutdown drain complete; in-flight dispatches finished", "waited", time.Since(started))
		return true
	}
	// Log and close. Nothing stronger is available or wanted: the writes still
	// running are the un-cancellable ones, so there is nothing to abort, and
	// holding the process open past the orchestrator's grace period just moves
	// the same truncation behind a SIGKILL where no line explains it.
	appLog.Warn("shutdown drain deadline expired; dispatches are still in flight and their terminal writes may fail against the closing pool — the next boot's reconcile re-queues them",
		"timeout", timeout)
	return false
}
