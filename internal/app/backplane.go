package app

import (
	"os"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/wsbackplane"
)

// buildWSBackplane wires the multi-mode cross-pod fan-out (TFAC-584):
// websocket.Hub.Broadcast/CloseUserConnections gain a Postgres
// LISTEN/NOTIFY relay, and the spawner/server get the presence/kick
// hooks that ride it. Local mode and any pre-multi-mode boot path never
// call this — a.wsBackplane stays nil, and every consumer (Hub,
// Spawner, Server) already treats a nil backplane as "behave exactly as
// before" (see their respective SetBackplane/SetPresenceChecker/
// SetWSBackplane doc comments).
//
// This never fails boot: the actual LISTEN connections open lazily
// inside the RunPublicListener/RunBusListener goroutines startWorkers
// starts, which reconnect with their own backoff — a Backplane that can
// never reach Postgres just leaves every pod on local-only fan-out
// (TFAC-584's "keep serving" contract), never crashes it.
func (a *App) buildWSBackplane() {
	if runmode.Current() != runmode.ModeMulti {
		return
	}
	dsn := directDSN()
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

// directDSN resolves the session-mode DSN the backplane's LISTEN
// connections use — see wsbackplane.DirectDSN's doc comment for the
// TFAC-307 pooler-split rationale. Duplicated here (rather than calling
// wsbackplane.DirectDSN directly) only so the "multi mode with no DSN at
// all" warning above can fire from app-level wiring without importing
// wsbackplane just for that one string read; wsbackplane.New's own
// callers elsewhere still resolve it via wsbackplane.DirectDSN.
func directDSN() string {
	if v := strings.TrimSpace(os.Getenv("TF_DATABASE_DIRECT_URL")); v != "" {
		return v
	}
	return os.Getenv("TF_DATABASE_URL")
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
