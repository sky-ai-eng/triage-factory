package inference

import (
	"context"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// Classify maps a provider error to an upstream class. ok is false when ctx
// is done: a cancellation is not a provider outcome.
//
// It is the one classification of a provider failure, so a failure one
// caller retries is a failure every other caller also reads as the provider
// being unavailable. Each caller decides for itself what to do with the class.
//
// The order is load-bearing. A context overflow is Rejected before anything
// reads its text: the same request can never succeed, whatever else its
// message quotes. A rendered status settles the question by itself, so a
// 400 whose body happens to quote "connection reset" stays Rejected. The
// markers are consulted only for a failure that carries no status: a
// transport error that never reached the provider, or a mid-stream error
// chunk, which bifrost delivers with no status attached.
func Classify(ctx context.Context, err error) (c upstream.Class, ok bool) {
	if err == nil {
		return upstream.OK, true
	}
	if ctx.Err() != nil {
		return "", false
	}
	if errors.Is(err, ErrContextOverflow) {
		return upstream.Rejected, true
	}
	if status, found := RenderedStatus(err); found {
		// A failed call is never OK. A status below 400 beside an error does
		// not describe the failure, and Rejected is the reading nothing
		// retries.
		if status < 400 {
			return upstream.Rejected, true
		}
		return ClassifyStatus(status), true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return upstream.Transient, true
	}
	msg := strings.ToLower(err.Error())
	if eofPattern.MatchString(msg) {
		return upstream.Transient, true
	}
	for _, m := range transientMarkers {
		if strings.Contains(msg, m) {
			return upstream.Transient, true
		}
	}
	// An unrecognized failure is Rejected: retrying or backing off on
	// something that will never succeed (a bad key, an unknown model, a
	// request TF built wrong) only hides it.
	return upstream.Rejected, true
}

// ClassifyStatus is Classify's status arm alone, for callers that hold an
// HTTP status without an error (the SDK's api_error_status).
//
// 409 is Transient: a conflict with the provider's current state is expected
// to clear, and reading it as Rejected would fail a run, or mark a model
// refused, on a condition that does. A 403 is Auth whatever its body, unlike
// upstream.ClassifyResponse: a provider error reaches this package already
// flattened, so there is no body left to tell a proxy's page from the
// provider's own refusal.
func ClassifyStatus(status int) upstream.Class {
	switch {
	case status < 400:
		return upstream.OK
	case status == http.StatusTooManyRequests:
		return upstream.RateLimited
	case status == http.StatusRequestTimeout, status == http.StatusConflict, status >= 500:
		return upstream.Transient
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return upstream.Auth
	default:
		return upstream.Rejected
	}
}

// UpstreamName is the metric label for a provider this package serves, and
// false for any other: a provider outside the closed vocabulary records
// nothing rather than inventing a label value.
func UpstreamName(provider schemas.ModelProvider) (upstream.Name, bool) {
	switch provider {
	case ProviderAnthropic:
		return upstream.Anthropic, true
	case ProviderBedrock:
		return upstream.Bedrock, true
	default:
		return "", false
	}
}

// transientMarkers are the substrings that classify a failure with no status
// as Transient, matched against the lowercased error text. The neutral layer
// flattens a provider failure to a message (bifrostError renders the cause,
// not wraps it), so the text is all there is to read.
//
// The first group is how a provider, or a proxy in front of it, names an
// overload or an outage in text, which is all a mid-stream error chunk
// carries ("overloaded_error"). The second is a failure that never reached
// the provider: dial, DNS, TLS, a reset or a timeout. "timeout" already matches every
// longer timeout spelling below it; those stay listed so that narrowing it
// later cannot silently drop them.
var transientMarkers = []string{
	"rate limit",
	"rate_limit",
	"overloaded",
	"internal server error",
	"bad gateway",
	"service unavailable",
	"gateway timeout",

	"timeout",
	"timed out",
	"i/o timeout",
	"tls handshake timeout",
	"timeout awaiting response",
	"context deadline exceeded",
	"connection reset",
	"connection refused",
	"no such host",
	"network is unreachable",
	"no route to host",
	"broken pipe",
}

// eofPattern matches a standalone "eof" token (io.EOF renders as "EOF",
// io.ErrUnexpectedEOF as "unexpected EOF") without matching it embedded in a
// longer run of characters: a base64 blob or a field name that happens to
// contain "eof" has a word character on at least one side, so \b excludes it.
// It is kept out of transientMarkers because that list is plain substring
// matching.
var eofPattern = regexp.MustCompile(`\beof\b`)
