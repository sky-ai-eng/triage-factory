package app

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/pgnotify"
	"github.com/sky-ai-eng/triage-factory/internal/wakebus"
	"github.com/sky-ai-eng/triage-factory/internal/wsbackplane"
)

// startWakeListener starts this process's tf_wake LISTEN connection
// (TFAC-586): a dedicated direct connection (pgnotify's usual shape —
// session-scoped, bypasses any transaction-mode pooler) that nudges the
// local dispatcher the instant ANY pod's EnqueueRun/resume-enqueue commits,
// instead of waiting out the 2s scan-interval backstop. Every notification
// wakes the dispatcher unconditionally, regardless of kind/org — the
// payload exists for future org-scoped filtering, not because this pod
// needs it today (a claim attempt that finds nothing is a cheap no-op).
//
// Dispatcher-capable roles only (executor/all — a.plan.dispatcher, checked
// by the caller): a control pod runs no dispatcher, so a wake here would
// have nothing to nudge. Uses wsbackplane.DirectDSN, same DSN resolution
// every other LISTEN in the fabric shares. A missing DSN degrades to
// scan-interval-only latency, never a boot failure — mirrors
// startCtlListener's contract exactly.
func (a *App) startWakeListener(ctx context.Context) {
	dsn := wsbackplane.DirectDSN()
	if dsn == "" {
		appLog.Warn("multi mode with no TF_DATABASE_URL/TF_DATABASE_DIRECT_URL; tf_wake listener disabled — claim latency degrades to the scan-interval backstop")
		return
	}
	listener := pgnotify.NewListener(dsn, wakebus.Channel, func(string) {
		a.spawner.WakeDispatcher()
	})
	listener.Run(ctx)
}
