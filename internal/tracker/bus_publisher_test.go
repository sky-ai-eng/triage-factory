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

// PublishPreEnqueued is the tracker's post-commit fan-out for events it
// enqueued itself; with no durable queue behind this adapter there is
// nothing to skip, so it is the same bus publish.
func (p busPublisher) PublishPreEnqueued(_ context.Context, evt domain.Event) { p.bus.Publish(evt) }

// droppingPublisher delivers nothing. It stands in for the crash between a
// committed emit and its cosmetic bus fan-out: the transaction landed, so
// the events are in the outbox and route on the drain worker's schedule —
// what a test with this publisher asserts is that nothing depends on the
// fan-out having happened.
type droppingPublisher struct{}

func (droppingPublisher) Publish(context.Context, domain.Event)            {}
func (droppingPublisher) PublishPreEnqueued(context.Context, domain.Event) {}
