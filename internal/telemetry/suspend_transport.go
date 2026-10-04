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

// suspendCheckingTransport moves to a fresh connection pool on the first
// request after a system suspend, before that request takes a connection. A
// connection opened before a suspend may be dead after it, because the NAT or
// VPN state behind it is gone, and a request that reuses one waits out its
// client's timeout before failing.
//
// Closing the old pool's idle connections is not enough: an HTTP/2
// connection that is carrying a request is not idle, so it stays in the pool,
// and a later request would multiplex onto it. So every request after the
// wake goes through a clone of the transport, which has no connections at
// all, while a request already in flight finishes on the old one. Checking
// here rather than only from a watcher means no request can reach a
// connection opened before the suspend, whichever timer fires first after
// the wake.
type suspendCheckingTransport struct {
	// mu covers the reading, the comparison and the swap together, so a
	// request that finds no suspend proceeds only after any concurrent
	// request that found one has swapped the pool.
	mu      sync.Mutex
	current *http.Transport
	last    time.Duration
}

// dropIdleAfterSuspend wraps rt in the suspend check. rt is returned as it is
// when it is not an *http.Transport, which has no pool to replace, or when
// this platform cannot report suspended time.
func dropIdleAfterSuspend(rt http.RoundTripper) http.RoundTripper {
	base, ok := rt.(*http.Transport)
	if !ok {
		return rt
	}
	last, ok := suspendclock.Suspended()
	if !ok {
		return rt
	}
	return &suspendCheckingTransport{current: base, last: last}
}

func (t *suspendCheckingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if seen, ok := suspendclock.Suspended(); ok {
		if seen-t.last >= suspendThreshold {
			// The retired pool's idle connections are closed now. A
			// connection still carrying a request goes back to it when that
			// request ends, and is closed by the retired transport's own
			// idle timeout.
			retired := t.current
			t.current = retired.Clone()
			retired.CloseIdleConnections()
		}
		t.last = seen
	}
	current := t.current
	t.mu.Unlock()
	return current.RoundTrip(req)
}

// CloseIdleConnections closes the idle connections of the pool new requests
// are sent through.
func (t *suspendCheckingTransport) CloseIdleConnections() {
	t.mu.Lock()
	current := t.current
	t.mu.Unlock()
	current.CloseIdleConnections()
}
