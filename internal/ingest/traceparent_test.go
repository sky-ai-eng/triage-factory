package ingest_test

import (
	"context"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// liveTracing installs a recording SDK provider and the W3C propagator for
// one test, mirroring what Init does when TF_TRACES_ENDPOINT is set, and
// puts the globals back afterwards. Returns the recorder so a test can read
// what was actually produced.
//
// Exactly ONE test in this package may use it. internal/ingest's tracer is
// a package-level var created at init, and the OTel global binds such a
// tracer to the first provider installed in the process and never re-binds
// it — so a second caller here would record into this recorder and read an
// empty one of its own. A second recording test means switching to the
// install-once-and-cursor shape internal/routing's tracing_test.go uses.
func liveTracing(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})
	return recorder
}

// TestIngestor_StampsProducerTraceparent is the emit half of the durable
// hop: the queue row carries the trace context of the enqueue span, so the
// routing that happens minutes later can point back at the cycle that found
// the event. It must be the ENQUEUE span's context, not the caller's —
// one poll cycle emits many events, and a link to the cycle could not say
// which emit it came from.
func TestIngestor_StampsProducerTraceparent(t *testing.T) {
	recorder := liveTracing(t)
	bus, _ := captureBus(t)
	conn := newIngestTestDB(t)
	queue := sqlitestore.New(conn).EventQueue
	ing := ingest.New(bus, queue, nil)

	ctx, caller := otel.Tracer("ingest/test").Start(context.Background(), "poll.cycle")
	entityID := seedIngestEntity(t, conn)
	ing.Publish(ctx, domain.Event{
		OrgID:     runmode.LocalDefaultOrgID,
		EntityID:  &entityID,
		EventType: domain.EventGitHubPRCICheckFailed,
	})
	caller.End()

	rows, err := queue.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListForEntity: rows=%d err=%v", len(rows), err)
	}
	if rows[0].Traceparent == "" {
		t.Fatal("queue row carries no traceparent; the durable hop is the one place trace context is propagated in this pipeline")
	}

	enqueueSpan := spanNamed(t, recorder, "event.enqueue")
	sc := extractSpanContext(rows[0].Traceparent)
	if sc.TraceID() != enqueueSpan.SpanContext().TraceID() {
		t.Errorf("row trace id = %s, want the enqueue span's %s", sc.TraceID(), enqueueSpan.SpanContext().TraceID())
	}
	if sc.SpanID() != enqueueSpan.SpanContext().SpanID() {
		t.Errorf("row span id = %s, want the enqueue span's %s — the link must resolve to this event's emit, not to the whole cycle", sc.SpanID(), enqueueSpan.SpanContext().SpanID())
	}
	if enqueueSpan.SpanKind() != trace.SpanKindProducer {
		t.Errorf("enqueue span kind = %v, want Producer", enqueueSpan.SpanKind())
	}
}

// TestIngestor_UntracedProducerStampsNothing is the degrade path, and the
// one that has to stay boring: with no provider installed (tracing off, the
// default everywhere) the row is enqueued exactly as before and simply
// carries no context to link.
func TestIngestor_UntracedProducerStampsNothing(t *testing.T) {
	bus, got := captureBus(t)
	conn := newIngestTestDB(t)
	queue := sqlitestore.New(conn).EventQueue
	ing := ingest.New(bus, queue, nil)

	entityID := seedIngestEntity(t, conn)
	ing.Publish(context.Background(), domain.Event{
		OrgID:     runmode.LocalDefaultOrgID,
		EntityID:  &entityID,
		EventType: domain.EventGitHubPRCICheckFailed,
	})

	rows, err := queue.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListForEntity: rows=%d err=%v", len(rows), err)
	}
	if rows[0].Traceparent != "" {
		t.Errorf("traceparent = %q, want empty with tracing disabled", rows[0].Traceparent)
	}
	if rows[0].Status != domain.QueuedEventStatusPending {
		t.Errorf("status = %q, want pending — an untraced event enqueues like any other", rows[0].Status)
	}
	// And the bus fan-out is unchanged, id stamped from the durable row.
	e := awaitEvent(t, got)
	if e.ID != rows[0].EventID {
		t.Errorf("bus event id = %q, want %q", e.ID, rows[0].EventID)
	}
}

// spanNamed returns the single ended span with the given name.
func spanNamed(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range recorder.Ended() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("recorded %d spans named %q, want 1", len(found), name)
	}
	return found[0]
}

// extractSpanContext parses a traceparent the way the router's consumer
// does, so the two halves of the hop are tested against one decoder.
func extractSpanContext(traceparent string) trace.SpanContext {
	ctx := propagation.TraceContext{}.Extract(
		context.Background(),
		propagation.MapCarrier{"traceparent": traceparent},
	)
	return trace.SpanContextFromContext(ctx)
}
