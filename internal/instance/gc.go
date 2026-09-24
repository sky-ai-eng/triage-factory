package instance

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

var gcLog = logging.Component("instance")

// RegistryGCStaleAfter is how long an instance row may go without a heartbeat
// before the registry GC deletes it. Long enough that no process that could
// still come back is ever removed: an executor stopped for a week re-registers
// at boot epoch 1 with nothing lost, because no claim references the row.
const RegistryGCStaleAfter = 7 * 24 * time.Hour

// RegistryGCInterval is the registry GC's steady-state cadence.
const RegistryGCInterval = 24 * time.Hour

// RunRegistryGC deletes instance rows whose heartbeat is older than
// olderThan, once as soon as it starts and then every interval, until ctx is
// cancelled. The first sweep is immediate because the interval is a cadence,
// not a grace period: a freshly started brain — a newly promoted leader, or a
// restart of the only one — reconciles the registry now rather than letting
// abandoned rows linger up to a full interval.
//
// Brain-gated by its caller: one process sweeps at a time. A nil store makes
// this a logged no-op.
func RunRegistryGC(ctx context.Context, store db.InstanceStore, olderThan, interval time.Duration) {
	if store == nil {
		gcLog.Warn("registry GC not started: no InstanceStore wired")
		return
	}
	if interval <= 0 {
		interval = RegistryGCInterval
	}
	sweep := func() {
		n, err := store.DeleteStaleSystem(ctx, olderThan)
		if err != nil {
			gcLog.Warn("registry GC sweep failed; retrying next tick", "error", err)
			return
		}
		if n > 0 {
			gcLog.Info("registry GC deleted stale instance rows", "count", n, "stale_after", olderThan)
		}
	}
	select {
	case <-ctx.Done():
		return
	default:
		sweep()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
