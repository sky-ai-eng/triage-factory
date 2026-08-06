package tracker

import "go.opentelemetry.io/otel"

// tracer names its spans' owner by Go package path, per the convention in
// internal/telemetry's package doc. Resolved at init against the OTel
// global, which forwards to whatever provider Init installs later — so
// package-level is safe even though this runs before app.New.
var tracer = otel.Tracer("internal/tracker")
