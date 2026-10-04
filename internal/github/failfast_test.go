package github

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	_, _ = c.Get(context.Background(), "/repos/o/e/pulls")
	if got := rt.n.Load() - before; got != 1+maxRateLimitRetries {
		t.Errorf("a request outside the scope made %d attempts, want %d", got, 1+maxRateLimitRetries)
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
