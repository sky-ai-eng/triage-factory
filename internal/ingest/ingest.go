// Package ingest is the single emit seam for poller/tracker-produced
// events. It splits event delivery by durability need so the
// router stops riding the lossy in-memory bus while cosmetic/idempotent
// subscribers keep their best-effort delivery.
package ingest

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Ingestor routes an emitted event two ways:
//
//   - Router-bound events — those whose source the router consumes
//     (routing.RouterBound: github:/jira: plus any ee/-registered source)
//     — are durably enqueued: the events audit row and a queue row,
//     written atomically, then a best-effort wake nudges the router's
//     drain worker. This is the path that must never drop: losing one of
//     these loses an event row and its task, which is exactly the data
//     loss this seam prevents.
//   - Every event (the durable ones included) is also published to the
//     in-memory bus for loss-tolerant subscribers: ws-broadcast's
//     cosmetic UI feed and the scorer's idempotent poll-complete kick.
//     Those reconstruct their state from the DB and tolerate a drop, so
//     they stay on the bus and the bus never blocks producers.
//
// System events (system:poll:*, etc.) are bus-only — coalesced signals,
// not durable work.
//
// Implements tracker.Publisher via Publish, so it drops in wherever the
// tracker previously took a *eventbus.Bus.
type Ingestor struct {
	bus   *eventbus.Bus
	queue db.EventQueueStore
	wake  func() // best-effort nudge to the drain worker; nil = rely on its floor scan
}

// New builds an Ingestor. queue may be nil (e.g. tests that only care
// about the bus fan-out), in which case Publish degrades to a plain
// bus.Publish for every event.
func New(bus *eventbus.Bus, queue db.EventQueueStore, wake func()) *Ingestor {
	return &Ingestor{bus: bus, queue: queue, wake: wake}
}

// Publish durably enqueues router-bound events (and wakes the drainer),
// then forwards every event to the ephemeral bus. evt.OrgID must already
// be stamped by the caller (the tracker does this in its publish()).
//
// ctx is the emitting work's — a poll cycle, a Slack webhook request — and
// is what makes the durable hop traceable: the enqueue span below is the
// producer an event's later routing links back to. A caller with no span
// active (an untraced path, or tracing disabled) stamps nothing and the
// row routes exactly as it always did.
func (i *Ingestor) Publish(ctx context.Context, evt domain.Event) {
	if i.queue != nil && routing.RouterBound(evt.EventType) {
		id, err := i.enqueue(ctx, evt)
		if err != nil {
			// The durability boundary failed — this event will not route
			// (no events row, no task). That's the loss this package exists
			// to prevent, so log it loudly; a DB write failure here is
			// exceptional and the next poll re-diffs the entity forward.
			//
			// Deliberately do NOT fall through to the bus: with no events
			// row, evt.ID is empty, and forwarding an id-less event would
			// push a phantom live update that has no durable backing for
			// the frontend to correlate against. Dropping it (the next poll
			// re-emits) is cleaner than a broadcast we can't stand behind.
			ingestLog.ErrorContext(ctx, "durable enqueue failed; dropping (next poll re-diffs)", "event_type", evt.EventType, "org", evt.OrgID, "error", err)
			return
		}
		evt.ID = id // so the bus event and the queue row agree on the id
		if i.wake != nil {
			i.wake()
		}
	}
	i.bus.Publish(evt)
}

// enqueue is the durable half, wrapped in the producer span whose context
// travels on the queue row. The span is started BEFORE the traceparent is
// rendered, so the consumer links to this enqueue rather than to whatever
// happened to be active around it — one poll cycle emits many events, and
// "which emit was mine" is the question a link has to answer.
//
// The bus fan-out gets no span of its own: it is a coalescing, lossy
// broadcast to subscribers that root their own cycles, so there is nothing
// for a span there to measure or a link there to point at.
func (i *Ingestor) enqueue(ctx context.Context, evt domain.Event) (string, error) {
	ctx, span := tracer.Start(ctx, "event.enqueue",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(telemetry.EventType(evt.EventType), telemetry.OrgID(evt.OrgID)))
	defer span.End()

	id, err := i.queue.Enqueue(ctx, evt.OrgID, evt, traceparentFrom(ctx))
	if err != nil {
		span.SetStatus(codes.Error, "enqueue")
		return "", err
	}
	span.SetAttributes(telemetry.EventID(id))
	return id, nil
}

// traceparentFrom renders ctx's active span as the W3C traceparent the
// queue row carries. Empty when nothing is active — tracing disabled (the
// global propagator is a no-op), or a producer nobody has instrumented
// yet — which stores as NULL and costs the consumer only its link.
func traceparentFrom(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier["traceparent"]
}
