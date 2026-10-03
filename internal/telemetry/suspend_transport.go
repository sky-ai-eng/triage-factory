package telemetry

import (
	"net/http"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/suspendclock"
)

// suspendThreshold is how far the suspend reading has to move between two
// requests for a suspend to count. It is the value the suspend watchers in
// internal/app and the claim lease use: far above the skew two clock reads
// can show, and far below a sleep long enough to kill a connection.
const suspendThreshold = time.Second

// sharedTransport is what TracedTransport sends through when it is given no
// base: http.DefaultTransport's connection pool, behind a suspend check.
var sharedTransport = dropIdleAfterSuspend(http.DefaultTransport)

// suspendCheckingTransport closes its pool's idle connections on the first
// request after a system suspend, before that request takes one. A keep-alive
// connection opened before a suspend may be dead after it, because the NAT or
// VPN state behind it is gone, and a request that reuses one waits out its
// client's timeout before failing. Checking here rather than only from a
// watcher means no request can reach the pool between the wake and the drop,
// whichever timer fires first after the wake.
type suspendCheckingTransport struct {
	base *http.Transport

	// mu covers the reading, the comparison and the drop together, so a
	// request that finds no suspend proceeds only after any concurrent
	// request that found one has emptied the pool.
	mu   sync.Mutex
	last time.Duration
}

// dropIdleAfterSuspend wraps rt in the suspend check. rt is returned as it is
// when it is not an *http.Transport, which has no pool to drop, or when this
// platform cannot report suspended time.
func dropIdleAfterSuspend(rt http.RoundTripper) http.RoundTripper {
	base, ok := rt.(*http.Transport)
	if !ok {
		return rt
	}
	last, ok := suspendclock.Suspended()
	if !ok {
		return rt
	}
	return &suspendCheckingTransport{base: base, last: last}
}

func (t *suspendCheckingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if seen, ok := suspendclock.Suspended(); ok {
		if seen-t.last >= suspendThreshold {
			t.base.CloseIdleConnections()
		}
		t.last = seen
	}
	t.mu.Unlock()
	return t.base.RoundTrip(req)
}
