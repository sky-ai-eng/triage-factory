package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestGet_RetriesOn429WithRetryAfter pins the primary acceptance scenario: a
// 429 carrying Retry-After is honored (not exponential backoff), and the
// retried GET succeeds transparently — exactly two upstream requests, no
// error surfaced to the caller.
func TestGet_RetriesOn429WithRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limited"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := clientAgainst(srv.URL)
	start := time.Now()
	data, err := c.Get(context.Background(), "/x")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("data = %q, want the second response's body", data)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream requests = %d, want exactly 2", got)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want >= ~1s (Retry-After: 1 honored)", elapsed)
	}
}

// TestGet_RetriesOn403SecondaryRateLimit pins the 403-as-rate-limit path: a
// secondary-rate-limit 403 carrying Retry-After gets the same retry
// treatment as a 429.
func TestGet_RetriesOn403SecondaryRateLimit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes."}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := clientAgainst(srv.URL)
	start := time.Now()
	data, err := c.Get(context.Background(), "/x")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("data = %q, want the second response's body", data)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream requests = %d, want exactly 2", got)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want >= ~1s (Retry-After: 1 honored)", elapsed)
	}
}

// TestGet_SecondaryRateLimitBodySniffedWithoutRetryAfter covers the body-text
// fallback: a secondary-limit 403 with no Retry-After header still retries,
// recognized by GitHub's standard message text.
func TestGet_SecondaryRateLimitBodySniffedWithoutRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit and have been temporarily blocked."}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := clientAgainst(srv.URL)
	data, err := c.Get(context.Background(), "/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("data = %q, want the second response's body", data)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream requests = %d, want exactly 2", got)
	}
}

// TestGet_Genuine403PassesThroughUnchanged pins that an ordinary permission
// 403 (no Retry-After, no secondary-rate-limit text) is NOT treated as rate
// limiting — it returns the normal *HTTPError, with its body intact, on the
// first attempt. This is what internal/tracker's 403/404-skip discrimination
// (see tracker.go discoverGitHub) relies on.
func TestGet_Genuine403PassesThroughUnchanged(t *testing.T) {
	var calls int32
	const wantBody = `{"message":"Must have admin rights to Repository."}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(wantBody))
	}))
	defer srv.Close()

	c := clientAgainst(srv.URL)
	_, err := c.Get(context.Background(), "/x")

	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusForbidden {
		t.Fatalf("err = %v, want a plain *HTTPError 403", err)
	}
	var rl *ErrRateLimited
	if errors.As(err, &rl) {
		t.Fatal("a genuine 403 must not be classified as ErrRateLimited")
	}
	if he.Body != wantBody {
		t.Errorf("Body = %q, want %q (replayed unchanged)", he.Body, wantBody)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream requests = %d, want exactly 1 (no retry for a non-rate-limit 403)", got)
	}
}

// TestGet_PreflightWaitsForKnownReset pins the remaining==0 pre-flight path:
// once a response has told the client remaining=0 with a near reset, the
// NEXT call waits for that reset before sending anything, rather than
// spending a request to discover it's still blocked.
func TestGet_PreflightWaitsForKnownReset(t *testing.T) {
	var calls int32
	reset := time.Now().Add(2 * time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", reset.Unix()))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"first":true}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"second":true}`))
	}))
	defer srv.Close()

	c := clientAgainst(srv.URL)
	if _, err := c.Get(context.Background(), "/x"); err != nil {
		t.Fatalf("seed Get: %v", err)
	}

	start := time.Now()
	data, err := c.Get(context.Background(), "/x")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if string(data) != `{"second":true}` {
		t.Errorf("data = %q, want the second response's body", data)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want it to block for roughly the ~2s reset", elapsed)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream requests = %d, want exactly 2", got)
	}
}

// TestGet_PreflightCapExceeded_ReturnsErrRateLimitedPromptly pins the yield
// behavior: when the known reset is further out than maxRateLimitWait, the
// call returns *ErrRateLimited immediately — no request sent, no long block
// — so a poll cycle can checkpoint and resume rather than tying up a
// goroutine for the full reset window.
func TestGet_PreflightCapExceeded_ReturnsErrRateLimitedPromptly(t *testing.T) {
	var calls int32
	farReset := time.Now().Add(10 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", farReset.Unix()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := clientAgainst(srv.URL)
	if _, err := c.Get(context.Background(), "/x"); err != nil {
		t.Fatalf("seed Get: %v", err)
	}

	start := time.Now()
	_, err := c.Get(context.Background(), "/x")
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want a prompt return (no long block)", elapsed)
	}
	var rl *ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *ErrRateLimited", err)
	}
	if rl.ResumeAt.IsZero() {
		t.Error("ResumeAt not populated")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream requests = %d, want exactly 1 (second call short-circuited before sending)", got)
	}
}

// TestPost_429DoesNotRetry pins the mutation side-effect-safety requirement:
// a POST that hits 429 surfaces *ErrRateLimited immediately, with exactly one
// upstream request — mutations are never silently replayed.
func TestPost_429DoesNotRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()

	c := clientAgainst(srv.URL)
	_, err := c.Post(context.Background(), "/x", map[string]any{"a": 1})

	var rl *ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *ErrRateLimited", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream requests = %d, want exactly 1 (mutations must not retry)", got)
	}
}

// TestGet_ContextCancelledDuringBackoffSleep pins ctx-awareness of the
// retry wait: cancelling mid-sleep aborts immediately with ctx.Err(), rather
// than waiting out the full Retry-After.
func TestGet_ContextCancelledDuringBackoffSleep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()

	c := clientAgainst(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.Get(ctx, "/x")
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want a prompt abort well under the 30s Retry-After", elapsed)
	}
}

// TestGet_ExhaustsRetriesThenErrRateLimited pins the retry bound: a GET that
// keeps hitting 429 gives up after maxRateLimitRetries extra attempts (one
// initial + maxRateLimitRetries retries) and returns *ErrRateLimited rather
// than retrying forever. Retry-After: 0 keeps the test fast — the bound
// under test is the attempt count, not backoff timing.
func TestGet_ExhaustsRetriesThenErrRateLimited(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()

	c := clientAgainst(srv.URL)
	_, err := c.Get(context.Background(), "/x")

	var rl *ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *ErrRateLimited after exhausting retries", err)
	}
	if got, want := atomic.LoadInt32(&calls), int32(1+maxRateLimitRetries); got != want {
		t.Errorf("upstream requests = %d, want %d (initial + %d retries)", got, want, maxRateLimitRetries)
	}
}

// TestGet_NoRateLimitHeaders_BehavesAsBefore pins the GHES-with-limits-
// disabled case: RateLimit().ok stays false forever when no response ever
// carries the headers, and a plain successful GET is completely unaffected.
func TestGet_NoRateLimitHeaders_BehavesAsBefore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := clientAgainst(srv.URL)
	if _, _, ok := c.RateLimit(); ok {
		t.Error("RateLimit ok should be false before any response is seen")
	}
	data, err := c.Get(context.Background(), "/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("data = %q", data)
	}
	if _, _, ok := c.RateLimit(); ok {
		t.Error("RateLimit ok should remain false when the server never sends rate-limit headers")
	}
}

// TestRateLimit_ReflectsMostRecentHeaders pins that RateLimit() tracks the
// latest response seen, not just the first.
func TestRateLimit_ReflectsMostRecentHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", r.URL.Query().Get("remaining"))
		w.Header().Set("X-RateLimit-Reset", r.URL.Query().Get("reset"))
		w.Header().Set("X-RateLimit-Used", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := clientAgainst(srv.URL)

	reset1 := time.Now().Add(time.Hour).Truncate(time.Second)
	if _, err := c.Get(context.Background(), fmt.Sprintf("/x?remaining=42&reset=%d", reset1.Unix())); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if remaining, reset, ok := c.RateLimit(); !ok || remaining != 42 || !reset.Equal(reset1) {
		t.Fatalf("RateLimit = (%d, %v, %v), want (42, %v, true)", remaining, reset, ok, reset1)
	}

	reset2 := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if _, err := c.Get(context.Background(), fmt.Sprintf("/x?remaining=10&reset=%d", reset2.Unix())); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if remaining, reset, ok := c.RateLimit(); !ok || remaining != 10 || !reset.Equal(reset2) {
		t.Fatalf("RateLimit = (%d, %v, %v), want (10, %v, true)", remaining, reset, ok, reset2)
	}
}

// TestPostGraphQL_RetriesOn429 pins that the GraphQL query path is retryable
// like a GET, per the ticket's "treat GraphQL reads as retryable" directive.
func TestPostGraphQL_RetriesOn429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limited"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	}))
	defer srv.Close()

	c := clientAgainst(srv.URL)
	data, err := c.PostGraphQL(context.Background(), map[string]any{"query": "{ __typename }"})
	if err != nil {
		t.Fatalf("PostGraphQL: %v", err)
	}
	if !strings.Contains(string(data), `"ok":true`) {
		t.Errorf("data = %s, want the second response's body", data)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream requests = %d, want exactly 2", got)
	}
}
