package telemetry

import (
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// TracedTransport wraps an outbound RoundTripper so every request through
// it produces a client span. A nil base means http.DefaultTransport, the
// same convention http.Client uses.
//
// upstream ("github", "jira", "slack", "llm") becomes the span name as
// "<upstream>.http" — one name per client, since otelhttp's default of
// "HTTP GET" makes every outbound call indistinguishable and the obvious
// fix, naming the span after the URL, is what scrub.go exists to prevent.
// What the call was *for* comes from the parent span.
//
// The transport is the choke point rather than each call site because it
// sees retries the caller's span can't separate and can't be bypassed by a
// path that builds its request differently — including direct-Do calls
// that skip a client's usual request helper.
func TracedTransport(base http.RoundTripper, upstream string) http.RoundTripper {
	name := upstream + ".http"
	return otelhttp.NewTransport(base,
		otelhttp.WithTracerProvider(ScrubbedTracerProvider()),
		otelhttp.WithSpanNameFormatter(func(string, *http.Request) string { return name }),
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
