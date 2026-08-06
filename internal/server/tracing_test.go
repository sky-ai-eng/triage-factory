package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
