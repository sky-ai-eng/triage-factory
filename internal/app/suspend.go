package app

import (
	"context"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/suspendclock"
)

// suspendCheckInterval is how often the process's suspend watchers read the
// suspend clock, and suspendThreshold is how far that reading has to move for
// a suspend to count. They are the claim lease's values, for its reasons: the
// threshold is far above the skew two clock reads can show and far below any
// sleep long enough to leave a connection or a poll schedule stale, and the
// interval means a wake is acted on within a second.
const (
	suspendCheckInterval = time.Second
	suspendThreshold     = time.Second
)

// watchSuspendForConnections logs each wake from a system suspend and drops
// the shared transport's idle connections, until ctx is done. It runs in
// every process, since every process calls out over HTTP.
func watchSuspendForConnections(ctx context.Context) {
	suspendclock.Watch(ctx, suspendCheckInterval, suspendThreshold, func(slept time.Duration) {
		appLog.Info("system resumed from suspend", "slept", slept)
		dropIdleConnections()
	})
}

// watchSuspendForPolls makes every org due for a poll on each wake from a
// system suspend, until ctx is done. The schedule runs on Go's monotonic
// clock, which a suspend stops, so without this every org's next poll would
// still lag by up to its full interval.
//
// The polls this makes due need no ordering against the connection watcher
// above: the GitHub and Jira clients send through telemetry.TracedTransport,
// which moves to a fresh connection pool on its first request after a suspend.
func (a *App) watchSuspendForPolls(ctx context.Context) {
	suspendclock.Watch(ctx, suspendCheckInterval, suspendThreshold, func(time.Duration) {
		a.pollerMgr.PollAllSoon()
	})
}

// dropIdleConnections closes http.DefaultTransport's idle connections, which
// may be dead after a suspend because the NAT or VPN state behind them is
// gone. It covers callers that send through http.DefaultTransport directly.
func dropIdleConnections() {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}
