package jira

import (
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// The Jira REST client shares one org bot identity across every run of an org
// (ForSystem, by design). Once many sandboxed delegations run concurrently on a
// fleet, they all draw on that single Atlassian per-account rate-limit budget —
// a ceiling we don't control. The limits below let a throttled request degrade
// gracefully (honor Retry-After, else back off and retry) instead of erroring
// straight into the agent. The primitives they apply are internal/upstream's.

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

// rateLimitWait picks how long to wait before the next attempt: Jira's
// Retry-After when present and usable, else exponential backoff on the attempt
// number.
func rateLimitWait(h http.Header, attempt int) time.Duration {
	if d, ok := upstream.RetryAfter(h); ok {
		return d
	}
	return backoff(attempt)
}

// backoff is the exponential backoff used when a retryable response carries no
// Retry-After: 500ms, 1s, 2s, ... capped at maxRateLimitWait.
func backoff(attempt int) time.Duration {
	return upstream.Backoff(attempt, rateLimitBackoffBase, maxRateLimitWait)
}
