package jira

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

// The Jira REST client shares one org bot identity across every run of an org
// (ForSystem, by design). Once many sandboxed delegations run concurrently on a
// fleet, they all draw on that single Atlassian per-account rate-limit budget —
// a ceiling we don't control. The primitives below let a throttled request
// degrade gracefully (honor Retry-After, else back off and retry) instead of
// erroring straight into the agent. They mirror internal/github's rate-limit
// core; Jira gets its own copy rather than a shared package because the two
// clients' request cores are otherwise independent and the surface is small.

const (
	// maxRateLimitRetries bounds how many additional attempts a request gets
	// after a retryable response, on top of the initial attempt.
	maxRateLimitRetries = 3

	// maxRateLimitWait caps how long a single call will block waiting out a
	// rate limit. A Retry-After longer than this is not honored inline — the
	// throttled response is surfaced to the caller instead, so a goroutine (and,
	// on the agent path, a run slot) is never pinned for a long stretch. The cap
	// is deliberately short: the tightest consumer is the agenthost's 30s
	// per-call dispatch budget, and the poller retries the org next cycle.
	maxRateLimitWait = 30 * time.Second
)

// rateLimitBackoffBase is the first sleep in the exponential backoff used when a
// retryable response carries no usable Retry-After. It doubles per attempt and
// is capped at maxRateLimitWait. It is a var, not a const, only so tests can
// shrink it to keep the suite fast; production never reassigns it.
var rateLimitBackoffBase = 500 * time.Millisecond

// retryableStatus reports whether a response status should be retried. A 429 is
// always retryable — a throttled request was rejected, not processed, so
// replaying it is safe even for a mutation. A 5xx is retried only for an
// idempotent request: a mutation that a 5xx leaves in an unknown state must not
// be blindly replayed (it might have partially applied), so it surfaces the
// error to the caller unchanged.
func retryableStatus(status int, idempotent bool) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status >= 500 && idempotent {
		return true
	}
	return false
}

// rateLimitWait picks how long to wait before the next attempt: Jira's
// Retry-After when present and usable, else exponential backoff on the attempt
// number.
func rateLimitWait(h http.Header, attempt int) time.Duration {
	if d, ok := parseRetryAfter(h); ok {
		return d
	}
	return backoffDuration(attempt)
}

// parseRetryAfter reads the Retry-After header, which Jira sends as either
// delay-seconds or an HTTP-date (RFC 7231 §7.1.3). A value that resolves to
// zero or negative — a non-positive delay-seconds count, or an HTTP-date
// already in the past (a stale header, or clock skew) — is reported as absent
// (ok=false) so the caller falls back to backoffDuration's sane minimum rather
// than retrying with no pause at all.
func parseRetryAfter(h http.Header) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if wait := time.Until(t); wait > 0 {
			return wait, true
		}
		return 0, false
	}
	return 0, false
}

// backoffDuration is the exponential backoff used when a retryable response
// carries no Retry-After: 500ms, 1s, 2s, ... capped at maxRateLimitWait.
func backoffDuration(attempt int) time.Duration {
	d := rateLimitBackoffBase * time.Duration(1<<uint(attempt-1))
	if d > maxRateLimitWait || d <= 0 {
		return maxRateLimitWait
	}
	return d
}

// sleepCtx sleeps for d or returns ctx.Err() if ctx is done first, so every
// rate-limit wait aborts immediately on cancellation instead of blocking to the
// end of the sleep. The caller's deadline (the poller's cycle budget, or the
// agenthost's per-call dispatch budget) is therefore the ultimate authority on
// total blocking, independent of the retry cap.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// readAllClose reads resp.Body to completion and closes it.
func readAllClose(resp *http.Response) ([]byte, error) {
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return data, err
}
