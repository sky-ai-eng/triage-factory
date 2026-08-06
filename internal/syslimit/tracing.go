package syslimit

import "go.opentelemetry.io/otel"

// tracer owns this package's spans. Conventions in internal/telemetry.
var tracer = otel.Tracer("internal/syslimit")
