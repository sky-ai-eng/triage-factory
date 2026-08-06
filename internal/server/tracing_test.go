package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestHTTPSpanNameUsesRoutePattern(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		pattern string
		want    string
	}{
		{
			// The case the formatter exists for: otherwise every org mints
			// its own span name from the concrete path.
			name:    "method-qualified pattern is used verbatim",
			method:  http.MethodGet,
			pattern: "GET /api/orgs/{org_id}/teams",
			want:    "GET /api/orgs/{org_id}/teams",
		},
		{
			// The SPA catch-all, JSON 404, and GoTrue proxy are registered
			// without a method.
			name:    "method-less pattern gets the request's method",
			method:  http.MethodGet,
			pattern: "/",
			want:    "GET /",
		},
		{
			// Span start, before ServeMux routes. otelhttp calls the
			// formatter again after, so this is what an unrouted request
			// keeps.
			name:    "no pattern falls back to the method",
			method:  http.MethodPost,
			pattern: "",
			want:    "POST",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/api/orgs/018f2c/teams", nil)
			r.Pattern = tc.pattern
			if got := httpSpanName("ignored", r); got != tc.want {
				t.Errorf("httpSpanName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTraceableRequestExcludesSocketAndProbes(t *testing.T) {
	excluded := []string{"/api/ws", "/readyz", "/api/health"}
	for _, path := range excluded {
		if traceableRequest(httptest.NewRequest(http.MethodGet, path, nil)) {
			t.Errorf("%s produced a span; websocket upgrades measure session length and probes drown out real traffic", path)
		}
	}
	for _, path := range []string{"/api/tasks", "/", "/api/orgs/018f2c/teams"} {
		if !traceableRequest(httptest.NewRequest(http.MethodGet, path, nil)) {
			t.Errorf("%s produced no span; only the socket and the probes are excluded", path)
		}
	}
}

// TestTracedHandlerDropsConcretePathFromServerSpans drives the real handler
// chain, so it covers the wiring rather than the scrubber in isolation:
// tracedHandler has to actually be built against the scrubbing provider.
//
// The route matters. TF's API is not all opaque ids — this one is keyed by
// repository — so the concrete path is a customer's repo name, and
// otelhttp attaches it as a span-START option, which a SetAttributes-only
// filter would miss entirely. The span name is what carries the route
// dimension afterwards, since otelhttp resolves http.route at span start,
// before ServeMux has populated r.Pattern, and so never sets it.
func TestTracedHandlerDropsConcretePathFromServerSpans(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("PATCH /api/repos/{owner}/{repo}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	s.tracedHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/repos/acme/secret-project", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	span := ended[0]

	// The compensating control for the scrubbed path: the pattern,
	// uninterpolated.
	const want = "PATCH /api/repos/{owner}/{repo}"
	if span.Name() != want {
		t.Errorf("span name = %q, want %q", span.Name(), want)
	}
	for _, kv := range span.Attributes() {
		if kv.Key == "url.path" {
			t.Errorf("url.path survived on a server span (%q)", kv.Value.AsString())
		}
		if strings.Contains(kv.Value.AsString(), "secret-project") {
			t.Errorf("attribute %s carries the repo name (%q); TF has repo-keyed routes, so the concrete path is tenant data", kv.Key, kv.Value.AsString())
		}
	}
}
