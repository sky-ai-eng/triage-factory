package server

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
)

// tracer names its spans' owner by Go package path, per the convention in
// internal/telemetry's package doc. Resolved at init against the OTel
// global, which forwards to whatever provider Init installs later — so
// package-level is safe even though this runs before app.New.
var tracer = otel.Tracer("internal/server")

// untracedPaths are the request paths that produce no server span, matched
// against the raw URL path because the filter runs before the mux routes.
//
// Each is excluded for its own reason, and none of them is "this endpoint
// is boring":
//
//   - /api/ws is a websocket upgrade. A server span ends when the handler
//     returns, and the handler returns when the socket closes — so the span
//     would measure session length, sit unexported in the batch processor
//     for the whole session, and vanish if the process dies first. The
//     work that happens *over* the socket is instrumented where it
//     originates, not here.
//   - /readyz and /api/health are probe targets. Platform liveness and
//     readiness checks hit them on a fixed interval forever, which would
//     make them the overwhelming majority of spans in the backend while
//     saying nothing about the workload.
var untracedPaths = map[string]struct{}{
	"/api/ws":     {},
	"/readyz":     {},
	"/api/health": {},
}

// traceableRequest is otelhttp's filter: true means "record a span".
func traceableRequest(r *http.Request) bool {
	_, excluded := untracedPaths[r.URL.Path]
	return !excluded
}

// httpSpanName names a server span after the ServeMux pattern that matched
// it ("GET /api/orgs/{org_id}/teams") rather than the concrete path, which
// would put a UUID in a span name and blow the name dimension open.
//
// otelhttp calls this twice: once at span start, when routing has not
// happened and r.Pattern is empty, and again after the handler returns if
// routing filled r.Pattern in. So the fallback is what an unrouted request
// (one the mux never matched) is left with, and the pattern is what
// everything else ends up named.
//
// A pattern registered without a method — the SPA catch-all "/", the JSON
// 404 "/api/", the GoTrue proxy "/auth/v1/" — carries no method token, so
// the request's method is prepended and those read "GET /" instead of "/".
func httpSpanName(_ string, r *http.Request) string {
	if r.Pattern == "" {
		return r.Method
	}
	// Go's pattern syntax is "[METHOD ][HOST]/[PATH]": a space is present
	// if and only if the pattern names a method.
	if strings.ContainsRune(r.Pattern, ' ') {
		return r.Pattern
	}
	return r.Method + " " + r.Pattern
}

// tracedHandler is the server's outermost handler chain: the mux, then the
// security headers, then otelhttp on the outside. Both Handler() and
// ListenAndServeContext build the chain through here so the two can't
// drift.
//
// otelhttp is outermost deliberately. Anything it does not wrap produces no
// span, and the responses TF generates *around* the mux — the JSON 404, a
// 405 from a pattern that matched a path but not a method — are exactly the
// ones worth seeing when a client reports a route "not working".
func (s *Server) tracedHandler() http.Handler {
	return otelhttp.NewHandler(
		s.withSecurityHeaders(s.mux),
		// Ignored: the name formatter below never reads the operation.
		// Its only job is to be the name of a request that somehow
		// reaches neither branch there, and there is no such request.
		"http.server",
		otelhttp.WithSpanNameFormatter(httpSpanName),
		otelhttp.WithFilter(traceableRequest),
		// Ignore whatever traceparent a client sends: start a fresh root
		// and demote the inbound context to a link. This API faces the
		// internet, and honoring a caller-supplied parent would let any
		// client graft junk onto TF's traces and — under a parent-based
		// sampler — make TF's sampling decisions for it. The SPA sends no
		// trace context at all, so nothing legitimate is lost.
		//
		// The unconditional func is what otelhttp's former
		// WithPublicEndpoint() was; only the Fn spelling survives.
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }),
	)
}
