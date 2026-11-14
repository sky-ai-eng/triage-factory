package credprovision

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestProvision_SpanRecordsReason: each seal's credentials.provision span
// carries the reason its caller gave, so a trace can tell the executor's
// request from either sweep's re-seal.
func TestProvision_SpanRecordsReason(t *testing.T) {
	// The package tracer binds to the first provider the global is given and
	// never rebinds, so this is installed once for the test binary; no other
	// test here installs one.
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))

	for _, reason := range []Reason{ReasonCredRequest, ReasonAwaitingSweep, ReasonRefreshSweep} {
		t.Run(string(reason), func(t *testing.T) {
			rig := newProvisionRig(t, eventsource.Availability{
				eventsource.NewSource(eventsource.KindGitHub, eventsource.StateAvailable),
			}, &installTokens{})
			start := len(recorder.Ended())
			if err := rig.m.ProvisionForConversation(context.Background(), "org-1", "conv-1", reason); err != nil {
				t.Fatalf("ProvisionForConversation: %v", err)
			}
			var got []string
			for _, s := range recorder.Ended()[start:] {
				if s.Name() != "credentials.provision" {
					continue
				}
				for _, a := range s.Attributes() {
					if a.Key == "reason" {
						got = append(got, a.Value.AsString())
					}
				}
			}
			if len(got) != 1 || got[0] != string(reason) {
				t.Errorf("credentials.provision reason = %q, want exactly [%q]", got, reason)
			}
		})
	}
}
