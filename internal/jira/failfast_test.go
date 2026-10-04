package jira

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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
