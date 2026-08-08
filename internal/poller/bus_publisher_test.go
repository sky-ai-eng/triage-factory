package poller

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
)

// busPublisher adapts a bare *eventbus.Bus to tracker.Publisher for tests
// that only want to observe the fan-out. Production emits through the
// durable ingestor, which is where the ctx does its work (the traceparent
// stamped onto the queue row); a bus publish has no durable row to carry
// one.
type busPublisher struct{ bus *eventbus.Bus }

func (p busPublisher) Publish(_ context.Context, evt domain.Event) { p.bus.Publish(evt) }

// PublishPreEnqueued is the tracker's post-commit fan-out for events it
// enqueued itself; with no durable queue behind this adapter there is
// nothing to skip, so it is the same bus publish.
func (p busPublisher) PublishPreEnqueued(_ context.Context, evt domain.Event) { p.bus.Publish(evt) }
