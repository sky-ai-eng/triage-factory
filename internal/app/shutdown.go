package app

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultShutdownDrainTimeout bounds how long shutdown waits for in-flight
// dispatches before the pools close under them (TF_SHUTDOWN_DRAIN_SEC).
//
// Bounded rather than indefinite, because the orchestrator's own grace period
// is the real ceiling: a SIGKILL at the end of it truncates the write whether
// or not we are still waiting, so an unbounded wait buys nothing and costs the
// chance to log why the drain did not finish. 20s sits under the common 30s
// default with room for Close's own 5s trace flush.
const DefaultShutdownDrainTimeout = 20 * time.Second

// shutdownDrainTimeout resolves TF_SHUTDOWN_DRAIN_SEC: unset is the default,
// 0 disables the join outright (close immediately, accepting the truncated
// write), and an unparseable or negative value warns and falls back.
func shutdownDrainTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("TF_SHUTDOWN_DRAIN_SEC"))
	if raw == "" {
		return DefaultShutdownDrainTimeout
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		appLog.Warn("invalid TF_SHUTDOWN_DRAIN_SEC; using default",
			"value", raw, "default", DefaultShutdownDrainTimeout)
		return DefaultShutdownDrainTimeout
	}
	return time.Duration(n) * time.Second
}

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
	a.awaitDispatches(a.spawner.WaitForDispatches, shutdownDrainTimeout())
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

	if timeout <= 0 {
		appLog.Warn("shutdown drain disabled; in-flight dispatches will race the pool close and their terminal writes may fail — the next boot's reconcile re-queues them",
			"env", "TF_SHUTDOWN_DRAIN_SEC")
		return false
	}

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
		"timeout", timeout, "env", "TF_SHUTDOWN_DRAIN_SEC")
	return false
}
