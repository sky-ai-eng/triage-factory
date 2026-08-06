package telemetry

import (
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// TracedTransport wraps an outbound RoundTripper so every request through
// it produces a client span. base may be nil, meaning http.DefaultTransport
// — the same convention http.Client itself uses.
//
// upstream names the service being called ("github", "jira", "slack",
// "llm") and becomes the span name as "<upstream>.http". A single name per
// client, deliberately: the alternative is otelhttp's default of "HTTP GET",
// which makes every outbound call in the process indistinguishable, and the
// obvious fix — naming the span after the URL — is the thing the scrubber
// above exists to prevent. What the call was *for* comes from the parent
// span, which is the caller TF wrote and can name freely.
//
// The transport is the right choke point for this rather than each call
// site: it sees retries the caller's own span cannot separate, and it
// cannot be bypassed by a code path that builds its request differently —
// including the direct-Do calls that skip a client's usual request helper.
func TracedTransport(base http.RoundTripper, upstream string) http.RoundTripper {
	name := upstream + ".http"
	return otelhttp.NewTransport(base,
		otelhttp.WithTracerProvider(ScrubbedTracerProvider()),
		otelhttp.WithSpanNameFormatter(func(string, *http.Request) string { return name }),
	)
}

// TracedHTTPClient returns an *http.Client with the given timeout whose
// transport is instrumented. The convenience form for the common case: a
// client constructor that today writes &http.Client{Timeout: d} and has no
// transport of its own.
func TracedHTTPClient(timeout time.Duration, upstream string) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: TracedTransport(nil, upstream),
	}
}
