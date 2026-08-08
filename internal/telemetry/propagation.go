package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// TraceparentFrom renders ctx's active span as the W3C traceparent a
// durable message carries — the queue row's producer context, which is
// what lets an event's later routing link back to the cycle that emitted
// it. Empty when nothing is active (tracing disabled, so the global
// propagator is a no-op, or a producer nobody has instrumented yet); a
// store writes that as NULL and the consumer loses only its link.
//
// It lives here rather than in a producer because two of them now need it
// — the ingest seam's per-event enqueue and the tracker's batched
// snapshot-CAS emit — and a second copy of the inject dance is how the two
// would quietly stop agreeing on what "no context" looks like.
func TraceparentFrom(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier["traceparent"]
}
