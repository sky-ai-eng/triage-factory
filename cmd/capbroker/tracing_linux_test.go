//go:build linux

package capbroker

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
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

// TestTeardownCallsStayInTheRunsTrace covers the calls that are hardest to
// attribute and matter most when they misbehave: killing and waiting on a
// sandboxed child.
//
// All of them are deliberately detached from the run's cancellation —
// teardown has to complete precisely when the run context is already dead —
// and the obvious spelling of that (context.Background()) drops the trace
// context along with it. That costs nothing until a span is created here, at
// which point every kill and wait mints a brand-new parentless trace carrying
// one attribute: the method name. A hung teardown would then be findable only
// by timestamp proximity to a run you already suspected.
//
// context.WithoutCancel keeps the values and drops only the cancellation, so
// these land in the run's own trace while the detachment stays byte-identical.
func TestTeardownCallsStayInTheRunsTrace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		run    func(t *testing.T, client *IPCClient, runCtx context.Context)
	}{
		{
			name:   "brokerRun.Close kills the child",
			method: methodKillRun,
			run: func(t *testing.T, client *IPCClient, runCtx context.Context) {
				r := &brokerRun{client: client, ctx: runCtx, started: true,
					params: sandbox.LaunchParams{ContainerID: "tf-teardown-1"}}
				_ = r.Close()
			},
		},
		{
			name:   "brokerRun.Wait fetches the exit status",
			method: methodWaitRun,
			run: func(t *testing.T, client *IPCClient, runCtx context.Context) {
				r := &brokerRun{client: client, ctx: runCtx, started: true,
					params: sandbox.LaunchParams{ContainerID: "tf-teardown-2"}}
				_ = r.Wait()
			},
		},
		{
			name:   "sidecarHandle.Close kills the sidecar",
			method: methodKillSidecar,
			run: func(t *testing.T, client *IPCClient, runCtx context.Context) {
				h := &sidecarHandle{client: client, ctx: runCtx, containerID: "tf-teardown-3"}
				_ = h.Close()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := recordSpans(t)
			client := serveTestBroker(t, &fakeOps{})

			// The run's context, already CANCELLED — which is the normal state
			// by the time teardown runs, and the exact condition under which a
			// naive re-parenting would break the RPC instead of tracing it.
			runCtx, span := otel.Tracer("test").Start(context.Background(), "agent.jail.launch")
			runCtx, cancel := context.WithCancel(runCtx)
			cancel()
			span.End() // the setup span is long over by teardown time

			tc.run(t, client, runCtx)

			var got sdktrace.ReadOnlySpan
			for _, s := range brokerSpans(read()) {
				if opAttr(t, s) == tc.method {
					got = s
				}
			}
			if got == nil {
				t.Fatalf("no capbroker.call span for %s", tc.method)
			}
			if !got.Parent().IsValid() {
				t.Fatalf("%s minted a parentless trace; a teardown that hung would be findable only by timestamp", tc.method)
			}
			if got.Parent().SpanID() != span.SpanContext().SpanID() {
				t.Errorf("%s parented to %v, want the run's span %v", tc.method, got.Parent().SpanID(), span.SpanContext().SpanID())
			}
			if got.SpanContext().TraceID() != span.SpanContext().TraceID() {
				t.Errorf("%s landed in trace %v, want the run's %v", tc.method, got.SpanContext().TraceID(), span.SpanContext().TraceID())
			}
		})
	}
}

// TestTeardownSurvivesACancelledRunContext is the other half: the detachment
// itself, which the parentage fix must not quietly cost.
//
// The run context is dead by the time teardown runs — that is the normal
// case, not the edge one — so if WithoutCancel were ever "simplified" to the
// run ctx, every kill would fail at the dial and the broker would leak the
// child. Asserted on the span's status rather than on the error text: the
// dial's wording is Go's ("operation was canceled"), not ours, and a test
// that pins somebody else's string tests the wrong thing.
func TestTeardownSurvivesACancelledRunContext(t *testing.T) {
	read := recordSpans(t)
	client := serveTestBroker(t, &fakeOps{})

	runCtx, cancel := context.WithCancel(context.Background())
	cancel()

	r := &brokerRun{client: client, ctx: runCtx, started: true,
		params: sandbox.LaunchParams{ContainerID: "tf-cancelled"}}
	_ = r.Close()

	var got sdktrace.ReadOnlySpan
	for _, s := range brokerSpans(read()) {
		if opAttr(t, s) == methodKillRun {
			got = s
		}
	}
	if got == nil {
		t.Fatal("no KillRun span at all — Close did not attempt the teardown")
	}
	if got.Status().Code == codes.Error {
		t.Fatalf("the teardown RPC was aborted by the run's own cancellation (%q); "+
			"the broker's child would leak", got.Status().Description)
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
