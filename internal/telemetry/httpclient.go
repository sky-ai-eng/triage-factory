package telemetry

import (
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

// TracedTransport wraps an outbound RoundTripper so every request made
// inside a trace produces a client span. A nil base means
// http.DefaultTransport, the same convention http.Client uses, behind a
// check that moves to a fresh connection pool on the first request after a
// system suspend (sharedTransport).
//
// upstream (for example "github", "gotrue" or "s3") becomes the span name as
// "<upstream>.http" — one name per client, since otelhttp's default of
// "HTTP GET" makes every outbound call indistinguishable and the obvious
// fix, naming the span after the URL, is what scrub.go exists to prevent.
// What the call was *for* comes from the parent span.
//
// The transport is the choke point rather than each call site because it
// sees retries the caller's span can't separate and can't be bypassed by a
// path that builds its request differently — including direct-Do calls
// that skip a client's usual request helper.
//
// A request whose ctx carries no span passes straight through: no span and
// no traceparent header. A client span exists only inside a trace a TF
// stage root opened, so a call from an untraced background pass never
// becomes a one-span trace of its own. The test is IsValid, not IsSampled:
// an unsampled parent's children are the sampler's to drop.
func TracedTransport(base http.RoundTripper, upstream string) http.RoundTripper {
	name := upstream + ".http"
	if base == nil {
		base = sharedTransport
	}
	return otelhttp.NewTransport(base,
		otelhttp.WithTracerProvider(ScrubbedTracerProvider()),
		otelhttp.WithSpanNameFormatter(func(string, *http.Request) string { return name }),
		otelhttp.WithFilter(func(r *http.Request) bool {
			return trace.SpanContextFromContext(r.Context()).IsValid()
		}),
	)
}

// TracedHTTPClient is the convenience form for the common case: a
// constructor that writes &http.Client{Timeout: d} and has no transport of
// its own.
func TracedHTTPClient(timeout time.Duration, upstream string) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: TracedTransport(nil, upstream),
	}
}
