package routing

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracer owns this package's spans. Conventions in internal/telemetry.
var tracer = otel.Tracer("internal/routing")

// startRouteSpan opens the root span for routing one claimed queue row.
//
// A ROOT, explicitly (WithNewRoot), even though the drain worker's context
// has no span today: rootness here is a decision, not a circumstance. One
// poll cycle emits N events that route later — minutes later, possibly on
// another pod, and up to five times if a row retries — so descending from
// the emit would grow a single unbounded multi-pod trace where N navigable
// ones belong.
//
// The producer is a LINK instead. Links are how a consumer says "this work
// came from there" without joining that work's trace, and a Tempo reader
// gets both directions out of the one link: the routing trace names its
// producer, and the poll-cycle span lists what it caused. A row with no
// traceparent — tracing off, an untraced producer, a row enqueued before
// this landed — gets no link and is otherwise identical.
//
// The queue-wait attribute is the one thing this span cannot measure by
// running: the row's time between enqueue and claim is over before the span
// starts, and it is exactly the number that distinguishes "routing is slow"
// from "the queue was backed up."
func startRouteSpan(ctx context.Context, qe *domain.QueuedEvent) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			telemetry.EventID(qe.EventID),
			telemetry.EventType(qe.EventType),
			telemetry.OrgID(qe.OrgID),
			telemetry.Attempt(qe.Attempts),
		),
	}
	if qe.ClaimedAt != nil {
		opts = append(opts, trace.WithAttributes(telemetry.QueueWait(queueWait(qe.EnqueuedAt, *qe.ClaimedAt))))
	}
	if link, ok := producerLink(qe.Traceparent); ok {
		opts = append(opts, trace.WithLinks(link))
	}
	return tracer.Start(ctx, "route.event", opts...)
}

// producerLink turns the queue row's stored traceparent back into a span
// link. ok=false for an absent one (the ordinary untraced case) and for a
// malformed one — a link to an invalid SpanContext is not recorded anyway,
// so refusing to build it keeps "no producer" a single state rather than
// two that look different in code and identical in the backend.
func producerLink(traceparent string) (trace.Link, bool) {
	if traceparent == "" {
		return trace.Link{}, false
	}
	produced := otel.GetTextMapPropagator().Extract(
		context.Background(),
		propagation.MapCarrier{"traceparent": traceparent},
	)
	sc := trace.SpanContextFromContext(produced)
	if !sc.IsValid() {
		return trace.Link{}, false
	}
	return trace.Link{SpanContext: sc}, true
}

// recordDisposition stamps the routed event's outcome onto whichever span
// is active — the root, in production. It is the answer to "what did
// routing decide," and it belongs on the span rather than only in the
// sentinel event, because most dispositions (taskless_no_handler, frozen,
// the dedup suppression) end the pipeline early and would otherwise show up
// as a short span with no explanation.
//
// A disposition is never a span error, including DispositionError: that
// value means a store query the pipeline depends on failed, and the failing
// call already carries its own error status on its own span, where the
// cause is visible. Marking the root too would double-count one failure.
func recordDisposition(ctx context.Context, disposition string) {
	if disposition == "" {
		return
	}
	trace.SpanFromContext(ctx).SetAttributes(telemetry.Disposition(disposition))
}

// queueWait is the enqueue→claim duration. Negative results — two pods'
// clocks disagreeing about now() across the write pair — are clamped to
// zero rather than exported as a wait that ran backwards.
func queueWait(enqueuedAt, claimedAt time.Time) time.Duration {
	if d := claimedAt.Sub(enqueuedAt); d > 0 {
		return d
	}
	return 0
}
