// Package upstream is the shared request-outcome model for TF's HTTP API
// clients (GitHub, Jira, Slack's Web API): one classification of what a
// request's response or failure means, the retry primitives every client's
// own retry loop is built from, bounded reads and short excerpts of error
// bodies, and the request and retry counters.
//
// Each client keeps its own retry loop and its own limits. What lives here is
// only what they share, so a client whose upstream signals a rate limit or an
// auth failure in a non-standard way (GitHub's rate-limit 403s) classifies
// that response itself before falling back to ClassifyResponse.
package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Name is the closed vocabulary of upstream systems; it is a metric label.
type Name string

const (
	GitHub Name = "github"
	Jira   Name = "jira"
	Slack  Name = "slack"
)

// Class is the closed vocabulary of request outcomes; it is a metric label.
type Class string

const (
	OK          Class = "ok"
	RateLimited Class = "rate_limited" // the upstream asked us to wait
	Transient   Class = "transient"    // 5xx, 408, a transport failure, or a non-JSON 403
	Auth        Class = "auth"         // 401, or a JSON 403 that is not a rate limit
	Rejected    Class = "rejected"     // any other 4xx: the request itself is wrong
)

// Classified is implemented by a client's typed error for a failed upstream
// request, so a caller holding only the error can recover its class (ClassOf).
type Classified interface {
	UpstreamClass() Class
}

// ClassifyResponse is the default classification of an HTTP response. The
// header is accepted so a caller hands over the whole response; the default
// reads only the status and, for a 403, the body.
//
// A 403 whose body is a JSON object came from the upstream itself and means
// the credential may not do this. A 403 with any other body came from
// something in front of the upstream (a VPN-dependent proxy in front of
// GHES), which says nothing about the credential and is expected to clear.
func ClassifyResponse(status int, header http.Header, body []byte) Class {
	switch {
	case status < 400:
		return OK
	case status == http.StatusTooManyRequests:
		return RateLimited
	case status == http.StatusRequestTimeout || status >= 500:
		return Transient
	case status == http.StatusUnauthorized:
		return Auth
	case status == http.StatusForbidden:
		if isJSONObject(body) {
			return Auth
		}
		return Transient
	default:
		return Rejected
	}
}

// ClassifyTransport classifies an error from http.Client.Do. When the
// caller's ctx is done the request was abandoned rather than failed, so it is
// not an upstream outcome and ok is false: nothing should be counted.
func ClassifyTransport(ctx context.Context, err error) (Class, bool) {
	if ctx.Err() != nil {
		return "", false
	}
	return Transient, true
}

// ClassOf recovers the class of an error returned by a client: a Classified
// error's own class, or Transient for a transport failure. ok is false for
// anything else, which is a local failure (a request that could not be
// built, a response that could not be parsed) or a cancellation.
//
// context.DeadlineExceeded is a transport failure here, not a cancellation:
// an http.Client timeout matches it, and that is the upstream not answering
// in time. A caller's own deadline matches it too, and the error alone
// cannot tell the two apart. The clients can, because they hold the ctx:
// ClassifyTransport is what keeps a caller's expired deadline out of the
// counters.
func ClassOf(err error) (Class, bool) {
	if err == nil {
		return "", false
	}
	var c Classified
	if errors.As(err, &c) {
		return c.UpstreamClass(), true
	}
	if errors.Is(err, context.Canceled) {
		return "", false
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return Transient, true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return Transient, true
	}
	return "", false
}

// LogLevel is the level a poll cycle logs err at. An upstream failure (one
// ClassOf recognizes) logs at Debug, because the poller reports a source's
// connection being lost and restored once each, from the cycle's own request
// outcomes, rather than once per failed request per cycle. Anything else is a
// fault of TF's own (a database read, a setting, a credential it could not
// load) and keeps the caller's level.
func LogLevel(err error, otherwise slog.Level) slog.Level {
	if _, ok := ClassOf(err); ok {
		return slog.LevelDebug
	}
	return otherwise
}

// Retryable reports whether a request that ended in class c may be sent
// again. A rate-limited request was refused before it was processed, so
// replaying it is safe even for a mutation. A transient failure may have
// happened after the upstream acted, so only an idempotent request is
// replayed.
func Retryable(c Class, idempotent bool) bool {
	switch c {
	case RateLimited:
		return true
	case Transient:
		return idempotent
	default:
		return false
	}
}

// RetryableResponse is Retryable for a response with the given status. A
// Transient 403 is the exception: it came from something in front of the
// upstream (a proxy that wants a VPN, a login lockout), which does not clear
// within a client's backoff, so another attempt only delays the error. It
// stays Transient for counting, since it says nothing about the credential.
func RetryableResponse(status int, c Class, idempotent bool) bool {
	if status == http.StatusForbidden && c == Transient {
		return false
	}
	return Retryable(c, idempotent)
}

// RetryableTransport is Retryable for an error from http.Client.Do, which is
// always Transient. Two transport failures are not retried, because another
// attempt only delays the same error: a TLS failure (a certificate the
// client does not trust or that names another host, or a server that does
// not speak TLS), which every attempt meets again, and a timeout, which has
// already spent the client's whole time budget on one attempt.
func RetryableTransport(err error, idempotent bool) bool {
	if !Retryable(Transient, idempotent) {
		return false
	}
	var (
		verifyErr    *tls.CertificateVerificationError
		authorityErr x509.UnknownAuthorityError
		hostnameErr  x509.HostnameError
		invalidErr   x509.CertificateInvalidError
		recordErr    tls.RecordHeaderError
	)
	switch {
	case errors.As(err, &verifyErr), errors.As(err, &authorityErr),
		errors.As(err, &hostnameErr), errors.As(err, &invalidErr),
		errors.As(err, &recordErr), errors.Is(err, http.ErrSchemeMismatch):
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return true
}

// maxRetryAfter is the longest wait RetryAfter reports. It is far above any
// client's own cap, which is what decides whether a wait is honored; the
// bound exists so a huge delay-seconds value cannot overflow a Duration.
const maxRetryAfter = 24 * time.Hour

// RetryAfter reads the Retry-After header, which is either delay-seconds or
// an HTTP-date (RFC 7231 §7.1.3). A value that resolves to zero or negative —
// a non-positive delay-seconds count, or an HTTP-date already past (a stale
// header, or clock skew) — is reported as absent, so the caller falls back to
// its backoff rather than retrying with no pause at all. A value above
// maxRetryAfter is reported as maxRetryAfter; the caller compares the result
// with its own cap.
func RetryAfter(h http.Header) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0, false
		}
		if secs > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter, true
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		wait := time.Until(t)
		if wait <= 0 {
			return 0, false
		}
		return min(wait, maxRetryAfter), true
	}
	return 0, false
}

// Backoff is the exponential backoff for the given 1-based attempt:
// base·2^(attempt-1), capped at max. An attempt below 1 is treated as 1.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := uint(attempt - 1)
	// Compared before shifting, so a large attempt cannot overflow into a
	// small wait.
	if base <= 0 || shift >= 63 || base > max>>shift {
		return max
	}
	return base << shift
}

// Sleep waits d, or returns ctx.Err() as soon as ctx is done, so the caller's
// deadline bounds every wait regardless of a client's retry cap.
func Sleep(ctx context.Context, d time.Duration) error {
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

// MaxErrorBody is the most of an error response's body a client keeps.
const MaxErrorBody = 64 << 10

// ReadErrorBody reads at most MaxErrorBody bytes of an error response's body.
// It does not read past the cap: the remainder is discarded with the
// connection when the caller closes the body, which is cheaper than draining
// an arbitrarily large proxy page to keep one connection warm. Success bodies
// are not read through this.
//
// A JSON body longer than the cap is cut mid-document and no longer parses,
// so ClassifyResponse and Excerpt treat it as a non-JSON body: a 403 that
// large classifies Transient rather than Auth. No upstream TF talks to sends
// an error body anywhere near that size.
func ReadErrorBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, MaxErrorBody))
}

// isJSONObject reports whether body parses as a JSON object, whatever the
// Content-Type said.
func isJSONObject(body []byte) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(body, &m) == nil && m != nil
}
