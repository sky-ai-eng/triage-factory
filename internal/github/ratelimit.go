package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
	"go.opentelemetry.io/otel/trace"
)

const (
	// maxRateLimitRetries bounds how many additional attempts an idempotent
	// request (GET, or the GraphQL query POST) gets after a rate limit or a
	// transient failure, on top of the initial attempt. Both causes draw on
	// the same budget, so one call never makes more than
	// 1+maxRateLimitRetries attempts.
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

	// transientBackoffMax caps the wait before retrying a transient failure,
	// and is the longest Retry-After on a 5xx that is waited out; a longer
	// one returns the response instead of retrying before the server asked.
	// A transient failure carries no reset time to wait for, so the cap is
	// the poll-cycle scale, not the rate-limit one.
	transientBackoffMax = 30 * time.Second
)

// transientBackoffBase is the first sleep before retrying a dropped
// connection, or a 5xx or a 408 that carries no Retry-After: 1s, 2s, 4s. It
// is a var, not a const, only so tests can shrink it to keep the suite fast;
// production never reassigns it.
var transientBackoffBase = 1 * time.Second

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

// UpstreamClass implements upstream.Classified.
func (e *ErrRateLimited) UpstreamClass() upstream.Class { return upstream.RateLimited }

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
	if err := upstream.Sleep(ctx, wait); err != nil {
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
// "one long wait" from "five short ones", and the disposition separates a
// rate limit from a transient failure. Neither outcome is an error
// status — waiting out a rate limit or a 503 is the client working, and a
// cancelled wait is the caller leaving.
func awaitRetry(ctx context.Context, attempt int, class upstream.Class, wait time.Duration) error {
	ctx, span := tracer.Start(ctx, "github.ratelimit.backoff",
		trace.WithAttributes(telemetry.Attempt(attempt), telemetry.Disposition(string(class))))
	defer span.End()

	if err := upstream.Sleep(ctx, wait); err != nil {
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
// rate limit (a 429, or a 403 that signals an exhausted primary budget or a
// secondary/abuse limit) or a transient failure up to maxRateLimitRetries extra
// attempts. See doWithRetry.
func (c *Client) doIdempotent(ctx context.Context, build reqBuilder) (*http.Response, error) {
	return c.doWithRetry(ctx, c.http, true, build)
}

// doMutation sends a single, non-retried request over c.http. A mutation
// (POST/PUT/PATCH/DELETE that changes state) must never be silently
// replayed — a retried mutation could double the side effect — so a
// rate-limited 429/403 response short-circuits straight into ErrRateLimited
// and a transient failure is returned as-is. See doWithRetry.
func (c *Client) doMutation(ctx context.Context, build reqBuilder) (*http.Response, error) {
	return c.doWithRetry(ctx, c.http, false, build)
}

// doWithRetry is the shared request loop behind every request-core method
// (request, GetConditional, postGraphQL, DownloadArtifact — the last supplies
// its own hc with an extended timeout, everything else passes c.http). Every
// attempt is classified and counted (upstream.Record) against the client's
// org.
//
// For idempotent calls it pre-flights a known exhausted budget (awaitBudget)
// and retries up to maxRateLimitRetries extra attempts:
//   - a rate limit — a 429, or a 403 carrying Retry-After,
//     x-ratelimit-remaining: 0, or a secondary-limit body — honoring
//     Retry-After when present, else the primary reset time when that's what
//     triggered it, else exponential backoff;
//   - a transient failure — a dropped connection, a 5xx or a 408 — after the
//     response's Retry-After when it has one, else transient backoff. A
//     transient failure that another attempt would only repeat is returned
//     at once: a 403 whose body is not JSON (a proxy in front of GHES), a
//     TLS failure, or a timeout (upstream.RetryableResponse,
//     upstream.RetryableTransport).
//
// Every sleep is ctx-aware. Mutations get exactly one attempt: a rate limit
// returns ErrRateLimited immediately, and a transient failure is returned to
// the caller unchanged.
//
// Under a fail-fast scope (upstream.WithFailFast), a request that ends in a
// transient failure marks its host unreachable, and every later request to
// that host gets one attempt the same way a mutation does. A request that
// timed out counts toward its host's silence, and a later request to a silent
// host is not sent (upstream.Silent).
//
// Any response that isn't retried is returned to the caller. A success keeps
// its body untouched and still open, so callers that stream
// (DownloadArtifact) or need the raw status (GetConditional's 304) keep their
// own post-processing. An error response's body has already been read,
// capped at upstream.MaxErrorBody, to classify it, and is replayed onto the
// response for the caller.
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
		host := req.URL.Host
		if upstream.Silent(ctx, host) {
			return nil, &upstream.TransportError{Err: upstream.ErrHostSilent}
		}
		sent := time.Now()
		resp, err := hc.Do(req)
		if err != nil {
			if c.viaProxy && dialFailed(err) {
				// The base is the run's own credential proxy, so a connection
				// to it that fails says the proxy is down, not GitHub. That is
				// a fault on this host, returned unmarked and uncounted: GitHub
				// unreachable behind a live proxy arrives as the proxy's 502.
				return nil, err
			}
			class, counted := upstream.ClassifyTransport(ctx, err)
			if !counted {
				return nil, err
			}
			upstream.Record(ctx, upstream.GitHub, c.orgID, class)
			if !upstream.RetryableTransport(err, idempotent) || attempt >= maxAttempts || upstream.Unreachable(ctx, host) {
				upstream.MarkTransportFailure(ctx, host, sent, err)
				return nil, &upstream.TransportError{Err: err}
			}
			if err := c.retryAfter(ctx, attempt, class, transientBackoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		upstream.MarkAnswered(ctx, host)
		c.recordRateLimit(resp.Header)

		if resp.StatusCode < 400 {
			upstream.Record(ctx, upstream.GitHub, c.orgID, upstream.OK)
			return resp, nil
		}

		data, readErr := upstream.ReadErrorBody(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			if class, counted := upstream.ClassifyTransport(ctx, readErr); counted {
				upstream.Record(ctx, upstream.GitHub, c.orgID, class)
				upstream.MarkUnreachable(ctx, host)
				return nil, &upstream.TransportError{Err: readErr}
			}
			return nil, readErr
		}
		resp.Body = newBodyReader(data)

		retryAfter, hasRetryAfter := upstream.RetryAfter(resp.Header)
		primaryReset, primaryExhausted := primaryBudgetExhausted(resp.Header)
		class := upstream.ClassifyResponse(resp.StatusCode, resp.Header, data)
		if resp.StatusCode == http.StatusForbidden && (hasRetryAfter || primaryExhausted || isSecondaryRateLimitBody(data)) {
			class = upstream.RateLimited
		}
		upstream.Record(ctx, upstream.GitHub, c.orgID, class)

		switch class {
		case upstream.RateLimited:
			wait := retryAfter
			switch {
			case hasRetryAfter:
				// wait already set.
			case primaryExhausted && !primaryReset.IsZero() && time.Until(primaryReset) > 0:
				// GitHub told us exactly when the primary budget resets — use it
				// instead of a blind guess.
				wait = time.Until(primaryReset)
			default:
				wait = upstream.Backoff(attempt, rateLimitBackoffBase, maxRateLimitWait)
			}

			if !idempotent || wait > maxRateLimitWait || attempt >= maxAttempts || upstream.Unreachable(ctx, host) {
				return nil, &ErrRateLimited{ResumeAt: time.Now().Add(wait)}
			}
			if err := c.retryAfter(ctx, attempt, class, wait); err != nil {
				return nil, err
			}
		case upstream.Transient:
			wait := transientBackoff(attempt)
			if hasRetryAfter {
				wait = retryAfter
			}
			if !upstream.RetryableResponse(resp.StatusCode, class, idempotent) || attempt >= maxAttempts ||
				wait > transientBackoffMax || upstream.Unreachable(ctx, host) {
				upstream.MarkUnreachable(ctx, host)
				return resp, nil
			}
			if err := c.retryAfter(ctx, attempt, class, wait); err != nil {
				return nil, err
			}
		default:
			return resp, nil
		}
	}
}

// retryAfter counts the decision to retry and then waits it out.
func (c *Client) retryAfter(ctx context.Context, attempt int, class upstream.Class, wait time.Duration) error {
	upstream.RecordRetry(ctx, upstream.GitHub, c.orgID, class)
	return awaitRetry(ctx, attempt, class, wait)
}

// transientBackoff is the wait before retrying a transient failure.
func transientBackoff(attempt int) time.Duration {
	return upstream.Backoff(attempt, transientBackoffBase, transientBackoffMax)
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

// newBodyReader wraps already-read bytes as a Response.Body replacement, so
// a response whose body doWithRetry drained to inspect can still be read
// normally by the caller.
func newBodyReader(data []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(data))
}

// dialFailed reports whether err is a connection that never opened.
func dialFailed(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
