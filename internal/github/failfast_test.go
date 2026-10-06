package github

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

// attemptCounter counts the attempts that reach the network.
type attemptCounter struct {
	n    atomic.Int32
	base http.RoundTripper
}

func (c *attemptCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.base.RoundTrip(r)
}

// TestFailFastScope_StopsRetryingAHostThatFailed: inside a fail-fast scope,
// the first request to a host that keeps failing spends its whole retry
// ladder, and every later request to that host is sent once. A request to
// another host in the same scope, and a request outside any scope, keep
// their retries.
func TestFailFastScope_StopsRetryingAHostThatFailed(t *testing.T) {
	SetTransientBackoffForTest(t, time.Millisecond)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	refused := "http://" + ln.Addr().String()
	_ = ln.Close()

	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"Service Unavailable"}`))
	}))
	t.Cleanup(srv.Close)

	rt := &attemptCounter{base: &http.Transport{}}
	c := &Client{baseURL: refused, pat: "t", http: &http.Client{Transport: rt}}
	ctx := upstream.WithFailFast(context.Background())

	if _, err := c.Get(ctx, "/repos/o/a/pulls"); err == nil {
		t.Fatal("a refused host answered")
	}
	if got := rt.n.Load(); got != 1+maxRateLimitRetries {
		t.Fatalf("the first request to a refused host made %d attempts, want the full ladder of %d", got, 1+maxRateLimitRetries)
	}
	for _, repo := range []string{"b", "c", "d"} {
		_, _ = c.Get(ctx, "/repos/o/"+repo+"/pulls")
	}
	if got := rt.n.Load() - (1 + maxRateLimitRetries); got != 3 {
		t.Errorf("three later requests to the refused host made %d attempts, want one each", got)
	}

	other := clientAgainst(srv.URL)
	if _, err := other.Get(ctx, "/x"); err == nil {
		t.Fatal("a 503 answered")
	}
	if got := served.Load(); got != 1+maxRateLimitRetries {
		t.Errorf("a request to another host in the same scope made %d attempts, want %d", got, 1+maxRateLimitRetries)
	}

	before := rt.n.Load()
	_, _ = c.Get(context.Background(), "/repos/o/f/pulls")
	if got := rt.n.Load() - before; got != 1+maxRateLimitRetries {
		t.Errorf("a request outside the scope made %d attempts, want %d", got, 1+maxRateLimitRetries)
	}
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

// TestFailFastScope_StopsSendingToAHostThatTimedOutTwice: inside a fail-fast
// scope, once two requests to a host have timed out with no answer from it
// between, later requests to that host are not sent, so a cycle over many
// repos does not wait out the client's timeout once per repo. The skipped
// request still fails as a transport failure, so a caller handles it as it
// would the timeout. Another host in the same scope, and the same host
// outside any scope, are still sent to.
func TestFailFastScope_StopsSendingToAHostThatTimedOutTwice(t *testing.T) {
	const timeout = 200 * time.Millisecond
	rt := &attemptCounter{base: &http.Transport{}}
	c := &Client{baseURL: "http://" + hungListener(t), pat: "t", http: &http.Client{Timeout: timeout, Transport: rt}}
	ctx := upstream.WithFailFast(context.Background())

	if _, err := c.Get(ctx, "/repos/o/a/pulls"); err == nil {
		t.Fatal("a host that never answers answered")
	}
	if got := rt.n.Load(); got != 1 {
		t.Fatalf("the request that timed out made %d attempts, want 1", got)
	}
	if _, err := c.Get(ctx, "/repos/o/b/pulls"); err == nil {
		t.Fatal("a host that never answers answered")
	}
	if got := rt.n.Load(); got != 2 {
		t.Fatalf("the request after one timeout made %d attempts, want 1", got-1)
	}

	start := time.Now()
	for _, repo := range []string{"c", "d", "e"} {
		_, err := c.Get(ctx, "/repos/o/"+repo+"/pulls")
		if class, ok := upstream.ClassOf(err); !ok || class != upstream.Transient {
			t.Errorf("a request not sent to a silent host returned %v (class %q), want a transient failure", err, class)
		}
	}
	if got := rt.n.Load() - 2; got != 0 {
		t.Errorf("three later requests to the host that timed out made %d attempts, want none", got)
	}
	if elapsed := time.Since(start); elapsed >= timeout {
		t.Errorf("three later requests took %v, waiting out the timeout again", elapsed)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	if _, err := clientAgainst(srv.URL).Get(ctx, "/x"); err != nil {
		t.Errorf("a request to another host in the same scope failed: %v", err)
	}

	before := rt.n.Load()
	_, _ = c.Get(context.Background(), "/repos/o/f/pulls")
	if got := rt.n.Load() - before; got != 1 {
		t.Errorf("a request outside the scope made %d attempts, want 1", got)
	}
}

// TestFailFastScope_OneSlowRequestDoesNotStopTheRest: a request that
// outlasts the client's timeout on a host that answers everything else does
// not stop the scope sending to that host. A poll cycle sends one request at
// a time, so a single heavy read that always times out would otherwise end
// every cycle at that read.
func TestFailFastScope_OneSlowRequestDoesNotStopTheRest(t *testing.T) {
	const timeout = 200 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/heavy/pulls" {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	rt := &attemptCounter{base: &http.Transport{}}
	c := &Client{baseURL: srv.URL, pat: "t", http: &http.Client{Timeout: timeout, Transport: rt}}
	ctx := upstream.WithFailFast(context.Background())

	if _, err := c.Get(ctx, "/repos/o/heavy/pulls"); err == nil {
		t.Fatal("the heavy read answered inside the timeout")
	}
	before := rt.n.Load()
	for _, repo := range []string{"a", "b", "c"} {
		if _, err := c.Get(ctx, "/repos/o/"+repo+"/pulls"); err != nil {
			t.Errorf("a request after one slow read failed: %v", err)
		}
	}
	if got := rt.n.Load() - before; got != 3 {
		t.Errorf("three requests after one slow read made %d attempts, want 3", got)
	}
}

// TestFailFastScope_NoRetryAfterWait: once a host is unreachable in the
// scope, a 503 asking for a Retry-After the client would otherwise honor is
// returned at once.
func TestFailFastScope_NoRetryAfterWait(t *testing.T) {
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	ctx := upstream.WithFailFast(context.Background())
	upstream.MarkUnreachable(ctx, u.Host)

	start := time.Now()
	_, err := clientAgainst(srv.URL).GetRaw(ctx, "/x", "application/json")
	if err == nil {
		t.Fatal("a 503 answered")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the request took %v, waiting out a Retry-After against an unreachable host", elapsed)
	}
	if got := served.Load(); got != 1 {
		t.Errorf("made %d attempts, want 1", got)
	}
}

// TestFailFastScope_TruncatedBodyMarksUnreachable: inside a fail-fast scope,
// a response that breaks off mid-body marks its host unreachable, so a later
// GET whose body also breaks off gets one attempt. The same GET outside any
// scope keeps its retries.
func TestFailFastScope_TruncatedBodyMarksUnreachable(t *testing.T) {
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		truncated(w, http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c := clientAgainst(srv.URL)
	ctx := upstream.WithFailFast(context.Background())

	if _, err := c.Post(ctx, "/repos/o/r/issues/1/comments", map[string]any{"body": "x"}); err == nil {
		t.Fatal("a truncated response answered")
	}
	u, _ := url.Parse(srv.URL)
	if !upstream.Unreachable(ctx, u.Host) {
		t.Fatal("a truncated response did not mark the host unreachable")
	}
	before := served.Load()
	if _, err := c.Get(ctx, "/x"); err == nil {
		t.Fatal("a truncated response answered")
	}
	if got := served.Load() - before; got != 1 {
		t.Errorf("a later GET made %d attempts, want 1", got)
	}

	before = served.Load()
	_, _ = c.Get(context.Background(), "/x")
	if got := served.Load() - before; got != 1+maxRateLimitRetries {
		t.Errorf("a GET outside the scope made %d attempts, want %d", got, 1+maxRateLimitRetries)
	}
}
