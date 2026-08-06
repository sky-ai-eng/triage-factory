package routing

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// One provider for the whole test binary, and a cursor per test.
//
// This is not a preference. A package-level tracer — internal/routing's, and
// every other subsystem's — is created at init, and the OTel global wires
// such a tracer to the FIRST provider installed in the process and never
// re-wires it (a sync.Once inside the API). Per-test install/restore
// therefore quietly sends the second test's spans to the first test's
// recorder, and the suite starts passing or failing by ordering. Installing
// once and reading a slice of what each test produced keeps the tests
// independent for the reason that actually holds.
var (
	traceOnce     sync.Once
	traceRecorder = tracetest.NewSpanRecorder()
)

// recordSpans arms tracing (once) and returns a reader for the spans that
// end after this call — this test's spans, and nobody else's, since the
// package runs its tests sequentially.
func recordSpans(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	traceOnce.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(traceRecorder)))
		// The propagator is what turns a span into a traceparent; Init
		// installs it on the same enabled path this stands in for.
		otel.SetTextMapPropagator(propagation.TraceContext{})
	})
	start := len(traceRecorder.Ended())
	return func() []sdktrace.ReadOnlySpan { return traceRecorder.Ended()[start:] }
}

func endedNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func spanAttr(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// TestRouteEventSpan_LinksTheProducer is the consume half of the durable
// hop: routing a row that carries a producer's trace context yields a span
// that LINKS to it — never a child of it, because one poll cycle emits many
// events routed later and possibly elsewhere, and parenting them would grow
// one unbounded trace instead of one per event.
func TestRouteEventSpan_LinksTheProducer(t *testing.T) {
	spans := recordSpans(t)
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	entity, _, err := st.Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "owner/repo#traced", "pr", "PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	enqueueCIFailed(t, database, entity.ID)

	// A producer span, stamped onto the row exactly as the ingestor does.
	_, producer := otel.Tracer("routing/test").Start(context.Background(), "event.enqueue")
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(trace.ContextWithSpan(context.Background(), producer), carrier)
	producer.End()
	if _, err := database.Exec(`UPDATE event_queue SET traceparent = ?`, carrier["traceparent"]); err != nil {
		t.Fatalf("stamp traceparent: %v", err)
	}

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	roots := endedNamed(spans(), "route.event")
	if len(roots) != 1 {
		t.Fatalf("recorded %d route.event spans, want 1", len(roots))
	}
	root := roots[0]
	if root.Parent().IsValid() {
		t.Error("route.event has a parent; it must be a root — a poll cycle's trace must not swallow every event it emitted")
	}
	if len(root.Links()) != 1 {
		t.Fatalf("route.event has %d links, want 1 (the producer)", len(root.Links()))
	}
	if got := root.Links()[0].SpanContext; got.TraceID() != producer.SpanContext().TraceID() || got.SpanID() != producer.SpanContext().SpanID() {
		t.Errorf("link = %s/%s, want the producer's %s/%s",
			got.TraceID(), got.SpanID(), producer.SpanContext().TraceID(), producer.SpanContext().SpanID())
	}
	if root.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("route.event kind = %v, want Consumer", root.SpanKind())
	}

	// The row's own bookkeeping, readable from the span: which event, which
	// tenant, which attempt, and how long it waited to be claimed — the one
	// interval that is over before the span begins.
	for _, key := range []string{"event.id", "event.type", "org.id", "attempt", "queue.wait_ms"} {
		if _, ok := spanAttr(root, key); !ok {
			t.Errorf("route.event carries no %s", key)
		}
	}
	if v, ok := spanAttr(root, "disposition"); !ok || v.AsString() != "task_created" {
		t.Errorf("disposition = %v (present=%v), want task_created", v.AsString(), ok)
	}
}

// TestRouteEventSpan_NoTraceparentRoutesWithoutLink pins the degrade: a row
// enqueued by an untraced producer (tracing off at emit, an older row) is
// routed identically and simply has nothing to point at.
func TestRouteEventSpan_NoTraceparentRoutesWithoutLink(t *testing.T) {
	spans := recordSpans(t)
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	entity, _, err := st.Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "owner/repo#untraced", "pr", "PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	enqueueCIFailed(t, database, entity.ID)

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	roots := endedNamed(spans(), "route.event")
	if len(roots) != 1 {
		t.Fatalf("recorded %d route.event spans, want 1", len(roots))
	}
	if n := len(roots[0].Links()); n != 0 {
		t.Errorf("route.event has %d links, want 0 for an untraced producer", n)
	}
	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != 1 {
		t.Errorf("done rows = %d, want 1 — a row with no trace context routes like any other", done)
	}
	if tasks := countRows(t, database, `SELECT COUNT(*) FROM tasks`); tasks != 1 {
		t.Errorf("tasks = %d, want 1", tasks)
	}
}

// TestRouteEventSpan_StagesAreChildren pins that a routed event's trace
// reads as the pipeline it is: every stage the event actually reached is a
// child span of the root, so "where did the time go" and "how far did this
// event get" are the same question.
func TestRouteEventSpan_StagesAreChildren(t *testing.T) {
	spans := recordSpans(t)
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	entity, _, err := st.Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "owner/repo#stages", "pr", "PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	enqueueCIFailed(t, database, entity.ID)

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	roots := endedNamed(spans(), "route.event")
	if len(roots) != 1 {
		t.Fatalf("recorded %d route.event spans, want 1", len(roots))
	}
	rootCtx := roots[0].SpanContext()

	var stages []string
	for _, s := range spans() {
		if strings.HasPrefix(s.Name(), "route.") && s.Name() != "route.event" {
			if s.Parent().SpanID() != rootCtx.SpanID() {
				t.Errorf("%s is not a child of route.event", s.Name())
			}
			stages = append(stages, s.Name())
		}
	}
	for _, want := range []string{"route.close", "route.match", "route.own", "route.task", "route.triggers"} {
		if !slices.Contains(stages, want) {
			t.Errorf("no %s span; recorded stages: %v", want, stages)
		}
	}
}

// TestProducerLink_MalformedTraceparentIsNoLink pins that a corrupt or
// truncated value degrades to "no producer" rather than to a link nothing
// resolves — the two are indistinguishable in a backend, so they must be
// one state in code.
func TestProducerLink_MalformedTraceparentIsNoLink(t *testing.T) {
	recordSpans(t)
	for _, tp := range []string{
		"",
		"garbage",
		"00-00000000000000000000000000000000-0000000000000000-01", // all-zero ids
		"00-4bf92f3577b34da6a3ce929d0e0e4736",                     // truncated
	} {
		if _, ok := producerLink(tp); ok {
			t.Errorf("producerLink(%q) built a link; want none", tp)
		}
	}
}
