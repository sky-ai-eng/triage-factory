package jira

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// TestFailFastScope_StopsRetryingAHostThatFailed: inside a fail-fast scope,
// the first request to a Jira that keeps failing spends its whole retry
// ladder, and every later request to it is sent once, with no Retry-After
// wait. A request outside any scope keeps its retries.
func TestFailFastScope_StopsRetryingAHostThatFailed(t *testing.T) {
	shortBackoff(t)
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"errorMessages":["Service Unavailable"]}`))
	}))
	t.Cleanup(srv.Close)
	c := testClient(srv.URL)
	ctx := upstream.WithFailFast(context.Background())

	if _, err := c.get(ctx, srv.URL+"/rest/api/2/myself"); err == nil {
		t.Fatal("a 503 answered")
	}
	if got := served.Load(); got != 1+maxRateLimitRetries {
		t.Fatalf("the first request made %d attempts, want the full ladder of %d", got, 1+maxRateLimitRetries)
	}
	for range 3 {
		_, _ = c.get(ctx, srv.URL+"/rest/api/2/myself")
	}
	if got := served.Load() - (1 + maxRateLimitRetries); got != 3 {
		t.Errorf("three later requests made %d attempts, want one each", got)
	}

	before := served.Load()
	_, _ = c.get(context.Background(), srv.URL+"/rest/api/2/myself")
	if got := served.Load() - before; got != 1+maxRateLimitRetries {
		t.Errorf("a request outside the scope made %d attempts, want %d", got, 1+maxRateLimitRetries)
	}
}

// TestFailFastScope_NoRetryAfterWait: once Jira is unreachable in the scope,
// a 429 asking for a wait the client would otherwise honor is returned at
// once.
func TestFailFastScope_NoRetryAfterWait(t *testing.T) {
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	ctx := upstream.WithFailFast(context.Background())
	upstream.MarkUnreachable(ctx, u.Host)

	start := time.Now()
	if _, err := testClient(srv.URL).get(ctx, srv.URL+"/rest/api/2/myself"); err == nil {
		t.Fatal("a 429 answered")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the request took %v, waiting out a Retry-After against an unreachable host", elapsed)
	}
	if got := served.Load(); got != 1 {
		t.Errorf("made %d attempts, want 1", got)
	}
}

// attemptCounter counts the attempts that reach the network.
type attemptCounter struct {
	n    atomic.Int32
	base http.RoundTripper
}

func (c *attemptCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.base.RoundTrip(r)
}

// hungListener accepts connections and never answers them, the way a host
// behind a firewall that drops traffic, or a proxy that stopped forwarding,
// looks to a client. It returns the listener's address.
func hungListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// TestFailFastScope_StopsSendingToAHostThatTimedOut: inside a fail-fast
// scope, once a request to a Jira times out, later requests to it are not
// sent, so a cycle over many projects does not wait out the client's timeout
// once per request. The skipped request still fails as a transport failure.
// The same host outside any scope is still sent to.
func TestFailFastScope_StopsSendingToAHostThatTimedOut(t *testing.T) {
	const timeout = 200 * time.Millisecond
	base := "http://" + hungListener(t)
	rt := &attemptCounter{base: &http.Transport{}}
	c := testClient(base)
	c.http = &http.Client{Timeout: timeout, Transport: rt}
	ctx := upstream.WithFailFast(context.Background())

	if _, err := c.get(ctx, base+"/rest/api/2/myself"); err == nil {
		t.Fatal("a host that never answers answered")
	}
	if got := rt.n.Load(); got != 1 {
		t.Fatalf("the request that timed out made %d attempts, want 1", got)
	}

	start := time.Now()
	for range 3 {
		_, err := c.get(ctx, base+"/rest/api/2/search")
		if class, ok := upstream.ClassOf(err); !ok || class != upstream.Transient {
			t.Errorf("a request not sent to a silent host returned %v (class %q), want a transient failure", err, class)
		}
	}
	if got := rt.n.Load() - 1; got != 0 {
		t.Errorf("three later requests to the host that timed out made %d attempts, want none", got)
	}
	if elapsed := time.Since(start); elapsed >= timeout {
		t.Errorf("three later requests took %v, waiting out the timeout again", elapsed)
	}

	before := rt.n.Load()
	_, _ = c.get(context.Background(), base+"/rest/api/2/myself")
	if got := rt.n.Load() - before; got != 1 {
		t.Errorf("a request outside the scope made %d attempts, want 1", got)
	}
}
