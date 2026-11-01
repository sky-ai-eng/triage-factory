package telemetry

import (
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// RecordSpanError puts err on span with an error status, and does nothing for
// a nil error, so an instrumented step reads as one statement rather than a
// branch repeated at every exit, and an error status never depends on
// remembering both calls.
func RecordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
