package app

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/credprovision"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/promptseed"
	"github.com/sky-ai-eng/triage-factory/internal/reaper"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// startBrain starts the leader-elected background brain as ONE UNIT
// (TFAC-583 spec §3): the durable event-queue drain worker + its sweeper,
// the poller scheduler, the tf_bus sentinel-relay LISTEN, and brain-gated
// EE OnReady workers. Called either directly from Run (role=all — always
// self-held, term is a moot constant) or from the lease elector's
// OnAcquire callback (role=control, on every acquisition including a
// re-acquisition after an earlier demotion).
//
// MUST STAY NON-BLOCKING. At role=control this runs SYNCHRONOUSLY on the
// lease.Manager's own renewal goroutine — deliberately, see
// internal/lease's onAcquire field doc for why an async version isn't
// safe without term-fencing this doesn't have. While this function is
// running, nothing renews and nothing checks the self-demote deadline; if
// it ever took longer than TF_LEASE_DEMOTE_SEC, this pod would self-demote
// on return even though it never actually lost the lease. Every step
// below is already a goroutine spawn (go ...) or a call that itself
// spawns one (RestartAll) for exactly this reason — a
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
// What's deliberately NOT started here: the scorer/profiler/
// reconciler Managers themselves. They're always constructed (buildAI,
// gated on a.plan.brain — true for every brain-capable role, holder or
// not) but never explicitly "started" — each is a per-org Manager that
// lazy-creates a Runner on its first Trigger. Their only Trigger sources
// are (a) the poll-completion bus subscribers, which only fire while this
// process's own poller is actually running (stopped by stopBrain), and
// (b) tf_ctl relay messages, whose dispatch is holder-gated
// (handleCtlMessage checks isBrainHolder, which flips false at demotion)
// — so no NEW work starts after demotion either way. Work already in
// flight is last-writer-wins-safe (spec §3's "fencing where it counts,
// tolerance elsewhere" — the one write that truly needs fencing, the
// tracker's snapshot RMW, already carries its poll_seq CAS, TFAC-579).
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
	// Terminal-state reconciler: the invariant backstop for entities whose
	// stored snapshot is terminal but whose row never flipped closed. Reaches
	// the close losses the event queue cannot — a transition the tracker
	// diffed but never published, a parked row, a row orphaned by a replaced
	// pod — none of which leave anything to retry. Minutes-cadence: it is a
	// rare-divergence sweep, not a hot path.
	go a.router.RunTerminalReconciler(brainCtx, routing.DefaultReconcileInterval)
	// Durable event-queue drain worker: claims github:/jira: events the
	// ingestor enqueued, routes them, and marks them done. Single worker,
	// global FIFO — exactly one process may run this at a time, which is
	// what makes it a brain component rather than a replica-safe one.
	go a.router.RunEventQueue(brainCtx, a.eventWake, routing.DefaultEventScanInterval, routing.DefaultEventPruneInterval, routing.DefaultEventPruneAge)
	// Poll schedule: RestartAll's initial synchronous cycle plus its
	// ticking loop. A fresh acquisition always starts cold (every org
	// due) — bounded/benign per the spec, because the per-repo ETag state
	// the catch-up cycle re-lists against is durable, so it costs mostly
	// free 304s. The GitHub round-robin resume cursor is not: it lives on
	// the poller process, so this acquisition also restarts at the head of
	// each org's repo list rather than where its predecessor stopped.
	a.reloader.initialPoll()
	// Brain-bound sentinel relay LISTEN (tf_bus): "only the brain LISTENs
	// on tf_bus" (spec §5.3) is enforced by SUBSCRIPTION scope — the
	// listener holds with the lease, stopping via brainCtx on demotion —
	// deliberately unlike tf_ctl's always-listen + dispatch-gate shape:
	// sentinel volume tracks agent activity, and a standby consuming that
	// stream only to hand it to a bus with zero system:conversation: subscribers
	// (the sole consumer, the EE lifecycle adapter, subscribes inside the
	// brain-gated OnReady worker below) would be exactly the waste §5.3
	// splits the channel to avoid. nil check: role=all never builds a
	// backplane (single-process — sentinels are already on the only bus
	// there is), nor does a control pod whose backplane found no DSN.
	if a.wsBackplane != nil {
		go a.wsBackplane.RunBusListener(brainCtx, a.bus.Publish)
	}
	// The cross-process trigger/PollSoon relay is NOT started here: since
	// TFAC-585's tf_ctl consolidation, relay messages arrive on the
	// process-lifetime shared tf_ctl listener (startCtlListener, ctl.go —
	// started once in startWorkers for every multi-mode role), and "only
	// the brain acts on relay traffic" is enforced by the isBrainHolder
	// gate at dispatch (handleCtlMessage, relay.go) instead of by
	// lease-scoped subscription. A freshly-promoted holder is therefore
	// already LISTENing the instant it acquires — no reconnect window.
	// Brain-gated EE OnReady workers (e.g. the Slack liveness adapter) —
	// gate on serveHTTP since that's when a.srv exists at all.
	if a.plan.serveHTTP {
		a.srv.StartBrainExtensionWorkers(brainCtx)
	}
	// Fleet reaper (dead-executor requeue/fail/cancel-finalize) + registry
	// GC (TFAC-586, spec §4.3/§4.1(5)) — leader-only, singleton sweeps
	// exactly like the drain sweeper above. nil in local mode and at
	// TF_ROLE=executor (buildReaper never constructs a.reaperStore there).
	if a.reaperStore != nil {
		go reaper.RunReaper(brainCtx, a.reaperStore, reaper.DefaultReapInterval, a.reaperStaleThreshold, a.reaperMaxAttempts)
		go reaper.RunRegistryGC(brainCtx, a.reaperStore, reaper.DefaultGCInterval, reaper.DefaultGCStaleAfter)
	}
	// Sealed-credential-bundle provisioner sweeps (TFAC-614): the backstop
	// for a dropped cred_request notification, and the periodic refresh of
	// hour-lived GitHub tokens on long-running runs. nil in local mode and
	// at TF_ROLE=executor (buildCredProvisioner never constructs
	// a.credProvisioner there) — RunAwaitingSweep/RunRefreshSweep are
	// themselves nil-safe no-ops too, matching every other brain-unit
	// member's defensive shape.
	//
	// One sweep across every conversation type: the row's conversation type
	// selects the resolution, so the single scan doesn't need to special-case
	// which kind of engagement is parked.
	go credprovision.RunAwaitingSweep(brainCtx, a.credProvisioner, credprovision.DefaultAwaitingSweepInterval)
	// Refresh cadence goes expiry-aware for role-mode Bedrock orgs (TFAC-616):
	// short-lived STS session creds must be re-minted before they expire, so
	// the interval + age threshold shrink with the LLM-credential TTL. At the
	// default 1h TTL this is the unchanged 5m / 30m PS-H5 cadence.
	refreshInterval, refreshAfter := credprovision.DefaultRefreshSweepInterval, credprovision.DefaultRefreshAfter
	if a.llmResolver != nil {
		refreshInterval, refreshAfter = credprovision.RefreshCadenceForTTL(a.llmResolver.TTL())
	}
	go credprovision.RunRefreshSweep(brainCtx, a.credProvisioner, refreshInterval, refreshAfter)
	// Shipped-defaults sync: bring every provisioned team's UNMODIFIED
	// copies of the shipped prompts/blueprints up to the current
	// compile-time content. Leader-only (this is the brain), idempotent, and
	// cheap to repeat every boot — the equality check makes an already-synced
	// team a pure read. Spawned so the non-blocking contract above holds.
	go a.runShippedDefaultsSync(brainCtx)
}

// runShippedDefaultsSync sweeps every provisioned org × team, bringing each
// team's unmodified shipped-default copies equal to the current shipped content
// (db.SyncShippedDefaultsForAllTeams). Per-team failures are logged and skipped
// so a bad team never blocks boot; a fresh install with no tenant no-ops. Honors
// ctx so a demoted lease holder stops mid-fleet.
func (a *App) runShippedDefaultsSync(ctx context.Context) {
	if err := db.SyncShippedDefaultsForAllTeams(ctx, a.stores, promptseed.Prompts(), promptseed.Blueprints()); err != nil {
		appLog.Warn("shipped defaults sync: completed with per-team errors", "err", err)
	}
}

// stopBrain stops the background brain. Idempotent under brainMu.
//
// Per component (the "what demotion means" pre-made decision): the
// poller stops STARTING new org cycles via StopAll — an in-flight org's
// current batch may finish (bounded seconds), matching poller.Manager's
// existing stop/restart contract. The drain worker, sweeper, tf_bus
// listener, and brain-gated EE workers all stop via brainCtx cancellation
// — the drain worker in particular re-checks ctx between claims
// (drainEventQueue's ctx.Err() guard), so it stops claiming new rows
// promptly rather than draining the whole queue first. Relay traffic
// needs no stopping: its
// dispatch gate (handleCtlMessage's isBrainHolder check) reads false the
// moment the elector demotes. No component here starts a new unit of
// work after this call returns.
//
// The drain worker's unit is the CLAIMED ROW, and it finishes the one it
// holds — routing and the terminal mark included — before stopping. That
// is a stated contract, not an accident of where the cancellation happens
// to be checked: the row is already 'processing', and the only thing that
// rescues a stranded 'processing' row is ownership-scoped boot recovery,
// which a demoted pod never reaches because it does not restart. So the
// worker pays out the debt it took on (a bounded handful of DB writes)
// and declines the next one.
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
		// Local-only (multi rejects all at boot): the single local process
		// always self-holds.
		return true
	case runmode.RoleExecutor:
		// Never brain-capable; every relay call must go over tf_ctl.
		return false
	default: // runmode.RoleControl
		return a.leaseElector != nil && a.leaseElector.IsHolder()
	}
}
