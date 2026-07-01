// Package ingest is the single emit seam for poller/tracker-produced
// events. It splits event delivery by durability need so the
// router stops riding the lossy in-memory bus while cosmetic/idempotent
// subscribers keep their best-effort delivery.
package ingest

import (
	"context"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
)

// Ingestor routes an emitted event two ways:
//
//   - Router-bound events (github:/jira:) are durably enqueued — the
//     events audit row and a queue row, written atomically — then a
//     best-effort wake nudges the router's drain worker. This is the
//     path that must never drop: losing one of these loses an event row
//     and its task, which is exactly the data loss this seam prevents.
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
func (i *Ingestor) Publish(evt domain.Event) {
	if i.queue != nil && routerBound(evt.EventType) {
		id, err := i.queue.Enqueue(context.Background(), evt.OrgID, evt)
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
			ingestLog.Error("durable enqueue failed; dropping (next poll re-diffs)", "event_type", evt.EventType, "org", evt.OrgID, "error", err)
			return
		}
		evt.ID = id // so the bus event and the queue row agree on the id
		if i.wake != nil {
			i.wake()
		}
	}
	i.bus.Publish(evt)
}

// routerBound reports whether the router consumes this event type — the
// same github:/jira: prefix set the router's old bus subscription
// filtered on, plus any event source an ee/ package has registered with
// internal/routing (TFAC-523, e.g. ee/slack's "slack:" prefix) — those must
// also get the durable-outbox enqueue, or they'd go bus-only and never reach
// the router via the drain worker. System events (system:poll:*) and raw
// webhook deliveries (webhook:github:*) are bus-only.
func routerBound(eventType string) bool {
	return strings.HasPrefix(eventType, "github:") || strings.HasPrefix(eventType, "jira:") || routing.SourceRegistered(eventType)
}
