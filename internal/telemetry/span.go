package telemetry

import (
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// RecordSpanError gives span an error status with message as its
// description, and does nothing for a nil error, so an instrumented step
// reads as one statement rather than a branch repeated at every exit.
//
// message names the stage that failed ("restoring the workspace failed"),
// never the error, and is a constant: span_callsite_test.go fails on one that
// is not. An error's text is tenant data: a git failure's output names
// remotes, refs, and run-tree paths, and a wrapped restore error names the
// repository. A span is exported to a backend the operator may not run, the
// same egress boundary attrs.go holds attributes to. For the same reason
// no exception event is recorded: span.RecordError would put err.Error() in
// the event's exception.message. A site with a closed vocabulary for why it
// failed sets Reason beside the status.
//
// The text belongs in the log, which stamps the span's trace_id on a record
// logged through a *Context call with a ctx that carries the span. Each error
// is logged that way once: at this call, unless err goes back to a caller in
// the same trace that logs it.
func RecordSpanError(span trace.Span, err error, message string) {
	if err == nil {
		return
	}
	span.SetStatus(codes.Error, message)
}
