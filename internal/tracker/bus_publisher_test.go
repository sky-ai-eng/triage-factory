package tracker

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
)

// busPublisher adapts a bare *eventbus.Bus to Publisher for tests that only
// want to observe the fan-out. Production always emits through the durable
// ingestor, which is where the ctx does its work (the traceparent stamped
// onto the queue row); a bus publish has no durable row to carry one, so
// dropping it here loses nothing a test could assert on.
type busPublisher struct{ bus *eventbus.Bus }

func (p busPublisher) Publish(_ context.Context, evt domain.Event) { p.bus.Publish(evt) }
