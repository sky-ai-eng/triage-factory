package db

import (
	"context"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestOpenTracedNestsStatementSpansUnderCaller is the load-bearing test for
// the driver-level wrap: it proves a store query — which passes ctx and
// knows nothing about tracing — lands as a child of whatever span the
// caller had open. That nesting is the entire reason the instrumentation
// sits at the driver rather than around the store interface, and it is not
// something the type system can check.
func TestOpenTracedNestsStatementSpansUnderCaller(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	handle, err := OpenTraced("sqlite", filepath.Join(t.TempDir(), "trace.db"), PoolLocal)
	if err != nil {
		t.Fatalf("OpenTraced: %v", err)
	}
	defer handle.Close()

	ctx, parent := otel.Tracer("db/test").Start(context.Background(), "caller")
	if _, err := handle.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("exec: %v", err)
	}
	parent.End()

	var stmt sdktrace.ReadOnlySpan
	for _, s := range recorder.Ended() {
		if s.SpanContext().SpanID() == parent.SpanContext().SpanID() {
			continue
		}
		if s.Parent().SpanID() == parent.SpanContext().SpanID() {
			stmt = s
			break
		}
	}
	if stmt == nil {
		t.Fatal("no statement span parented to the caller's span; the driver wrap is not reaching store queries")
	}

	want := map[attribute.Key]string{
		"db.system.name": "sqlite",
		"db.pool":        PoolLocal,
	}
	got := map[attribute.Key]string{}
	for _, kv := range stmt.Attributes() {
		got[kv.Key] = kv.Value.AsString()
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("statement span %s = %q, want %q", k, got[k], v)
		}
	}
}

// TestOpenTracedSuppressesPingSpans covers the option that keeps /readyz
// from drowning the trace backend: a ping is on a fixed probe interval
// forever and describes none of TF's work.
func TestOpenTracedSuppressesPingSpans(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	handle, err := OpenTraced("sqlite", filepath.Join(t.TempDir(), "ping.db"), PoolLocal)
	if err != nil {
		t.Fatalf("OpenTraced: %v", err)
	}
	defer handle.Close()

	ctx, parent := otel.Tracer("db/test").Start(context.Background(), "caller")
	if err := handle.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	parent.End()

	for _, s := range recorder.Ended() {
		if s.Name() == "sql.conn.ping" {
			t.Error("a ping produced a span; /readyz pings would out-number every other span in the process")
		}
	}
}
