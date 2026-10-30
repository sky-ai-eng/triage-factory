package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestTracedTransportPassesThroughOutsideATrace covers the rule that a
// client span exists only inside a trace a stage root opened. A call from
// an untraced background pass gets no span and no traceparent: the span
// would export as a one-span trace, and the header would hand the upstream
// a trace id nothing on TF's side records.
func TestTracedTransportPassesThroughOutsideATrace(t *testing.T) {
	recorder, srv, seen := tracedTransportFixture(t)

	getThroughTracedTransport(t, context.Background(), srv)

	if n := len(recorder.Started()); n != 0 {
		t.Errorf("parentless request started %d spans, want 0", n)
	}
	if got := <-seen; got != "" {
		t.Errorf("upstream saw traceparent %q on a parentless request, want none", got)
	}
}

// TestTracedTransportTracesInsideATrace is the other half: under a
// caller's span the request gets its client span, parented there, and
// propagates it.
func TestTracedTransportTracesInsideATrace(t *testing.T) {
	recorder, srv, seen := tracedTransportFixture(t)

	ctx, parent := otel.Tracer("telemetry/test").Start(context.Background(), "caller")
	getThroughTracedTransport(t, ctx, srv)
	parent.End()

	var client sdktrace.ReadOnlySpan
	for _, s := range recorder.Ended() {
		if s.Name() == "test.http" {
			if client != nil {
				t.Fatal("more than one test.http span for one request")
			}
			client = s
		}
	}
	if client == nil {
		t.Fatal("no test.http span recorded")
	}
	if client.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("test.http parent = %s, want the caller's span %s", client.Parent().SpanID(), parent.SpanContext().SpanID())
	}
	if got := <-seen; got == "" {
		t.Error("upstream saw no traceparent on a request made inside a trace")
	}
}

// tracedTransportFixture installs a recording provider and the W3C
// propagator explicitly rather than through InitTraces, and returns a
// server that reports each request's traceparent header.
func tracedTransportFixture(t *testing.T) (*tracetest.SpanRecorder, *httptest.Server, <-chan string) {
	t.Helper()
	restoreTraceGlobals(t)
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	otel.SetTextMapPropagator(propagation.TraceContext{})

	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("traceparent")
	}))
	t.Cleanup(srv.Close)
	return recorder, srv, seen
}

// getThroughTracedTransport builds its transport after the fixture
// installs the propagator, because otelhttp reads the global propagator
// when the transport is constructed.
func getThroughTracedTransport(t *testing.T, ctx context.Context, srv *httptest.Server) {
	t.Helper()
	client := &http.Client{Transport: TracedTransport(srv.Client().Transport, "test")}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
}
