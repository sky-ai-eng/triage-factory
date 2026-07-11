package app

import (
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/wsbackplane"
)

// buildWSBackplane wires the cross-pod fan-out (TFAC-584):
// websocket.Hub.Broadcast/CloseUserConnections gain a Postgres
// LISTEN/NOTIFY relay, and the spawner/server get the presence/kick
// hooks that ride it. a.wsBackplane stays nil — and every consumer (Hub,
// Spawner, Server) already treats that as "behave exactly as before"
// (see their respective SetBackplane/SetPresenceChecker/SetWSBackplane
// doc comments) — in two cases:
//
//   - local mode, obviously (no Postgres at all).
//   - TF_ROLE=all, even under TF_MODE=multi: role=all is single-process
//     by construction (none of the spec's deployment shapes run more
//     than one role=all process against the same database — a fleet is
//     always control+executor roles), so Broadcast's local delivery,
//     CloseUserConnections, and PresentFor already reach every socket
//     that will ever exist. Constructing the backplane there would only
//     add idle LISTEN connections plus heartbeat/reaper goroutines for
//     zero behavioral benefit, and silently contradict this package's
//     own doc comment ("single-process TF_ROLE=all never constructs a
//     Backplane") the moment someone actually checked.
//
// This never fails boot: the actual LISTEN connections open lazily
// inside the RunPublicListener (startWorkers) / RunBusListener
// (startBrain, lease-scoped) goroutines, which reconnect with their own
// backoff — a Backplane that can never reach Postgres just leaves every
// pod on local-only fan-out (TFAC-584's "keep serving" contract), never
// crashes it.
func (a *App) buildWSBackplane() {
	if runmode.Current() != runmode.ModeMulti || a.plan.role == runmode.RoleAll {
		return
	}
	dsn := wsbackplane.DirectDSN()
	if dsn == "" {
		appLog.Warn("multi mode with no TF_DATABASE_URL/TF_DATABASE_DIRECT_URL; websocket backplane disabled, this pod runs local-only fan-out")
		return
	}
	a.wsBackplane = wsbackplane.New(a.database, dsn, a.identity.ID, a.wsHub)
	a.wsHub.SetBackplane(a.wsBackplane)
	if a.srv != nil {
		a.srv.SetWSBackplane(a.wsBackplane)
	}
}

// registerSentinelRelay subscribes TFAC-592 run sentinels
// (system:run:status / system:run:activity) for cross-pod relay to the
// brain via tf_bus — executor-only: a brain process already has these on
// its own local bus (see broadcastEvent's identical exclusion in
// subscribers.go), so relaying a brain's own sentinels back to itself
// would be a silent no-op at best. Requires the backplane (multi mode).
func (a *App) registerSentinelRelay() {
	if a.plan.brain || a.wsBackplane == nil {
		return
	}
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "tf-bus-relay",
		Filter: []string{"system:run:"},
		Handle: a.wsBackplane.PublishSentinel,
	})
}
