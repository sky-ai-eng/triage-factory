package agenthost

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracer owns this package's spans. Conventions in internal/telemetry.
//
// Only the RelayServer's dispatch is instrumented, and only where it runs:
// in the ORCHESTRATOR, serving ops the sidecar relayed to it. The same
// package also compiles into the jail-cli, which is a pure RPC client with
// no provider installed — so those spans are no-ops there, and nothing this
// package writes to a socket carries trace context.
var tracer = otel.Tracer("cmd/exec/agenthost")

// startRelayOp opens the span for one relayed op: a fresh root LINKED to the
// engagement that built this server, never a child of it.
//
// Linked because the parent is long gone. The engagement's setup span ends
// at agent-live, and a relayed op is the agent doing its work minutes or
// hours later — exactly the unbounded streaming period standing decision 5
// keeps spans out of. A link records where the op came from while leaving it
// independently exportable, so a crash mid-run costs the op's span and
// nothing that already left.
//
// The op is an attribute rather than part of the span name so the whole
// relay surface stays one comparable series; both halves of the value are Go
// constants from the dispatch switch, so it stays an enum.
func (s *RelayServer) startRelayOp(ctx context.Context, name, namespace, op string) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithAttributes(
			telemetry.Op(namespace+"."+op),
			telemetry.ConversationID(s.info.RunID),
			telemetry.OrgID(s.info.OrgID),
		),
	}
	if s.engagement.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: s.engagement}))
	}
	return tracer.Start(ctx, name, opts...)
}

// recordSpanError puts err on span with an error status, and does nothing
// for a nil error.
func recordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
