package reconcile

import (
	"context"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// traceRecorder is installed on the OTel global once for the package: the job
// cycle tracer resolves through the global delegate, which binds to the first
// provider installed, so a recorder installed per test would capture nothing
// after the first.
var (
	traceOnce     sync.Once
	traceRecorder = tracetest.NewSpanRecorder()
)

// recordSpans returns a function reporting the spans ended since it was
// called.
func recordSpans(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	traceOnce.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(traceRecorder)))
	})
	start := len(traceRecorder.Ended())
	return func() []sdktrace.ReadOnlySpan { return traceRecorder.Ended()[start:] }
}

// spanAttr is the string value of key on s, and whether s carries it.
func spanAttr(s sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// Every reconcile cycle is the root of its own trace, so its DB reads and the
// RefreshPRs batch have a parent to hang from. A completed cycle reports no
// outcome (it returns only an error); a shutdown mid-cycle is an anticipated
// stop, and only a real failure carries an error status.
func TestRunner_CycleSpan(t *testing.T) {
	cases := []struct {
		name        string
		closeDB     bool
		cancelled   bool
		wantOutcome string
		wantError   bool
	}{
		{name: "completed"},
		// A closed connection fails the artifact list with ctx still live.
		{name: "failed", closeDB: true, wantError: true},
		{name: "shutdown", cancelled: true, wantOutcome: "shutdown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, stores, _, _ := reconcileTestDB(t)
			rc := NewReconciler(&fakeResolver{client: newStubClient(t, &stubGH{})}, stores.Artifacts, stores.ExternalActions, stores.Entities, stores.Repos, stores.Orgs, nil)
			if c.closeDB {
				_ = conn.Close()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if c.cancelled {
				cancel()
			}

			spans := recordSpans(t)
			NewRunner(rc, runmode.LocalDefaultOrgID).run(ctx)

			var cycles []sdktrace.ReadOnlySpan
			for _, s := range spans() {
				if s.Name() == "systemjob.cycle" {
					cycles = append(cycles, s)
				}
			}
			if len(cycles) != 1 {
				t.Fatalf("%d systemjob.cycle spans for one cycle; want 1", len(cycles))
			}
			s := cycles[0]
			if job, _ := spanAttr(s, "job"); job != "reconciler" {
				t.Errorf("job = %q; want reconciler", job)
			}
			if org, _ := spanAttr(s, "org.id"); org != runmode.LocalDefaultOrgID {
				t.Errorf("org.id = %q; want %q", org, runmode.LocalDefaultOrgID)
			}
			if outcome, _ := spanAttr(s, "outcome"); outcome != c.wantOutcome {
				t.Errorf("outcome = %q; want %q", outcome, c.wantOutcome)
			}
			if gotError := s.Status().Code == codes.Error; gotError != c.wantError {
				t.Errorf("error status = %v; want %v", gotError, c.wantError)
			}
		})
	}
}
