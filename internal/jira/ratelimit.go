package jira

import (
	"net/http"
	"strconv"
	"strings"
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

// The rate-limit buckets reported for a Jira credential (recordBudgets).
//
// Jira Cloud meters two that a response describes. quota is the hourly points
// quota Atlassian enforces on OAuth 2.0 (3LO), Forge and Connect apps; a
// response states no count for it, only X-RateLimit-NearLimit: true once less
// than 20% of it is left. burst is the per-second request allowance for the
// endpoint, from X-RateLimit-Limit and X-RateLimit-Remaining, and is not the
// hourly quota. Jira Data Center, when an admin turns rate limiting on, meters
// each user with one token bucket, requests, which every response states.
const (
	bucketQuota    = "quota"
	bucketBurst    = "burst"
	bucketRequests = "requests"
)

// quotaWindow and burstWindow are the windows of Jira Cloud's quota and burst
// allowance. A response states the reset of neither except on a 429, so a
// reading is kept for its window after the response that carried it: by then
// the bucket has refilled, whatever the reading said.
const (
	quotaWindow = time.Hour
	burstWindow = time.Second
)

// The RateLimit-Reason values Jira Cloud documents for a 429, each naming the
// limit that refused the request. A 429 is counted under its reason
// (upstream.RecordRefused), or under reasonOther when it carries none or
// another value: developers have reported values the documentation does not
// list.
const (
	reasonQuotaGlobal   = "jira-quota-global-based"
	reasonQuotaTenant   = "jira-quota-tenant-based"
	reasonBurst         = "jira-burst-based"
	reasonPerIssueWrite = "jira-per-issue-on-write"
	reasonOther         = "other"
)

// maxLoggedReason bounds how much of an unrecognized RateLimit-Reason is
// logged.
const maxLoggedReason = 64

// recordBudgets reports the rate-limit readings a response states to the
// process-wide recorder (upstream.RecordBudget), which the rate-limit gauges
// and the low-budget WARN read, and counts a 429 under the limit that refused
// it. Called on every response, successful or not; now is when it arrived.
//
// A client that names no org or no credential reports nothing: a credential
// being validated before it is stored is no org's, a user's own API token or
// personal access token is metered per user, which a metric label may not
// name, and a run's credential-proxy client cannot say which credential the
// proxy attached.
func (c *Client) recordBudgets(status int, h http.Header, now time.Time) {
	if c.orgID == "" || c.credential == "" {
		return
	}
	if c.cfg.Deployment == DeploymentCloud {
		c.recordCloud(status, h, now)
		return
	}
	c.recordDataCenter(status, h, now)
}

// recordCloud records a Jira Cloud response's quota and burst readings, and a
// 429's reason.
//
// The quota is recorded when the response says whether it is near its limit,
// and on every response for a Cloud OAuth credential, which the quota meters:
// for it, a response without X-RateLimit-NearLimit says the quota is not near.
// An API token is not subject to the quota, so its responses record it only
// if one says so. A 429 for the quota or for burst takes its reading's reset
// from the refusal (refusalReset). Burst readings are Quiet: a busy endpoint
// can stay below 20% of its per-second allowance for many seconds in a row.
//
// TODO(TFAC-1120): read the quota's remaining and limit from Beta-RateLimit /
// Beta-RateLimit-Policy (or RateLimit / RateLimit-Policy) if a real Cloud
// response shows Atlassian still sends them.
func (c *Client) recordCloud(status int, h http.Header, now time.Time) {
	var reason string
	if status == http.StatusTooManyRequests {
		reason = c.countRefusal(h)
	}
	record := func(bucket string, b upstream.Budget) {
		upstream.RecordBudget(upstream.Jira, c.orgID, string(c.credential), bucket, b)
	}

	nearHeader := strings.TrimSpace(h.Get("X-RateLimit-NearLimit"))
	switch {
	case reason == reasonQuotaGlobal || reason == reasonQuotaTenant:
		record(bucketQuota, upstream.NearLimitReading(true, refusalReset(h, now, quotaWindow)))
	case nearHeader != "" || c.credential == AuthMethodCloudOAuth:
		record(bucketQuota, upstream.NearLimitReading(strings.EqualFold(nearHeader, "true"), now.Add(quotaWindow)))
	}

	if b, ok := readCount(h); ok {
		b.Reset = now.Add(burstWindow)
		b.Quiet = true
		if reason == reasonBurst {
			b.Reset = refusalReset(h, now, burstWindow)
		}
		record(bucketBurst, b)
	}
}

// countRefusal counts a Jira Cloud 429 under its RateLimit-Reason and returns
// the reason it was counted under. A reason outside the documented set is
// counted as reasonOther and logged, so a value Atlassian starts sending shows
// up without becoming a label.
func (c *Client) countRefusal(h http.Header) string {
	raw := strings.TrimSpace(h.Get("RateLimit-Reason"))
	reason := raw
	switch raw {
	case reasonQuotaGlobal, reasonQuotaTenant, reasonBurst, reasonPerIssueWrite:
	default:
		reason = reasonOther
		if raw != "" {
			if len(raw) > maxLoggedReason {
				raw = strings.ToValidUTF8(raw[:maxLoggedReason], "")
			}
			jiraLog.Warn("jira rate limit refused a request for a reason TF does not recognize",
				"org", c.orgID, "credential", string(c.credential), "reason", raw)
		}
	}
	upstream.RecordRefused(upstream.Jira, c.orgID, string(c.credential), reason)
	return reason
}

// recordDataCenter records a Jira Data Center response's token bucket, and
// counts a 429 against it. An instance with rate limiting off sends no
// headers, and then nothing is recorded.
//
// The reset is when the bucket is full again: the tokens missing, at
// X-RateLimit-FillRate tokens every X-RateLimit-Interval-Seconds, and at least
// one interval, so a full bucket's reading is kept until the next fill. A
// response without the fill rate or the interval leaves the reset unknown.
func (c *Client) recordDataCenter(status int, h http.Header, now time.Time) {
	if status == http.StatusTooManyRequests {
		upstream.RecordRefused(upstream.Jira, c.orgID, string(c.credential), bucketRequests)
	}
	b, ok := readCount(h)
	if !ok {
		return
	}
	fill, ferr := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-FillRate")))
	interval, ierr := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Interval-Seconds")))
	if ferr == nil && ierr == nil && fill > 0 && interval > 0 && b.Limit > 0 {
		missing := max(b.Limit-b.Remaining, 0)
		intervals := max((missing+fill-1)/fill, 1)
		b.Reset = now.Add(time.Duration(intervals*interval) * time.Second)
	}
	upstream.RecordBudget(upstream.Jira, c.orgID, string(c.credential), bucketRequests, b)
}

// readCount reads X-RateLimit-Remaining and X-RateLimit-Limit. Remaining is
// required for a reading; a limit that is absent or unparsable is left unknown.
func readCount(h http.Header) (upstream.Budget, bool) {
	remaining, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Remaining")))
	if err != nil {
		return upstream.Budget{}, false
	}
	b := upstream.Budget{Remaining: remaining}
	if limit, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Limit"))); err == nil {
		b.Limit = limit
	}
	return b, true
}

// refusalReset is when the limit that refused a 429 resets: X-RateLimit-Reset,
// an ISO 8601 time, else the response time plus Retry-After, else plus
// window.
func refusalReset(h http.Header, now time.Time, window time.Duration) time.Time {
	if t, ok := parseReset(h.Get("X-RateLimit-Reset")); ok && t.After(now) {
		return t
	}
	if d, ok := upstream.RetryAfter(h); ok {
		return now.Add(d)
	}
	return now.Add(window)
}

// parseReset parses X-RateLimit-Reset. Atlassian documents it as ISO 8601 and
// has shown it both with seconds and without (2024-06-20T13:21Z).
func parseReset(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
