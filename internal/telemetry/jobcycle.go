package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// jobTracer is shared by the three background runners rather than each
// owning one, because the helper below is where their spans are actually
// created — a per-package tracer would name an owner that never calls Start.
var jobTracer = otel.Tracer("internal/telemetry/jobcycle")

// StartJobCycle opens the root span for one per-org background job cycle:
// the scorer, the repo profiler, or the project classifier. All three have
// the same shape — a per-org Runner with a single-flight guard, woken by a
// trigger channel — so they get one helper rather than three near-identical
// span setups that would drift.
//
// A fresh root, never a child. These runners are woken by
// Manager.Trigger(orgID), which coalesces: N poll completions collapse into
// one signal, so there is no single caller to descend from and picking one
// would misattribute the cycle to whichever poll happened to win the race.
// The org id is the join instead — a cycle and the poll that provoked it
// share it, and correlating on that is exactly the pattern this epic uses
// everywhere a boundary coalesces.
//
// job goes on an attribute rather than into the span name so the three
// runners stay one comparable series; the caller ends the span.
func StartJobCycle(ctx context.Context, job, orgID string) (context.Context, trace.Span) {
	return jobTracer.Start(ctx, "systemjob.cycle",
		trace.WithAttributes(Job(job), OrgID(orgID)))
}
