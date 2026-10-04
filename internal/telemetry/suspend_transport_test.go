package telemetry

import (
	"context"
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

// TestSuspendCheckingTransport_HTTP2BusyConnectionNotReused: an HTTP/2
// connection carrying a request across a suspend is not idle, so closing the
// idle pool leaves it in place, and a request after the wake would multiplex
// onto it. The first request after the suspend must dial a connection of its
// own, while the request in flight still finishes on the old one.
func TestSuspendCheckingTransport_HTTP2BusyConnectionNotReused(t *testing.T) {
	var suspended atomic.Int64
	suspended.Store(int64(time.Hour))
	suspendclock.SetSourceForTest(t, func() (time.Duration, bool) {
		return time.Duration(suspended.Load()), true
	})

	release := make(chan struct{})
	held := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			close(held)
			<-release
		}
		_, _ = io.WriteString(w, "ok")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	base := srv.Client().Transport.(*http.Transport)
	rt := dropIdleAfterSuspend(base)
	t.Cleanup(func() { rt.(*suspendCheckingTransport).CloseIdleConnections() })
	client := &http.Client{Transport: rt}

	type result struct {
		conn  string
		proto string
		err   error
	}
	get := func(path string) result {
		var conn string
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { conn = info.Conn.LocalAddr().String() }}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			return result{err: err}
		}
		resp, err := client.Do(req)
		if err != nil {
			return result{err: err}
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return result{conn: conn, proto: resp.Proto}
	}

	inFlight := make(chan result, 1)
	go func() { inFlight <- get("/hold") }()
	<-held

	suspended.Add(int64(2 * time.Minute))
	after := get("/")
	if after.err != nil {
		t.Fatalf("request after the suspend: %v", after.err)
	}
	if after.proto != "HTTP/2.0" {
		t.Fatalf("request after the suspend used %s; the test needs HTTP/2", after.proto)
	}

	close(release)
	held1 := <-inFlight
	if held1.err != nil {
		t.Fatalf("the request in flight across the suspend failed: %v", held1.err)
	}
	if after.conn == held1.conn {
		t.Errorf("the first request after the suspend reused the connection a request held across it (%s)", after.conn)
	}

	again := get("/")
	if again.err != nil {
		t.Fatalf("second request after the suspend: %v", again.err)
	}
	if again.conn != after.conn {
		t.Errorf("the second request after the suspend dialed %s instead of reusing %s", again.conn, after.conn)
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
