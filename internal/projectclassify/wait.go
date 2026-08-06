package projectclassify

import (
	"context"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

// DefaultWaitTimeout is the spawner's deadline for a fresh
// classification before a delegation proceeds without project KB.
// 90 seconds gives generous headroom for headless `claude` cold-start
// (~5-15s) plus the classification call (~3-8s), with a wide margin for
// a slow or queued org. Pathological cases still resolve via the
// timeout rather than hanging the spawner indefinitely.
const DefaultWaitTimeout = 90 * time.Second

// pollInterval is how often WaitFor checks classified_at. The read is
// a single indexed point-lookup on either backend (sub-millisecond on
// SQLite, a cheap admin-pool query on Postgres), so 1s is essentially
// free; finer granularity wouldn't materially change behavior given
// that classifications take seconds.
const pollInterval = 1 * time.Second

// WaitFor blocks until the entity has been classified (classified_at
// IS NOT NULL), the entity row vanishes, ctx is cancelled, or the
// timeout elapses — whichever fires first. Calls trigger(orgID) once on
// entry to ensure the classifier wakes up even if no post-poll trigger has
// fired for this entity yet.
//
// trigger is a caller-supplied kick rather than a bound *Manager.Trigger
// so this works on every role (TFAC-583): a control pod that currently
// holds the background-brain lease can trigger in-process, a standby
// control pod or an EXECUTOR (which builds no local classifier Manager at
// all) instead relays the trigger over tf_ctl to whichever pod IS the
// holder — see internal/app's triggerClassifier. Either way, WaitFor
// itself only ever POLLS the DB for the classification result; it never
// assumes trigger's effect lands in this process.
//
// The classification read goes through the dialect-aware EntityStore's
// ClassificationStatusSystem (the admin-pool variant), because WaitFor
// runs in the background spawner with no request JWT claims and the app
// pool would RLS-deny the read in multi mode. orgID scopes both the read
// and the trigger to the run's tenant; the spawner call site supplies it
// from the run config.
//
// Always returns — never propagates error to the caller. The caller
// (typically the spawner's setup path) proceeds with whatever
// project_id is currently on the row.
//
// Intended call site: spawner setup, just before reading
// entity.project_id to inject project knowledge into the worktree.
func WaitFor(ctx context.Context, entities db.EntityStore, trigger func(orgID string), orgID, entityID string, timeout time.Duration) {
	if entityID == "" || entities == nil || trigger == nil {
		return
	}
	// An empty orgID can never be classified: the probe below is org-scoped
	// and trigger is expected to drop an empty org (Manager.Trigger does),
	// so without this guard WaitFor would poll the full timeout waiting for
	// a kick that can never fire. Every call site has the run's orgID by
	// construction, so an empty value is a caller bug worth surfacing
	// rather than stalling on.
	if orgID == "" {
		classifyLog.Warn("waitfor called with empty org; cannot kick classification, returning early", "entity", entityID)
		return
	}
	// Respect an already-cancelled context at entry: skip the doomed
	// classification probe and the spurious trigger() kick that would
	// otherwise fire before the loop's <-ctx.Done() arm returns. (The kick
	// is idempotent and harmless, but waking the classifier on a shutdown
	// path is pointless work.) Mid-wait cancellation is still handled by
	// the loop below.
	if ctx.Err() != nil {
		return
	}
	// Blocks the delegation spawner for up to the caller's timeout — 90s
	// in production — and surfaces only as unexplained spawn latency. The
	// outcome is what makes it answerable: "classified" means the wait did
	// its job, "timeout" means the run proceeded without project context,
	// and from the outside the two look identical.
	ctx, span := tracer.Start(ctx, "projectclassify.wait",
		trace.WithAttributes(telemetry.OrgID(orgID), telemetry.EntityID(entityID)))
	defer span.End()

	done, gone := classificationState(ctx, entities, orgID, entityID)
	if gone {
		span.SetAttributes(telemetry.Outcome("entity_gone"))
		classifyLog.InfoContext(ctx, "waitfor entity not found, returning early", "entity", entityID)
		return
	}
	if done {
		span.SetAttributes(telemetry.Outcome("already_classified"))
		return
	}
	trigger(orgID)

	// NewTimer + NewTicker rather than time.After in a loop so the
	// timers stop cleanly on ctx-cancel (no garbage timers firing
	// later) and we don't allocate a fresh timer per iteration. Hot
	// path — called once per delegated run setup.
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			span.SetAttributes(telemetry.Outcome("cancelled"))
			return
		case <-timer.C:
			span.SetAttributes(telemetry.Outcome("timeout"))
			classifyLog.WarnContext(ctx, "waitfor timed out, proceeding without project context", "entity", entityID, "timeout", timeout)
			return
		case <-ticker.C:
			done, gone := classificationState(ctx, entities, orgID, entityID)
			if gone {
				span.SetAttributes(telemetry.Outcome("entity_gone"))
				classifyLog.InfoContext(ctx, "waitfor entity vanished mid-wait, returning early", "entity", entityID)
				return
			}
			if done {
				span.SetAttributes(telemetry.Outcome("classified"))
				return
			}
		}
	}
}

// classificationState probes one entity's classification via the
// dialect-aware admin-pool read and maps the result onto WaitFor's two
// decisions:
//
//   - done: classified_at IS NOT NULL — the wait can stop with project
//     context available.
//   - gone: the row does not exist — a deleted/unknown entity will never
//     be classified, so the caller stops polling early.
//
// A transient read error returns (false, false) and is logged, so the
// caller keeps polling rather than short-circuiting the wait on a DB
// blip — preserving the prior behavior where any error was treated
// as "still pending." Context cancellation/deadline is the one error we
// don't log: it's a deliberate shutdown signal (the loop's <-ctx.Done()
// arm handles the return), not a DB blip, so logging it as "transient"
// would be misleading.
func classificationState(ctx context.Context, entities db.EntityStore, orgID, entityID string) (done, gone bool) {
	classified, exists, err := entities.ClassificationStatusSystem(ctx, orgID, entityID)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			classifyLog.Warn("waitfor transient read error", "entity", entityID, "error", err)
		}
		return false, false
	}
	if !exists {
		return false, true
	}
	return classified, false
}
