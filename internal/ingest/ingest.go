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
	"go.opentelemetry.io/otel/codes"
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
// Implements tracker.Publisher via Publish and PublishPreEnqueued — the
// latter for the tracker's snapshot-CAS emit, which owns its own
// transaction and so arrives here already durable.
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
			// exceptional.
			//
			// Nothing retries it here. Whether anything re-emits is a
			// property of the caller, not of this seam, and the surviving
			// callers differ — so no one of them may be assumed to
			// recover. An emit re-derived from durable state every cycle (the
			// tracker reconciling a stale review-request against the stored
			// snapshot) does come back on the next poll. One that fires off
			// a one-time observation does not: a first-discovery backfill
			// runs behind an entity whose snapshot is already seeded, so
			// the next cycle takes the already-exists path and never
			// synthesizes it again, and an ee/ ingest runs behind a
			// delivery id already recorded as consumed, so the upstream's
			// own redelivery is dropped as a duplicate. Both are gone for
			// good.
			// The tracker's diffed transitions are not in this set at all —
			// they commit inside the snapshot CAS's transaction and never
			// reach this path.
			//
			// Deliberately do NOT fall through to the bus: with no events
			// row, evt.ID is empty, and forwarding an id-less event would
			// push a phantom live update that has no durable backing for
			// the frontend to correlate against — a broadcast we can't
			// stand behind is worse than the silence.
			ingestLog.ErrorContext(ctx, "durable enqueue failed; event dropped", "event_type", evt.EventType, "org", evt.OrgID, "error", err)
			return
		}
		evt.ID = id // so the bus event and the queue row agree on the id
		if i.wake != nil {
			i.wake()
		}
	}
	i.bus.Publish(evt)
}

// PublishPreEnqueued is Publish for an event the caller ALREADY committed
// to the outbox itself: it nudges the drain worker and fans the event out
// to the bus, and deliberately does not enqueue. The tracker is the caller
// — it enqueues its diffed transitions inside the same transaction as the
// snapshot CAS they were diffed against (EnqueueBatchWithSnapshotCAS), so
// routing them through Publish would write a second queue row for an event
// already queued.
//
// evt.ID must be the id that enqueue minted, so the WS feed and the queue
// row agree the way they do on the ordinary path.
func (i *Ingestor) PublishPreEnqueued(ctx context.Context, evt domain.Event) {
	if i.wake != nil {
		i.wake()
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

	id, err := i.queue.Enqueue(ctx, evt.OrgID, evt, telemetry.TraceparentFrom(ctx))
	if err != nil {
		span.SetStatus(codes.Error, "enqueue")
		return "", err
	}
	span.SetAttributes(telemetry.EventID(id))
	return id, nil
}
