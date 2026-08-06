package github

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

const (
	// maxRateLimitRetries bounds how many additional attempts an idempotent
	// request (GET, or the GraphQL query POST) gets after hitting a 429 or a
	// secondary-limit 403, on top of the initial attempt.
	maxRateLimitRetries = 3

	// maxRateLimitWait caps how long a single call will sleep for a rate-limit
	// reset — via Retry-After, exponential backoff, or a known
	// remaining-budget reset — before giving up. Beyond this, the caller gets
	// ErrRateLimited instead of a goroutine blocked for a potentially long
	// stretch; the poller/tracker can checkpoint and resume the cycle later.
	maxRateLimitWait = 5 * time.Minute

	// rateLimitBackoffBase is the first sleep in the exponential backoff used
	// when a 429/secondary-403 carries no Retry-After header. It doubles per
	// retry and is capped at maxRateLimitWait.
	rateLimitBackoffBase = 1 * time.Second
)

// ErrRateLimited is returned when a GitHub rate-limit budget is exhausted:
// retries for a transient 429/secondary-403 ran out, or a known
// remaining-budget reset wouldn't land within maxRateLimitWait. It lets
// internal/poller / internal/tracker distinguish "stop and resume later" from
// a genuine API failure — the poller-side resume logic is a separate ticket
// (TFAC-569's parent epic), this type is only the signal.
type ErrRateLimited struct {
	ResumeAt time.Time
}

func (e *ErrRateLimited) Error() string {
	return fmt.Sprintf("github: rate limit budget exhausted, resume at %s", e.ResumeAt.Format(time.RFC3339))
}

// RateLimit returns the primary rate-limit budget from the most recent
// response that carried x-ratelimit-* headers. ok is false until the first
// such response is seen — a GHES host with rate limiting disabled never sets
// it, and callers should treat that as unlimited rather than exhausted.
func (c *Client) RateLimit() (remaining int, reset time.Time, ok bool) {
	c.rlMu.Lock()
	defer c.rlMu.Unlock()
	return c.rlRemaining, c.rlReset, c.rlOK
}

// recordRateLimit updates the client's rate-limit state from a response's
// headers. Called on every response the request core sees, successful or
// not. x-ratelimit-remaining and x-ratelimit-reset are required for the
// state to become valid (ok=true); x-ratelimit-used is stored best-effort
// alongside them. Missing or unparsable headers leave the prior state
// untouched — GHES may omit them entirely when rate limiting is disabled.
func (c *Client) recordRateLimit(h http.Header) {
	remStr := h.Get("X-RateLimit-Remaining")
	resetStr := h.Get("X-RateLimit-Reset")
	if remStr == "" || resetStr == "" {
		return
	}
	remaining, err := strconv.Atoi(remStr)
	if err != nil {
		return
	}
	resetUnix, err := strconv.ParseInt(resetStr, 10, 64)
	if err != nil {
		return
	}
	reset := time.Unix(resetUnix, 0)
	used, _ := strconv.Atoi(h.Get("X-RateLimit-Used"))

	c.rlMu.Lock()
	c.rlRemaining = remaining
	c.rlReset = reset
	c.rlUsed = used
	c.rlOK = true
	observer := c.rlObserver
	c.rlMu.Unlock()

	// Invoked outside the lock: the observer (the resolver's registry
	// write) takes its own lock, and calling out to arbitrary caller code
	// while holding rlMu is an avoidable lock-ordering risk.
	if observer != nil {
		observer(RateLimitState{Remaining: remaining, Reset: reset, Used: used})
	}
}

// SetRateLimitObserver registers fn to be invoked every time this client's
// cached rate-limit state updates from a response's headers (see
// recordRateLimit) — the hook the resolver uses to mirror a short-lived,
// per-resolve *Client's state into its process-wide, per-org
// RateLimitRegistry (GET /readyz's soft rate-limit signal, TFAC-573).
// Optional; a *Client with no observer set is unaffected.
func (c *Client) SetRateLimitObserver(fn func(RateLimitState)) {
	c.rlMu.Lock()
	c.rlObserver = fn
	c.rlMu.Unlock()
}

// awaitBudget blocks until there is likely budget for an idempotent request,
// using the most recently observed remaining/reset. It is a pre-flight
// check only — it never talks to GitHub — so a cycle that's already known to
// be out of budget doesn't spend a request finding that out again. Mutations
// never call this: see doMutation.
func (c *Client) awaitBudget(ctx context.Context) error {
	remaining, reset, ok := c.RateLimit()
	if !ok || remaining > 0 {
		return nil
	}
	wait := time.Until(reset)
	if wait <= 0 {
		return nil
	}
	// Only past here does the call do anything — block, or refuse. The
	// returns above are the overwhelming majority and get no span, so a
	// healthy client's traces aren't mostly no-ops.
	ctx, span := tracer.Start(ctx, "github.ratelimit.await")
	defer span.End()

	if wait > maxRateLimitWait {
		span.SetAttributes(telemetry.Outcome("refused"))
		return &ErrRateLimited{ResumeAt: reset}
	}
	if err := sleepCtx(ctx, wait); err != nil {
		span.SetAttributes(telemetry.Outcome("cancelled"))
		return err
	}
	span.SetAttributes(telemetry.Outcome("waited"))
	return nil
}

// awaitRetry blocks out one backoff between attempts under its own span.
//
// These sleeps are why a GitHub call can take five minutes with no slow
// request in it: untraced, the caller's span just takes minutes while
// every transport span inside it is fast. The attempt number separates
// "one long wait" from "five short ones". Neither outcome is an error
// status — waiting out a rate limit is the client working, and a
// cancelled wait is the caller leaving.
func awaitRetry(ctx context.Context, attempt int, wait time.Duration) error {
	ctx, span := tracer.Start(ctx, "github.ratelimit.backoff",
		trace.WithAttributes(telemetry.Attempt(attempt)))
	defer span.End()

	if err := sleepCtx(ctx, wait); err != nil {
		span.SetAttributes(telemetry.Outcome("cancelled"))
		return err
	}
	span.SetAttributes(telemetry.Outcome("waited"))
	return nil
}

// reqBuilder constructs a fresh *http.Request for one attempt. It's a
// closure rather than a pre-built *http.Request because a retried request
// needs its own body reader — http.Request bodies aren't safely reusable
// across attempts.
type reqBuilder func() (*http.Request, error)

// doIdempotent sends the request(s) built by build over c.http, retrying a
// 429 or a rate-limited 403 (primary budget exhausted, or secondary/abuse)
// per Retry-After or the primary reset (or exponential backoff when neither
// is present) up to maxRateLimitRetries extra attempts. See doWithRetry.
func (c *Client) doIdempotent(ctx context.Context, build reqBuilder) (*http.Response, error) {
	return c.doWithRetry(ctx, c.http, true, build)
}

// doMutation sends a single, non-retried request over c.http. A mutation
// (POST/PUT/PATCH/DELETE that changes state) must never be silently
// replayed — a retried mutation could double the side effect — so a
// rate-limited 429/403 response short-circuits straight into ErrRateLimited
// instead of being retried. See doWithRetry.
func (c *Client) doMutation(ctx context.Context, build reqBuilder) (*http.Response, error) {
	return c.doWithRetry(ctx, c.http, false, build)
}

// doWithRetry is the shared rate-limit-aware request loop behind every
// request-core method (request, GetConditional, PostGraphQL, DownloadArtifact
// — the last supplies its own hc with an extended timeout, everything else
// passes c.http). For idempotent calls it pre-flights a known exhausted
// budget (awaitBudget) and retries a 429 or rate-limited 403 (primary budget
// exhausted via x-ratelimit-remaining: 0, or secondary/abuse) up to
// maxRateLimitRetries extra attempts — honoring Retry-After when present,
// else the primary reset time when that's what triggered it, else
// exponential backoff; every sleep is ctx-aware. Mutations get exactly one
// attempt: a rate-limited 429/403 returns ErrRateLimited immediately rather
// than retrying.
//
// A non-rate-limit response (any 2xx, or a 403/429 that doesn't look like
// rate limiting) is returned to the caller with its body untouched and still
// open, so callers that stream (DownloadArtifact) or need the raw status
// (GetConditional's 304) keep their existing post-processing unchanged. Only
// the rate-limit-candidate branch reads the (small, JSON) error body itself;
// when that turns out to be a genuine 403 rather than rate limiting, the
// already-drained body is replayed onto the response before returning it.
func (c *Client) doWithRetry(ctx context.Context, hc *http.Client, idempotent bool, build reqBuilder) (*http.Response, error) {
	if idempotent {
		if err := c.awaitBudget(ctx); err != nil {
			return nil, err
		}
	}

	maxAttempts := 1
	if idempotent {
		maxAttempts = 1 + maxRateLimitRetries
	}

	for attempt := 1; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		c.recordRateLimit(resp.Header)

		if !rateLimitCandidate(resp.StatusCode) {
			return resp, nil
		}

		retryAfter, hasRetryAfter := parseRetryAfter(resp.Header)
		primaryReset, primaryExhausted := primaryBudgetExhausted(resp.Header)
		data, readErr := readAllClose(resp)
		if readErr != nil {
			return nil, readErr
		}

		if resp.StatusCode == http.StatusForbidden && !hasRetryAfter && !primaryExhausted && !isSecondaryRateLimitBody(data) {
			// A genuine 403 (auth/permissions), not rate limiting — replay the
			// already-drained body so the caller's normal handling sees it.
			resp.Body = newBodyReader(data)
			return resp, nil
		}

		wait := retryAfter
		switch {
		case hasRetryAfter:
			// wait already set.
		case primaryExhausted && !primaryReset.IsZero() && time.Until(primaryReset) > 0:
			// GitHub told us exactly when the primary budget resets — use it
			// instead of a blind guess.
			wait = time.Until(primaryReset)
		default:
			wait = backoffDuration(attempt)
		}

		if !idempotent || wait > maxRateLimitWait || attempt >= maxAttempts {
			return nil, &ErrRateLimited{ResumeAt: time.Now().Add(wait)}
		}
		if err := awaitRetry(ctx, attempt, wait); err != nil {
			return nil, err
		}
	}
}

// rateLimitCandidate reports whether status is one GitHub might use to
// signal rate limiting — 429 always, 403 sometimes (both the classic primary
// budget exhaustion and secondary/abuse limits). A 403 candidate is only
// actually treated as rate limiting once its Retry-After header, its
// x-ratelimit-remaining: 0 header, or its body text confirms it (see
// doWithRetry).
func rateLimitCandidate(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusForbidden
}

// primaryBudgetExhausted reports whether resp signals the primary rate-limit
// budget is exhausted via x-ratelimit-remaining: 0 — GitHub's classic
// response to primary-limit exhaustion is a 403 with this header set and no
// Retry-After, so this is the only way to recognize it (as distinct from an
// ordinary permissions 403). reset is the x-ratelimit-reset value when
// present and parsable; it's the zero Time otherwise, in which case the
// caller falls back to exponential backoff instead of a computed wait.
func primaryBudgetExhausted(h http.Header) (reset time.Time, exhausted bool) {
	if h.Get("X-RateLimit-Remaining") != "0" {
		return time.Time{}, false
	}
	resetUnix, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return time.Time{}, true
	}
	return time.Unix(resetUnix, 0), true
}

// isSecondaryRateLimitBody reports whether body looks like one of GitHub's
// secondary-rate-limit / abuse-detection error messages, for a 403 that
// carries no Retry-After header and no x-ratelimit-remaining: 0.
func isSecondaryRateLimitBody(body []byte) bool {
	b := bytes.ToLower(body)
	return bytes.Contains(b, []byte("secondary rate limit")) || bytes.Contains(b, []byte("abuse detection mechanism"))
}

// parseRetryAfter reads the Retry-After header, which GitHub sends as either
// delay-seconds or an HTTP-date (RFC 7231 §7.1.3). A value that resolves to
// zero or negative — a non-positive delay-seconds count, or an HTTP-date
// that's already past (a stale/expired header, or clock skew between us and
// GitHub) — is reported as absent (ok=false) rather than honored as "wait
// zero", so the caller falls back to backoffDuration's sane minimum instead
// of retrying with no pause at all.
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

// backoffDuration is the exponential backoff used when a rate-limited
// response carries no Retry-After: 1s, 2s, 4s, ... capped at maxRateLimitWait.
func backoffDuration(attempt int) time.Duration {
	d := rateLimitBackoffBase * time.Duration(1<<uint(attempt-1))
	if d > maxRateLimitWait || d <= 0 {
		return maxRateLimitWait
	}
	return d
}

// sleepCtx sleeps for d or returns ctx.Err() if ctx is done first, so every
// rate-limit wait aborts immediately on cancellation instead of blocking to
// the end of the sleep.
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

// readAllClose reads resp.Body to completion and closes it, for the small
// JSON error bodies a rate-limit-candidate response carries.
func readAllClose(resp *http.Response) ([]byte, error) {
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	return data, err
}

// newBodyReader wraps already-read bytes as a Response.Body replacement, so
// a response whose body doWithRetry drained to inspect can still be read
// normally by the caller.
func newBodyReader(data []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(data))
}
