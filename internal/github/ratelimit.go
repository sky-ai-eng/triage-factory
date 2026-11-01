package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	// and is the longest Retry-After on a 5xx, or on a response whose body
	// broke off, that is waited out; a longer one returns the failure instead
	// of retrying before the server asked. A transient failure carries no
	// reset time of its own (a spent budget's reset is awaitBudget's to wait
	// out), so the cap is the poll-cycle scale, not the rate-limit one.
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
// internal/eventing/poller / internal/eventing/tracker distinguish "stop and resume later" from
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

// The rate-limit buckets GitHub names in X-RateLimit-Resource that TF reads
// by name. GitHub returns others (integration_manifest, code_scanning_upload,
// …); those are recorded under whatever name it sends.
const (
	bucketCore    = "core"
	bucketGraphQL = "graphql"
	bucketSearch  = "search"
)

// requestBucket is the rate-limit bucket a request to u draws on, as far as
// its URL says: GraphQL and search are metered on their own, and everything
// else TF calls draws on core. The response's X-RateLimit-Resource is the
// authority; this is the bucket a pre-flight checks before there is a
// response, and the one a response that omits the header is recorded under.
func requestBucket(u *url.URL) string {
	switch {
	case strings.HasSuffix(u.Path, "/graphql"):
		return bucketGraphQL
	case strings.Contains(u.Path, "/search/"):
		return bucketSearch
	}
	return bucketCore
}

// RateLimit returns the latest reading of bucket from this client's
// responses. ok is false until a response for that bucket has carried
// x-ratelimit-* headers — a GHES host with rate limiting disabled never sends
// them, and callers should treat that as unlimited rather than exhausted.
func (c *Client) RateLimit(bucket string) (remaining int, reset time.Time, ok bool) {
	c.rlMu.Lock()
	defer c.rlMu.Unlock()
	b, ok := c.rl[bucket]
	return b.Remaining, b.Reset, ok
}

// GraphQLBudgetResumeAt is when c's last reading of the GraphQL budget resets,
// when that reading says the budget is spent. A query sent on a spent budget
// whose reset is within maxRateLimitWait waits for the reset instead of
// returning ErrRateLimited, so a caller whose own deadline is shorter than the
// wait sees its deadline, and reads this to learn it was a rate limit.
func (c *Client) GraphQLBudgetResumeAt() (time.Time, bool) {
	remaining, reset, ok := c.RateLimit(bucketGraphQL)
	if !ok || remaining > 0 || !reset.After(time.Now()) {
		return time.Time{}, false
	}
	return reset, true
}

// recordRateLimit keeps a response's reading of the bucket its
// X-RateLimit-Resource names, or of fallback (the request's bucket) when the
// header is absent, and returns that bucket. Called on every response the
// request core sees, successful or not. x-ratelimit-remaining and
// x-ratelimit-reset are required for a reading; x-ratelimit-limit is read
// alongside them, or derived from x-ratelimit-used when absent, and is 0
// (unknown) when neither arrives. Missing or
// unparsable headers leave the prior reading untouched — GHES may omit them
// entirely when rate limiting is disabled.
//
// A client made for an org and a credential (WithOrg, WithCredential) also
// reports the reading to the process-wide recorder (upstream.RecordBudget),
// which the rate-limit gauges and the low-budget WARN read. A client that
// names no credential reports nothing: an unauthenticated call has no budget
// of the org's, and a run's credential-proxy client cannot say which of the
// org's credentials the proxy attached.
func (c *Client) recordRateLimit(h http.Header, fallback string) string {
	bucket := h.Get("X-RateLimit-Resource")
	if bucket == "" {
		bucket = fallback
	}
	remaining, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err != nil {
		return bucket
	}
	resetUnix, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return bucket
	}
	limit, err := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	if err != nil {
		limit = 0
		if used, err := strconv.Atoi(h.Get("X-RateLimit-Used")); err == nil {
			limit = used + remaining
		}
	}
	b := upstream.Budget{Remaining: remaining, Limit: limit, Reset: time.Unix(resetUnix, 0)}

	c.rlMu.Lock()
	if c.rl == nil {
		c.rl = make(map[string]upstream.Budget)
	}
	c.rl[bucket] = b
	c.rlMu.Unlock()

	if c.orgID != "" && c.credential != "" {
		upstream.RecordBudget(upstream.GitHub, c.orgID, c.credential, bucket, b)
	}
	return bucket
}

// awaitBudget blocks until there is likely budget in bucket for an idempotent
// request, using this client's latest reading of it. It is a pre-flight
// check only — it never talks to GitHub — so a cycle that's already known to
// be out of budget doesn't spend a request finding that out again. Mutations
// never call this: see doMutation.
func (c *Client) awaitBudget(ctx context.Context, bucket string) error {
	remaining, reset, ok := c.RateLimit(bucket)
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
	if err := upstream.WaitOnLimit(ctx, upstream.GitHub, c.orgID, wait); err != nil {
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
// cancelled wait is the caller leaving. A rate limit's wait is also logged and
// counted (upstream.WaitOnLimit); a transient failure's backoff is not.
func (c *Client) awaitRetry(ctx context.Context, attempt int, class upstream.Class, wait time.Duration) error {
	ctx, span := tracer.Start(ctx, "github.ratelimit.backoff",
		trace.WithAttributes(telemetry.Attempt(attempt), telemetry.Disposition(string(class))))
	defer span.End()

	var err error
	if class == upstream.RateLimited {
		err = upstream.WaitOnLimit(ctx, upstream.GitHub, c.orgID, wait)
	} else {
		err = upstream.Sleep(ctx, wait)
	}
	if err != nil {
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
func (c *Client) doIdempotent(ctx context.Context, build reqBuilder) (*http.Response, []byte, error) {
	return c.doWithRetry(ctx, c.http, true, false, build)
}

// doMutation sends a single, non-retried request over c.http. A mutation
// (POST/PUT/PATCH/DELETE that changes state) must never be silently
// replayed — a retried mutation could double the side effect — so a
// rate-limited 429/403 response short-circuits straight into ErrRateLimited
// and a transient failure is returned as-is. See doWithRetry.
func (c *Client) doMutation(ctx context.Context, build reqBuilder) (*http.Response, []byte, error) {
	return c.doWithRetry(ctx, c.http, false, false, build)
}

// doStream is doIdempotent over hc for a response the caller streams instead
// of buffering: a success comes back with its body unread, and only an error
// response's body is returned. See doWithRetry.
func (c *Client) doStream(ctx context.Context, hc *http.Client, build reqBuilder) (*http.Response, []byte, error) {
	return c.doWithRetry(ctx, hc, true, true, build)
}

// doWithRetry is the shared request loop behind every request-core method
// (request, GetConditional, postGraphQL, DownloadArtifact — the last supplies
// its own hc with an extended timeout and streams its body, everything else
// passes c.http). Every attempt is classified and counted (upstream.Record)
// against the client's org.
//
// For idempotent calls it pre-flights a known exhausted budget (awaitBudget)
// and retries up to maxRateLimitRetries extra attempts:
//   - a rate limit — a 429, or a 403 carrying Retry-After,
//     x-ratelimit-remaining: 0, or a secondary-limit body — honoring
//     Retry-After when present, else the primary reset time when that's what
//     triggered it, else exponential backoff;
//   - a transient failure — a dropped connection, a response body that breaks
//     off partway, a 5xx or a 408 — after the response's Retry-After when it
//     has one, else transient backoff. A body that breaks off is retried
//     only once the budget its headers report has room again (awaitBudget),
//     since a retry against a spent budget would be refused. A transient
//     failure that another attempt would only repeat is returned at once: a
//     403 whose body is not JSON (a proxy in front of GHES), a TLS failure, a
//     timeout (upstream.RetryableResponse, upstream.RetryableTransport), or
//     one whose Retry-After is beyond transientBackoffMax.
//
// Every sleep is ctx-aware. Mutations get exactly one attempt: a rate limit
// returns ErrRateLimited immediately, and a transient failure is returned to
// the caller unchanged.
//
// Under a fail-fast scope (upstream.WithFailFast), a request that ends in a
// transient failure marks its host unreachable, and every later request to
// that host gets one attempt the same way a mutation does. A request that
// timed out counts toward its host's silence, and a later request to a silent
// host is not sent (upstream.Silent). A body that breaks off counts toward
// unreachable only: the host answered. A response that fails marks the host
// that served it, which after a redirect (an artifact's signed storage URL)
// is not the host the request was sent to, so a storage host's failure does
// not cost later API requests their retries.
//
// Any response that isn't retried is returned to the caller along with its
// body, already read — whole for a success, capped at upstream.MaxErrorBody
// for an error — so a success is counted only once its body has arrived. The
// response's own Body is spent (http.NoBody). A streamed request (stream) is
// the exception: its success comes back with the body unread, no bytes, and
// the attempt not yet counted, and streamedBody counts it when the caller's
// read ends. That read is never retried, because what it read is already in
// the caller's hands.
func (c *Client) doWithRetry(ctx context.Context, hc *http.Client, idempotent, stream bool, build reqBuilder) (*http.Response, []byte, error) {
	maxAttempts := 1
	if idempotent {
		maxAttempts = 1 + maxRateLimitRetries
	}

	// bucket is the rate-limit bucket the request draws on: predicted from
	// its URL until a response names it.
	var bucket string
	for attempt := 1; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, nil, err
		}
		if attempt == 1 {
			bucket = requestBucket(req.URL)
			if idempotent {
				if err := c.awaitBudget(ctx, bucket); err != nil {
					return nil, nil, err
				}
			}
		}
		host := req.URL.Host
		if upstream.Silent(ctx, host) {
			return nil, nil, &upstream.TransportError{Err: upstream.ErrHostSilent}
		}
		sent := time.Now()
		resp, err := hc.Do(req)
		if err != nil {
			if c.viaProxy && dialFailed(err) {
				// The base is the run's own credential proxy, so a connection
				// to it that fails says the proxy is down, not GitHub. That is
				// a fault on this host, returned unmarked and uncounted: GitHub
				// unreachable behind a live proxy arrives as the proxy's 502.
				return nil, nil, err
			}
			class, counted := upstream.ClassifyTransport(ctx, err)
			if !counted {
				return nil, nil, err
			}
			upstream.Record(ctx, upstream.GitHub, c.orgID, class)
			if !upstream.RetryableTransport(err, idempotent) || attempt >= maxAttempts || upstream.Unreachable(ctx, host) {
				upstream.MarkTransportFailure(ctx, host, sent, err)
				return nil, nil, &upstream.TransportError{Err: err}
			}
			if err := c.retryAfter(ctx, attempt, class, transientBackoff(attempt)); err != nil {
				return nil, nil, err
			}
			continue
		}
		upstream.MarkAnswered(ctx, host)
		bucket = c.recordRateLimit(resp.Header, bucket)
		// The response came from the last hop of any redirect.
		servedBy := host
		if resp.Request != nil {
			servedBy = resp.Request.URL.Host
		}

		if stream && resp.StatusCode < 400 {
			resp.Body = &streamedBody{ReadCloser: resp.Body, ctx: ctx, orgID: c.orgID, host: servedBy}
			return resp, nil, nil
		}

		var data []byte
		var readErr error
		if resp.StatusCode < 400 {
			data, readErr = io.ReadAll(resp.Body)
		} else {
			data, readErr = upstream.ReadErrorBody(resp.Body)
		}
		resp.Body.Close()
		resp.Body = http.NoBody
		if readErr != nil {
			// The response broke off mid-body: a transport failure like one
			// from Do, retried on the same terms, after the Retry-After its
			// headers carry, since those arrived intact. GitHub did answer, so
			// it counts toward unreachable but never toward silent.
			class, counted := upstream.ClassifyTransport(ctx, readErr)
			if !counted {
				return nil, nil, readErr
			}
			upstream.Record(ctx, upstream.GitHub, c.orgID, class)
			wait, hasRetryAfter := upstream.RetryAfter(resp.Header)
			if !hasRetryAfter {
				wait = transientBackoff(attempt)
			}
			if !upstream.RetryableTransport(readErr, idempotent) || attempt >= maxAttempts ||
				wait > transientBackoffMax || upstream.Unreachable(ctx, host) {
				upstream.MarkUnreachable(ctx, servedBy)
				return nil, nil, &upstream.TransportError{Err: readErr}
			}
			// Headers that report a spent primary budget mean the retry would
			// be refused until it resets, so it waits as the next request
			// would, or stops with ErrRateLimited.
			if err := c.awaitBudget(ctx, bucket); err != nil {
				return nil, nil, err
			}
			if err := c.retryAfter(ctx, attempt, class, wait); err != nil {
				return nil, nil, err
			}
			continue
		}

		if resp.StatusCode < 400 {
			upstream.Record(ctx, upstream.GitHub, c.orgID, upstream.OK)
			return resp, data, nil
		}

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
				return nil, nil, &ErrRateLimited{ResumeAt: time.Now().Add(wait)}
			}
			if err := c.retryAfter(ctx, attempt, class, wait); err != nil {
				return nil, nil, err
			}
		case upstream.Transient:
			wait := transientBackoff(attempt)
			if hasRetryAfter {
				wait = retryAfter
			}
			if !upstream.RetryableResponse(resp.StatusCode, class, idempotent) || attempt >= maxAttempts ||
				wait > transientBackoffMax || upstream.Unreachable(ctx, host) {
				upstream.MarkUnreachable(ctx, servedBy)
				return resp, data, nil
			}
			if err := c.retryAfter(ctx, attempt, class, wait); err != nil {
				return nil, nil, err
			}
		default:
			return resp, data, nil
		}
	}
}

// retryAfter counts the decision to retry and then waits it out.
func (c *Client) retryAfter(ctx context.Context, attempt int, class upstream.Class, wait time.Duration) error {
	upstream.RecordRetry(ctx, upstream.GitHub, c.orgID, class)
	return c.awaitRetry(ctx, attempt, class, wait)
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

// streamedBody is the body of a success doWithRetry returns unread, and it
// counts that attempt once the outcome is known. A body read to its end, or
// closed early by a caller that stopped reading, is OK. A read that breaks
// off partway is a transport failure: it is counted as one, marks the host
// unreachable, and is returned as a *upstream.TransportError so a caller can
// tell it from a fault of its own, such as a failed write to its destination.
type streamedBody struct {
	io.ReadCloser
	ctx   context.Context
	orgID string
	host  string
	ended bool
}

func (b *streamedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	switch err {
	case nil:
	case io.EOF:
		b.end(upstream.OK, true)
	default:
		class, counted := upstream.ClassifyTransport(b.ctx, err)
		b.end(class, counted)
		if counted {
			err = &upstream.TransportError{Err: err}
		}
	}
	return n, err
}

func (b *streamedBody) Close() error {
	b.end(upstream.OK, true)
	return b.ReadCloser.Close()
}

// end counts the attempt the first time its outcome is known. An outcome
// that is not counted (the caller's ctx ended) is not counted at Close
// either.
func (b *streamedBody) end(class upstream.Class, counted bool) {
	if b.ended {
		return
	}
	b.ended = true
	if !counted {
		return
	}
	upstream.Record(b.ctx, upstream.GitHub, b.orgID, class)
	if class != upstream.OK {
		upstream.MarkUnreachable(b.ctx, b.host)
	}
}

// dialFailed reports whether err is a connection that never opened.
func dialFailed(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
