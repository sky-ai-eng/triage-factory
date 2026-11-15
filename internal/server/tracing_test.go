package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
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

// TestTracedHandlerDropsConcretePathFromServerSpans: through the real handler
// chain, a server span is named by the route pattern and carries no attribute
// holding the concrete path, which on a repo-keyed route is a customer's repo name.
func TestTracedHandlerDropsConcretePathFromServerSpans(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	s := &Server{mux: &routeMux{ServeMux: http.NewServeMux()}}
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

// TestConversationWriteRoutesPutConversationIDOnServerSpan: a request to a
// route that changes a conversation records the conversation's id on its
// server span, which is how a search on the id finds the request beside the
// engagement it affected. A read route records none, and neither does a path
// value that is not a UUID, which would otherwise export client-supplied text.
func TestConversationWriteRoutesPutConversationIDOnServerSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	s := newTestServer(t)
	handler := s.tracedHandler()
	cases := []struct {
		name    string
		pattern string
		id      string
		want    bool
	}{
		{"message", "POST /api/agent/conversations/{conversationID}/message", uuid.NewString(), true},
		{"stop", "POST /api/agent/conversations/{conversationID}/stop", uuid.NewString(), true},
		{"permission answer", "POST /api/agent/conversations/{conversationID}/permissions/{toolCallID}", uuid.NewString(), true},
		{"deliverables refresh", "POST /api/agent/conversations/{conversationID}/deliverables/refresh", uuid.NewString(), true},
		{"polled read", "GET /api/agent/conversations/{conversationID}", uuid.NewString(), false},
		{"id not a uuid", "POST /api/agent/conversations/{conversationID}/message", "acme-secret-project", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := len(recorder.Ended())
			method, path, _ := strings.Cut(tc.pattern, " ")
			path = strings.Replace(path, "{conversationID}", tc.id, 1)
			path = strings.Replace(path, "{toolCallID}", "toolu_fixture", 1)
			req := httptest.NewRequest(method, path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(httptest.NewRecorder(), req)

			var server []sdktrace.ReadOnlySpan
			for _, span := range recorder.Ended()[start:] {
				if span.SpanKind() == trace.SpanKindServer {
					server = append(server, span)
				}
			}
			if len(server) != 1 || server[0].Name() != tc.pattern {
				t.Fatalf("server spans = %d (first %q), want one named %q", len(server), spanName(server), tc.pattern)
			}
			got, found := "", false
			for _, kv := range server[0].Attributes() {
				if kv.Key == "conversation.id" {
					got, found = kv.Value.AsString(), true
				}
			}
			switch {
			case tc.want && got != tc.id:
				t.Errorf("conversation.id = %q, want %q", got, tc.id)
			case !tc.want && found:
				t.Errorf("conversation.id = %q, want none", got)
			}
		})
	}
}

func spanName(spans []sdktrace.ReadOnlySpan) string {
	if len(spans) == 0 {
		return ""
	}
	return spans[0].Name()
}
