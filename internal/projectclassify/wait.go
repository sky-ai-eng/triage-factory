package projectclassify

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// DefaultWaitTimeout is the spawner's deadline for a fresh
// classification before a delegation proceeds without project KB.
// 90 seconds gives generous headroom for headless `claude` cold-start
// (~5-15s) plus Stage 1 (~3-8s) and a single Stage 2 escalation
// (~10-30s) with margin. Pathological cases still resolve via the
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
// timeout elapses — whichever fires first. Triggers the runner once
// on entry to ensure the classifier wakes up even if no post-poll
// trigger has fired for this entity yet.
//
// The classification read goes through the runner's dialect-aware
// EntityStore (ClassificationStatusSystem) rather than a raw *sql.DB —
// the System (admin-pool) variant, because WaitFor runs in the
// background spawner with no request JWT claims and the app pool would
// RLS-deny the read in multi mode. orgID scopes the read to the run's
// tenant; the spawner call site supplies it from the run config.
//
// Always returns — never propagates error to the caller. The caller
// (typically the spawner's setup path) proceeds with whatever
// project_id is currently on the row.
//
// Intended call site: spawner setup, just before reading
// entity.project_id to inject project knowledge into the worktree.
func WaitFor(ctx context.Context, runner *Runner, orgID, entityID string, timeout time.Duration) {
	if entityID == "" || runner == nil {
		return
	}
	done, gone := classificationState(ctx, runner.entities, orgID, entityID)
	if gone {
		log.Printf("[classify] WaitFor: entity %s not found — returning early", entityID)
		return
	}
	if done {
		return
	}
	runner.Trigger()

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
			return
		case <-timer.C:
			log.Printf("[classify] WaitFor timed out for entity %s after %s — proceeding without project context", entityID, timeout)
			return
		case <-ticker.C:
			done, gone := classificationState(ctx, runner.entities, orgID, entityID)
			if gone {
				log.Printf("[classify] WaitFor: entity %s vanished mid-wait — returning early", entityID)
				return
			}
			if done {
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
// blip — preserving the pre-SKY-392 behavior where any error was treated
// as "still pending." Context cancellation/deadline is the one error we
// don't log: it's a deliberate shutdown signal (the loop's <-ctx.Done()
// arm handles the return), not a DB blip, so logging it as "transient"
// would be misleading.
func classificationState(ctx context.Context, entities db.EntityStore, orgID, entityID string) (done, gone bool) {
	classified, exists, err := entities.ClassificationStatusSystem(ctx, orgID, entityID)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("[classify] WaitFor: transient read error for entity %s: %v", entityID, err)
		}
		return false, false
	}
	if !exists {
		return false, true
	}
	return classified, false
}
