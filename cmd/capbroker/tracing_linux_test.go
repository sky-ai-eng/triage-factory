//go:build linux

package capbroker

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// One provider per test binary — the package tracer is created at init and
// the OTel global wires it to the first provider installed, never re-wiring.
var traceRecorder = tracetest.NewSpanRecorder()

func init() {
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(traceRecorder)))
}

func recordSpans(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	start := len(traceRecorder.Ended())
	return func() []sdktrace.ReadOnlySpan { return traceRecorder.Ended()[start:] }
}

func brokerSpans(spans []sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "capbroker.call" {
			out = append(out, s)
		}
	}
	return out
}

func opAttr(t *testing.T, s sdktrace.ReadOnlySpan) string {
	t.Helper()
	for _, kv := range s.Attributes() {
		if string(kv.Key) == "op" {
			return kv.Value.AsString()
		}
	}
	t.Fatalf("capbroker.call span carries no op attribute (has %v)", s.Attributes())
	return ""
}

// TestIPCCallIsTracedClientSide: the executor-side span is the ONLY record of
// what the broker did. The broker holds CAP_SYS_ADMIN/CAP_NET_ADMIN and must
// never gain an outbound telemetry dependency, so it exports nothing itself
// — which makes this span the whole signal for a slow host operation.
//
// The span descends from the caller's context, so a bring-up's broker calls
// land inside the engagement's trace rather than as orphan roots.
func TestIPCCallIsTracedClientSide(t *testing.T) {
	read := recordSpans(t)
	client := serveTestBroker(t, &fakeOps{
		chownRunTreeFn: func(ctx context.Context, root, subpath string) error { return nil },
	})

	parentCtx, parent := otel.Tracer("test").Start(context.Background(), "engagement.setup")
	if err := client.ChownRunTree(parentCtx, "/tmp/tf-runs/run-1", "owner/repo"); err != nil {
		t.Fatalf("ChownRunTree: %v", err)
	}
	parent.End()

	got := brokerSpans(read())
	if len(got) != 1 {
		t.Fatalf("capbroker.call spans = %d, want 1 — every IPC method must be covered by the one choke point", len(got))
	}
	if op := opAttr(t, got[0]); op != methodChownRunTree {
		t.Errorf("op = %q, want %q", op, methodChownRunTree)
	}
	if got[0].SpanKind() != trace.SpanKindClient {
		t.Errorf("span kind = %v, want Client — this is a call INTO another process", got[0].SpanKind())
	}
	if got[0].Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("the broker call did not descend from its caller; a bring-up's IPC timings would orphan out of the engagement trace")
	}
	if got[0].Status().Code == codes.Error {
		t.Error("a successful call recorded an error status")
	}
}

// TestIPCCallRecordsBrokerRefusal: a broker-side validation rejection is a
// real failure and must reach the trace with an error status. The broker
// refusing to chown /etc is the whole point of the privilege split, and it
// surfaces here as the only span either process produces for it.
func TestIPCCallRecordsBrokerRefusal(t *testing.T) {
	read := recordSpans(t)
	client := serveTestBroker(t, &fakeOps{
		removeRunTreeFn: func(ctx context.Context, path string) error {
			return errors.New("refusing to remove a path outside a run tree")
		},
	})

	if err := client.RemoveRunTree(context.Background(), "/etc"); err == nil {
		t.Fatal("expected the broker's refusal to propagate")
	}

	got := brokerSpans(read())
	if len(got) != 1 {
		t.Fatalf("capbroker.call spans = %d, want 1", len(got))
	}
	if got[0].Status().Code != codes.Error {
		t.Errorf("status = %v, want Error", got[0].Status().Code)
	}
	if op := opAttr(t, got[0]); op != methodRemoveRunTree {
		t.Errorf("op = %q, want %q", op, methodRemoveRunTree)
	}
}
