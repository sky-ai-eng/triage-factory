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
	"encoding/json"
	"errors"
	"io"
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

// RetryAfter reads the Retry-After header, which is either delay-seconds or
// an HTTP-date (RFC 7231 §7.1.3). A value that resolves to zero or negative —
// a non-positive delay-seconds count, or an HTTP-date already past (a stale
// header, or clock skew) — is reported as absent, so the caller falls back to
// its backoff rather than retrying with no pause at all.
func RetryAfter(h http.Header) (time.Duration, bool) {
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

// Backoff is the exponential backoff for the given 1-based attempt:
// base·2^(attempt-1), capped at max. An attempt below 1 is treated as 1.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 62 {
		return max
	}
	d := base * time.Duration(int64(1)<<uint(attempt-1))
	if d > max || d <= 0 {
		return max
	}
	return d
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
func ReadErrorBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, MaxErrorBody))
}

// isJSONObject reports whether body parses as a JSON object, whatever the
// Content-Type said.
func isJSONObject(body []byte) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(body, &m) == nil && m != nil
}
