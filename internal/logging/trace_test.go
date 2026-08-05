package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestTraceCorrelation covers the logs↔traces join in both directions: a
// record emitted under a span carries that span's IDs, and one emitted
// outside a span carries no trace attributes at all — an absent trace_id is
// the signal that a log line has no trace to jump to, so it must never be
// stamped with an empty or zero-valued one.
//
// The span context is built through the trace API rather than the SDK: the
// handler's whole input is the context's span context, so an SDK provider
// would add a sampler, a batcher, and an exporter to the test without
// changing what is under test — and would cost internal/logging a test-time
// dependency on the SDK it deliberately doesn't have.
func TestTraceCorrelation(t *testing.T) {
	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
	)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    mustTraceID(t, traceID),
		SpanID:     mustSpanID(t, spanID),
		TraceFlags: trace.FlagsSampled,
	})

	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{
			name: "under a span",
			ctx:  trace.ContextWithSpanContext(context.Background(), sc),
			want: true,
		},
		{
			name: "outside a span",
			ctx:  context.Background(),
			want: false,
		},
		{
			// What every call site looks like with tracing disabled: the
			// no-op provider hands back a span whose context is the zero
			// value, so the handler must recognize it as "no trace" rather
			// than stamping 32 zeros.
			name: "under a no-op provider's span",
			ctx:  noopSpanCtx(),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			t.Cleanup(func() { slog.SetDefault(prev) })
			slog.SetDefault(slog.New(traceHandler{slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: levelVar})}))

			Component("router").InfoContext(tc.ctx, "task bumped")

			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("decode record: %v (%q)", err, buf.String())
			}
			if !tc.want {
				if _, ok := rec["trace_id"]; ok {
					t.Errorf("unexpected trace_id on a record with no span: %v", rec)
				}
				if _, ok := rec["span_id"]; ok {
					t.Errorf("unexpected span_id on a record with no span: %v", rec)
				}
				return
			}
			if rec["trace_id"] != traceID {
				t.Errorf("trace_id = %v, want %s", rec["trace_id"], traceID)
			}
			if rec["span_id"] != spanID {
				t.Errorf("span_id = %v, want %s", rec["span_id"], spanID)
			}
			// The correlation attributes are additive — the existing
			// component tagging still has to survive the wrapper.
			if rec["component"] != "router" {
				t.Errorf("component = %v, want router", rec["component"])
			}
		})
	}
}

// TestTraceCorrelationWiredIntoDefault verifies the handler is actually in
// the process-wide chain (and composes with procHandler), not merely
// implemented — the package's init is the only place that wires it.
func TestTraceCorrelationWiredIntoDefault(t *testing.T) {
	t.Cleanup(func() { SetProcess("") })
	var buf bytes.Buffer
	restore := SetOutput(&buf)
	defer restore()

	SetProcess("orchestrator")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    mustTraceID(t, "4bf92f3577b34da6a3ce929d0e0e4736"),
		SpanID:     mustSpanID(t, "00f067aa0ba902b7"),
		TraceFlags: trace.FlagsSampled,
	}))
	Component("poller").InfoContext(ctx, "poll cycle done")

	out := buf.String()
	for _, want := range []string{
		"trace_id=4bf92f3577b34da6a3ce929d0e0e4736",
		"span_id=00f067aa0ba902b7",
		"component=poller",
		"proc=orchestrator",
	} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("default logger output missing %q: %q", want, out)
		}
	}

	// slog only carries a context on the *Context variants; a bare Info
	// under the same span has nothing to read, and stamping it would take
	// a guess this package refuses to take.
	buf.Reset()
	Component("poller").Info("poll cycle done")
	if bytes.Contains(buf.Bytes(), []byte("trace_id")) {
		t.Errorf("bare Info picked up a trace_id from somewhere: %q", buf.String())
	}
}

func noopSpanCtx() context.Context {
	ctx, span := noop.NewTracerProvider().Tracer("logging/test").Start(context.Background(), "disabled")
	span.End()
	return ctx
}

func mustTraceID(t *testing.T, s string) trace.TraceID {
	t.Helper()
	id, err := trace.TraceIDFromHex(s)
	if err != nil {
		t.Fatalf("trace id %q: %v", s, err)
	}
	return id
}

func mustSpanID(t *testing.T, s string) trace.SpanID {
	t.Helper()
	id, err := trace.SpanIDFromHex(s)
	if err != nil {
		t.Fatalf("span id %q: %v", s, err)
	}
	return id
}
