package server

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
)

// tracer owns this package's spans. Conventions in internal/telemetry.
var tracer = otel.Tracer("internal/server")

// untracedPaths produce no server span, matched against the raw URL path
// because the filter runs before the mux routes.
//
//   - /api/ws is a websocket upgrade: the span would end when the socket
//     closes, so it measures session length, sits unexported in the batch
//     processor that whole time, and vanishes if the process dies first.
//   - /readyz and /api/health are probe targets, hit on a fixed interval
//     forever — they'd out-number every span that describes real work.
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
// would put a UUID in a span name.
//
// otelhttp calls this twice — at span start, before routing, and again
// after the handler returns if routing filled r.Pattern in — so the
// method-only fallback is what an unrouted request keeps.
func httpSpanName(_ string, r *http.Request) string {
	if r.Pattern == "" {
		return r.Method
	}
	// Go's pattern syntax is "[METHOD ][HOST]/[PATH]": a space is present
	// iff the pattern names a method. Method-less registrations (the SPA
	// catch-all "/", the JSON 404 "/api/") get the request's prepended.
	if strings.ContainsRune(r.Pattern, ' ') {
		return r.Pattern
	}
	return r.Method + " " + r.Pattern
}

// tracedHandler is the server's outermost handler chain: the mux, then the
// security headers, then otelhttp on the outside. Both Handler() and
// ListenAndServeContext build it through here so the two can't drift.
//
// otelhttp is outermost deliberately: what it doesn't wrap produces no
// span, and the responses TF generates *around* the mux — the JSON 404, a
// 405 from a path-matched-but-method-mismatched pattern — are exactly the
// ones worth seeing when a client reports a route "not working".
func (s *Server) tracedHandler() http.Handler {
	return otelhttp.NewHandler(
		s.withSecurityHeaders(s.mux),
		"http.server", // unused; httpSpanName ignores the operation
		otelhttp.WithSpanNameFormatter(httpSpanName),
		otelhttp.WithFilter(traceableRequest),
		// Ignore any inbound traceparent, demoting it to a link. This API
		// faces the internet, so honoring a caller-supplied parent would
		// let any client graft junk onto TF's traces and — under a
		// parent-based sampler — make TF's sampling decisions for it. The
		// SPA sends no trace context, so nothing legitimate is lost. (The
		// unconditional func is otelhttp's former WithPublicEndpoint();
		// only the Fn spelling survives.)
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }),
	)
}
