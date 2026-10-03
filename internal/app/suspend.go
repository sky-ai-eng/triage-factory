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
// It drops the idle connections itself before clearing a single slot, rather
// than relying on watchSuspendForConnections: the two watchers tick
// independently, so the polls this makes due could otherwise go out on a
// connection that did not survive the suspend, and wait out the client
// timeout the drop exists to avoid. In the brain process the drop therefore
// runs twice per wake, which costs nothing.
func (a *App) watchSuspendForPolls(ctx context.Context) {
	suspendclock.Watch(ctx, suspendCheckInterval, suspendThreshold, func(time.Duration) {
		dropIdleConnections()
		a.pollerMgr.PollAllSoon()
	})
}

// dropIdleConnections closes http.DefaultTransport's idle connections. A
// keep-alive connection opened before a suspend may be dead after it, because
// the NAT or VPN state behind it is gone, and the first request to reuse one
// waits out its client's timeout before failing. Every client built on
// telemetry.TracedHTTPClient sends through http.DefaultTransport, so one call
// covers GitHub, Jira, the Jira OAuth and GitHub App flows, and Slack. A
// connection in use at the wake is left to its request.
func dropIdleConnections() {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}
