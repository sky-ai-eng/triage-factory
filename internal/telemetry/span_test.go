package telemetry

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestRecordSpanErrorExportsTheMessageNotTheError covers the rule the helper
// exists for: the status says which stage failed, in the caller's constant
// words, and nothing on the span holds the error's text.
func TestRecordSpanErrorExportsTheMessageNotTheError(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	_, span := tp.Tracer("test").Start(context.Background(), "restore")
	RecordSpanError(span, errors.New("rehydrate: restore acme/secret-project/main: exit status 128"), "restoring the workspace failed")
	span.End()

	got := recorder.Ended()[0]
	if got.Status().Code != codes.Error {
		t.Errorf("status code = %v, want Error", got.Status().Code)
	}
	if got.Status().Description != "restoring the workspace failed" {
		t.Errorf("status description = %q, want the caller's message", got.Status().Description)
	}
	if events := got.Events(); len(events) != 0 {
		t.Errorf("recorded %d span events, want 0; an exception event holds the error's text", len(events))
	}
}

func TestRecordSpanErrorIgnoresNil(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	_, span := tp.Tracer("test").Start(context.Background(), "step")
	RecordSpanError(span, nil, "the step failed")
	span.End()

	if got := recorder.Ended()[0].Status().Code; got != codes.Unset {
		t.Errorf("status code = %v, want Unset for a nil error", got)
	}
}
