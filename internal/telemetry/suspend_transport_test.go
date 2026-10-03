package telemetry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/suspendclock"
)

// TestSuspendCheckingTransport_DropsIdleAfterSuspend pins the request-time
// check: a keep-alive connection is reused while the suspend reading holds or
// moves under the threshold, and the first request after it moves by the
// threshold dials a fresh one, once.
func TestSuspendCheckingTransport_DropsIdleAfterSuspend(t *testing.T) {
	var suspended atomic.Int64
	suspended.Store(int64(time.Hour))
	suspendclock.SetSourceForTest(t, func() (time.Duration, bool) {
		return time.Duration(suspended.Load()), true
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	base := &http.Transport{}
	t.Cleanup(base.CloseIdleConnections)
	client := &http.Client{Transport: dropIdleAfterSuspend(base)}

	reused := func() bool {
		t.Helper()
		var got bool
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { got = info.Reused }}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		// Drained and closed, so the connection goes back to the idle pool.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return got
	}

	if reused() {
		t.Fatal("first request reused a connection from an empty pool")
	}
	if !reused() {
		t.Error("request with no suspend between did not reuse the idle connection")
	}
	suspended.Add(int64(500 * time.Millisecond))
	if !reused() {
		t.Error("request after a 500ms advance, under the 1s threshold, did not reuse the idle connection")
	}
	suspended.Add(int64(2 * time.Minute))
	if reused() {
		t.Error("first request after a 2m suspend reused a connection opened before it")
	}
	if !reused() {
		t.Error("second request after the suspend did not reuse the connection the first one opened")
	}
}

// TestDropIdleAfterSuspend_PassesThroughWhenUnsupported pins that a platform
// with no suspend reading gets its transport back unwrapped.
func TestDropIdleAfterSuspend_PassesThroughWhenUnsupported(t *testing.T) {
	suspendclock.SetSourceForTest(t, func() (time.Duration, bool) { return 0, false })
	base := &http.Transport{}
	if got := dropIdleAfterSuspend(base); got != http.RoundTripper(base) {
		t.Errorf("dropIdleAfterSuspend wrapped the transport on a platform that cannot report suspended time: %T", got)
	}
}
