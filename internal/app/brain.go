package app

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// startBrain starts the leader-elected background brain as ONE UNIT
// (TFAC-583 spec §3): the durable event-queue drain worker + its sweeper,
// the poller scheduler, the tf_ctl cross-process relay listener, and
// brain-gated EE OnReady workers. Called either directly from Run (role=
// all — always self-held, term is a moot constant) or from the lease
// elector's OnAcquire callback (role=control, on every acquisition
// including a re-acquisition after an earlier demotion).
//
// MUST STAY NON-BLOCKING. At role=control this runs SYNCHRONOUSLY on the
// lease.Manager's own renewal goroutine — deliberately, see
// internal/lease's onAcquire field doc for why an async version isn't
// safe without term-fencing this doesn't have. While this function is
// running, nothing renews and nothing checks the self-demote deadline; if
// it ever took longer than TF_LEASE_DEMOTE_SEC, this pod would self-demote
// on return even though it never actually lost the lease. Every step
// below is already a goroutine spawn (go ...) or a call that itself
// spawns one (RestartAll, ctlbus.Listen) for exactly this reason — a
// future addition here must keep that shape; do NOT add a synchronous DB
// call, network round-trip, or anything else that can block for more
// than a few milliseconds directly in this function's body. (lease.Manager
// logs a WARN/ERROR if this ever regresses — see its
// warnIfCallbackAteDeadlineMargin — but that's an early-warning net, not
// a substitute for keeping this function fast.)
//
// Idempotent under brainMu: a redundant call is a no-op, guarding against
// (in principle) a lease manager promoting twice without an intervening
// demotion, which shouldn't happen but costs nothing to guard.
//
// What's deliberately NOT started here: the scorer/classifier/profiler/
// reconciler Managers themselves. They're always constructed (buildAI,
// gated on a.plan.brain — true for every brain-capable role, holder or
// not) but never explicitly "started" — each is a per-org Manager that
// lazy-creates a Runner on its first Trigger. Their only Trigger sources
// are (a) the poll-completion bus subscribers, which only fire while this
// process's own poller is actually running, and (b) the tf_ctl relay
// listener started below — both stop the instant this function's
// counterpart (stopBrain) runs, so no NEW work starts after demotion.
// Work already in flight is last-writer-wins-safe (spec §3's "fencing
// where it counts, tolerance elsewhere" — the one write that truly needs
// fencing, the tracker's snapshot RMW, already carries its poll_seq CAS,
// TFAC-579).
func (a *App) startBrain(term int64) {
	a.brainMu.Lock()
	defer a.brainMu.Unlock()
	if a.brainRunning {
		return
	}
	a.brainRunning = true
	brainCtx, cancel := context.WithCancel(a.runCtx)
	a.brainCancel = cancel
	appLog.Info("background brain: starting", "term", term)

	// Drain sweeper: safety net for queues stuck on transient fire errors.
	go a.router.RunDrainSweeper(brainCtx, 30*time.Second)
	// Durable event-queue drain worker: claims github:/jira: events the
	// ingestor enqueued, routes them, and marks them done. Single worker,
	// global FIFO — exactly one process may run this at a time, which is
	// what makes it a brain component rather than a replica-safe one.
	go a.router.RunEventQueue(brainCtx, a.eventWake, routing.DefaultEventScanInterval, routing.DefaultEventPruneInterval, routing.DefaultEventPruneAge)
	// Poll schedule: RestartAll's initial synchronous cycle plus its
	// ticking loop. A fresh acquisition always starts cold (every org
	// due) — bounded/benign per the spec (mostly free 304s; GitHub cycles
	// are budgeted/resumable from the durable cursor, TFAC-571).
	a.reloader.initialPoll()
	// Cross-process trigger/PollSoon relay: LISTEN on tf_ctl and dispatch
	// to the in-process managers/poller. Scoped to the brain's lifetime —
	// "only the brain LISTENs" (spec §5.3's pattern for tf_bus, mirrored
	// here for tf_ctl) — so subscription scope alone keeps a standby from
	// acting on relay traffic; no per-message routing tag needed.
	a.startCtlListener(brainCtx)
	// Brain-gated EE OnReady workers (e.g. the Slack liveness adapter) —
	// gate on serveHTTP since that's when a.srv exists at all.
	if a.plan.serveHTTP {
		a.srv.StartBrainExtensionWorkers(brainCtx)
	}
}

// stopBrain stops the background brain. Idempotent under brainMu.
//
// Per component (the "what demotion means" pre-made decision): the
// poller stops STARTING new org cycles via StopAll — an in-flight org's
// current batch may finish (bounded seconds), matching poller.Manager's
// existing stop/restart contract. The drain worker, sweeper, tf_ctl
// listener, and brain-gated EE workers all stop via brainCtx
// cancellation — the drain worker in particular re-checks ctx between
// claims (drainEventQueue's ctx.Err() guard), so it stops claiming new
// rows promptly rather than draining the whole queue first. No component
// here starts a new unit of work after this call returns.
func (a *App) stopBrain(reason string) {
	a.brainMu.Lock()
	defer a.brainMu.Unlock()
	if !a.brainRunning {
		return
	}
	a.brainRunning = false
	appLog.Info("background brain: stopping", "reason", reason)
	if a.brainCancel != nil {
		a.brainCancel()
		a.brainCancel = nil
	}
	if a.pollerMgr != nil {
		a.pollerMgr.StopAll()
	}
}

// isBrainHolder reports whether THIS process currently runs the brain —
// the gate every Trigger/PollSoon relay wrapper (relay.go) uses to decide
// between an in-process call and a tf_ctl NOTIFY.
func (a *App) isBrainHolder() bool {
	switch a.plan.role {
	case runmode.RoleAll:
		// Single process, always self-holds — local mode always resolves
		// here too (InitRoleFromEnv coerces any split role to all there).
		return true
	case runmode.RoleExecutor:
		// Never brain-capable; every relay call must go over tf_ctl.
		return false
	default: // runmode.RoleControl
		return a.leaseElector != nil && a.leaseElector.IsHolder()
	}
}

// startCtlListener starts the tf_ctl LISTEN loop for a control pod that
// currently holds the brain. role=all never calls this (isBrainHolder is
// always true there, so nothing ever needs to relay INTO this process);
// an executor never runs the brain at all, so startBrain — and this —
// never runs there either.
func (a *App) startCtlListener(ctx context.Context) {
	if a.plan.role != runmode.RoleControl {
		return
	}
	go ctlbus.Listen(ctx, a.adminDSN, a.handleCtlMessage)
}
