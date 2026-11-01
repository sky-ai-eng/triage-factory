package agentproc

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel"
)

// tracer owns this package's spans. Conventions in internal/telemetry.
//
// Everything here runs in the ORCHESTRATOR. The sidecar and the jail are
// instrumented only from this side — as client spans around the calls into
// them — because neither may hold an OTLP exporter (standing decision 1: an
// outbound trace connection in the sidecar would breach its own fail-closed
// egress posture), and no frame this package writes carries trace context.
var tracer = otel.Tracer("internal/agentproc")

// ensureSDKTraced is EnsureSDK under a span. Worth one on a call that is
// almost always a no-op: the first run on a cold image installs Node and
// the Agent SDK here, so this is where an otherwise inexplicable
// minutes-long gap between "claimed" and "agent started" comes from, and
// the warm case costs an empty span nobody has to explain.
//
// A wrapper rather than a ctx parameter on EnsureSDK, whose callers
// (including the installer CLI) have no run context to give it.
func ensureSDKTraced(ctx context.Context) (string, error) {
	_, span := tracer.Start(ctx, "agent.sdk.ensure")
	defer span.End()
	path, err := EnsureSDK()
	telemetry.RecordSpanError(span, err)
	return path, err
}
