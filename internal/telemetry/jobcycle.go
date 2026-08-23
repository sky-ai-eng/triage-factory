package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// jobTracer is shared by the three background runners: the helper below is
// where their spans are created, so a per-package tracer would name an
// owner that never calls Start.
var jobTracer = otel.Tracer("internal/telemetry/jobcycle")

// StartJobCycle opens the root span for one per-org background job cycle —
// scorer or repo profiler. Both are the same
// shape (a per-org Runner with a single-flight guard, woken by a trigger
// channel), so one helper beats two span setups that would drift.
//
// A fresh root, never a child: Manager.Trigger(orgID) coalesces N poll
// completions into one signal, so there is no single caller to descend
// from and picking one would misattribute the cycle. org.id is the join
// instead. job is an attribute rather than part of the name so the three
// stay one comparable series; the caller ends the span.
func StartJobCycle(ctx context.Context, job, orgID string) (context.Context, trace.Span) {
	return jobTracer.Start(ctx, "systemjob.cycle",
		trace.WithAttributes(Job(job), OrgID(orgID)))
}
