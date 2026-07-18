package jira

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// shortBackoff shrinks the exponential-backoff base for the duration of a test
// so retry paths don't sleep for real seconds. Restored on cleanup.
func shortBackoff(t *testing.T) {
	t.Helper()
	prev := rateLimitBackoffBase
	rateLimitBackoffBase = time.Millisecond
	t.Cleanup(func() { rateLimitBackoffBase = prev })
}

// retryServer replies with each status in codes in order (one per request),
// then 200 for every request after the list is exhausted. It counts requests.
func retryServer(t *testing.T, header http.Header, codes ...int) (*httptest.Server, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := atomic.AddInt32(&n, 1)
		w.Header().Set("Content-Type", "application/json")
		if int(i) <= len(codes) {
			for k, vs := range header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(codes[i-1])
			_, _ = w.Write([]byte(`{"error":"throttled"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func testClient(url string) *Client {
	return NewClient(DataCenterPAT(url, "tok"))
}

func TestDoRequest_RetriesGetOn429ThenSucceeds(t *testing.T) {
	shortBackoff(t)
	srv, n := retryServer(t, nil, 429, 429)
	c := testClient(srv.URL)

	body, err := c.get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %s, want ok", body)
	}
	if got := atomic.LoadInt32(n); got != 3 {
		t.Errorf("attempts = %d, want 3 (2 retries + success)", got)
	}
}

func TestDoRequest_RetriesGetOn5xxThenSucceeds(t *testing.T) {
	shortBackoff(t)
	srv, n := retryServer(t, nil, 503, 500)
	c := testClient(srv.URL)

	if _, err := c.get(context.Background(), srv.URL); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := atomic.LoadInt32(n); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestDoRequest_MutationNotRetriedOn5xx(t *testing.T) {
	shortBackoff(t)
	srv, n := retryServer(t, nil, 500, 500)
	c := testClient(srv.URL)

	// post is a mutation: a 5xx must NOT be replayed (it might have applied).
	err := c.post(context.Background(), srv.URL, map[string]string{"k": "v"})
	if err == nil {
		t.Fatal("post: want error on 500, got nil")
	}
	if got := atomic.LoadInt32(n); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry for a mutation 5xx)", got)
	}
}

func TestDoRequest_MutationRetriedOn429(t *testing.T) {
	shortBackoff(t)
	srv, n := retryServer(t, nil, 429)
	c := testClient(srv.URL)

	// A 429 is safe to replay even for a mutation: the request was throttled,
	// not processed, so retrying can't double the side effect.
	if err := c.post(context.Background(), srv.URL, map[string]string{"k": "v"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if got := atomic.LoadInt32(n); got != 2 {
		t.Errorf("attempts = %d, want 2 (429 retried then success)", got)
	}
}

func TestDoRequest_ExhaustsRetriesAndSurfacesError(t *testing.T) {
	shortBackoff(t)
	// Always 429: more failures than the retry budget.
	srv, n := retryServer(t, nil, 429, 429, 429, 429, 429, 429)
	c := testClient(srv.URL)

	_, err := c.get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("get: want error after exhausting retries")
	}
	if got := atomic.LoadInt32(n); got != 1+maxRateLimitRetries {
		t.Errorf("attempts = %d, want %d (initial + %d retries)", got, 1+maxRateLimitRetries, maxRateLimitRetries)
	}
}

func TestDoRequest_HonorsRetryAfterOverCapSurfacesImmediately(t *testing.T) {
	shortBackoff(t)
	h := http.Header{}
	h.Set("Retry-After", "3600") // 1h, far over maxRateLimitWait
	srv, n := retryServer(t, h, 429, 429)
	c := testClient(srv.URL)

	start := time.Now()
	_, err := c.get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("get: want error, got nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("blocked %v; an over-cap Retry-After must surface immediately", elapsed)
	}
	if got := atomic.LoadInt32(n); got != 1 {
		t.Errorf("attempts = %d, want 1 (no inline wait for an over-cap Retry-After)", got)
	}
}

func TestDoRequest_ContextCancelDuringBackoff(t *testing.T) {
	// Backoff long enough that cancellation lands mid-wait.
	prev := rateLimitBackoffBase
	rateLimitBackoffBase = 200 * time.Millisecond
	t.Cleanup(func() { rateLimitBackoffBase = prev })

	srv, _ := retryServer(t, nil, 429, 429, 429, 429)
	c := testClient(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := c.get(ctx, srv.URL)
	if err == nil {
		t.Fatal("get: want ctx error, got nil")
	}
}

// TestSearchIssues_RetriesTransient5xx pins the poller's hot read path: a JQL
// search is a read-shaped POST, so a transient 5xx is retried rather than
// failing the whole org poll cycle.
func TestSearchIssues_RetriesTransient5xx(t *testing.T) {
	shortBackoff(t)
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"issues":[{"key":"SKY-1"}]}`))
	}))
	t.Cleanup(srv.Close)

	c := testClient(srv.URL)
	issues, err := c.SearchIssues(context.Background(), "project = SKY", nil, 10)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Key != "SKY-1" {
		t.Fatalf("issues = %+v, want one SKY-1", issues)
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Errorf("attempts = %d, want 2 (503 retried then success)", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		wantOK bool
		approx time.Duration
	}{
		{"absent", "", false, 0},
		{"seconds", "5", true, 5 * time.Second},
		{"zero", "0", false, 0},
		{"negative", "-3", false, 0},
		{"http-date-future", time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat), true, 10 * time.Second},
		{"http-date-past", time.Now().Add(-10 * time.Second).UTC().Format(http.TimeFormat), false, 0},
		{"garbage", "soon", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.value != "" {
				h.Set("Retry-After", tc.value)
			}
			d, ok := parseRetryAfter(h)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK {
				// Allow a couple seconds of slack for HTTP-date rounding.
				if d < tc.approx-2*time.Second || d > tc.approx+time.Second {
					t.Errorf("duration = %v, want ~%v", d, tc.approx)
				}
			}
		})
	}
}

func TestRetryableStatus(t *testing.T) {
	cases := []struct {
		status     int
		idempotent bool
		want       bool
	}{
		{http.StatusTooManyRequests, false, true},
		{http.StatusTooManyRequests, true, true},
		{http.StatusInternalServerError, true, true},
		{http.StatusServiceUnavailable, true, true},
		{http.StatusInternalServerError, false, false},
		{http.StatusBadRequest, true, false},
		{http.StatusOK, true, false},
		{http.StatusNotFound, false, false},
	}
	for _, tc := range cases {
		if got := retryableStatus(tc.status, tc.idempotent); got != tc.want {
			t.Errorf("retryableStatus(%d, idempotent=%v) = %v, want %v", tc.status, tc.idempotent, got, tc.want)
		}
	}
}

func TestBackoffDuration(t *testing.T) {
	prev := rateLimitBackoffBase
	rateLimitBackoffBase = 500 * time.Millisecond
	t.Cleanup(func() { rateLimitBackoffBase = prev })

	if got := backoffDuration(1); got != 500*time.Millisecond {
		t.Errorf("attempt 1 = %v, want 500ms", got)
	}
	if got := backoffDuration(2); got != time.Second {
		t.Errorf("attempt 2 = %v, want 1s", got)
	}
	if got := backoffDuration(3); got != 2*time.Second {
		t.Errorf("attempt 3 = %v, want 2s", got)
	}
	// Large attempt saturates at the cap, never overflows negative.
	if got := backoffDuration(40); got != maxRateLimitWait {
		t.Errorf("attempt 40 = %v, want cap %v", got, maxRateLimitWait)
	}
}
