package reaper

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

var reaperLog = logging.Component("reaper")

// Default knobs (spec's "Pre-made decisions — do not re-derive"): heartbeat
// 4s < self-fence 15s < reaper staleness 30s. TF_RUN_MAX_ATTEMPTS defaults
// to 2 (decision log #4) — distinct from delegate.maxRunAttempts (5), which
// caps in-process workspace-setup retries on the SAME executor; this knob
// caps executor-loss retries across a re-claim on a DIFFERENT executor. Two
// budgets, two values, one unit: both count the current episode of
// consecutive hand-backs, never the conversation's lifetime claims.
const (
	DefaultStaleThreshold = 30 * time.Second
	DefaultMaxAttempts    = 2

	// DefaultReapInterval is the leader reaper's tick — spec §"scope"
	// calls out "~10s tick".
	DefaultReapInterval = 10 * time.Second

	// DefaultGCStaleAfter is the registry tombstone threshold — decision
	// log #5's "registry tombstone GC at 7 days heartbeat-stale".
	DefaultGCStaleAfter = 7 * 24 * time.Hour
	// DefaultGCInterval is the registry GC's tick — spec §4.1(5) calls it
	// "daily".
	DefaultGCInterval = 24 * time.Hour
)

// ParseStaleThreshold parses TF_REAPER_STALE_SEC. Empty maps to
// DefaultStaleThreshold; anything else must parse as a positive integer
// second count. internal/app cross-validates the result against the
// executor self-fence deadline (TF_SELF_FENCE_SEC) — this function only
// knows its own env var.
func ParseStaleThreshold(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return DefaultStaleThreshold, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid TF_REAPER_STALE_SEC=%q (want a positive integer number of seconds)", raw)
	}
	return time.Duration(n) * time.Second, nil
}

// ParseMaxAttempts parses TF_RUN_MAX_ATTEMPTS. Empty maps to
// DefaultMaxAttempts; anything else must parse as a positive integer.
func ParseMaxAttempts(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return DefaultMaxAttempts, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid TF_RUN_MAX_ATTEMPTS=%q (want a positive integer)", raw)
	}
	return n, nil
}

// RunReaper is the leader reaper's tick loop: every interval, sweep runs
// whose owning executor's heartbeat has gone stale past staleThreshold,
// requeuing or terminal-failing them (attempts-capped at maxAttempts) or
// finalizing them cancelled (blueprint already cancel-requested) — see
// Store.ReapDeadExecutors. Brain-gated: callers start this only while
// holding the background-brain lease (internal/app's startBrain/stopBrain),
// and stop it on ctx cancellation the same way every other brain component
// does. A nil store makes this a logged no-op, mirroring
// Spawner.RunInstanceHeartbeat's nil-seam contract.
func RunReaper(ctx context.Context, store Store, interval time.Duration, staleThreshold time.Duration, maxAttempts int) {
	if store == nil {
		reaperLog.Warn("leader reaper not started: no Store wired")
		return
	}
	if interval <= 0 {
		interval = DefaultReapInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			counts, err := store.ReapDeadExecutors(ctx, staleThreshold, maxAttempts)
			if err != nil {
				reaperLog.Warn("reap sweep failed; retrying next tick", "error", err)
				continue
			}
			if counts.Requeued > 0 || counts.Failed > 0 || counts.Cancelled > 0 {
				reaperLog.Info("reaped dead-executor runs",
					"requeued", counts.Requeued, "failed_executor_lost", counts.Failed, "cancelled", counts.Cancelled)
			}
			// Curator homing (spec §6.3): retire turns stranded on a dead home so
			// they stop showing in-flight — the user's next turn re-homes.
			// Separate from ReapDeadExecutors: curator turns are curator_requests
			// rows, not runs, and the recovery is retire-only (no requeue).
			if n, cerr := store.CancelStrandedCuratorTurns(ctx, staleThreshold); cerr != nil {
				reaperLog.Warn("curator stranded-home sweep failed; retrying next tick", "error", cerr)
			} else if n > 0 {
				reaperLog.Info("cancelled curator turns stranded on a dead home", "count", n)
			}
			// Claim-desync janitor: heal the shape the app-pool terminal
			// writes can strand (see Store.HealClaimDesyncs). Periodic here so
			// a desync's lifetime is bounded by a tick, not the next restart.
			if released, herr := store.HealClaimDesyncs(ctx); herr != nil {
				reaperLog.Warn("claim-desync sweep failed; retrying next tick", "error", herr)
			} else if released > 0 {
				reaperLog.Info("healed run↔claim desyncs", "released_claims", released)
			}
		}
	}
}

// RunRegistryGC is the registry tombstone GC's tick loop: every interval,
// delete instances rows whose heartbeat is older than staleAfter — spec
// §4.1(5). Brain-gated and nil-seamed, same contract as RunReaper.
func RunRegistryGC(ctx context.Context, store Store, interval time.Duration, staleAfter time.Duration) {
	if store == nil {
		reaperLog.Warn("registry GC not started: no Store wired")
		return
	}
	if interval <= 0 {
		interval = DefaultGCInterval
	}
	sweep := func() {
		n, err := store.DeleteStaleInstances(ctx, staleAfter)
		if err != nil {
			reaperLog.Warn("registry GC sweep failed; retrying next tick", "error", err)
			return
		}
		if n > 0 {
			reaperLog.Info("registry GC deleted stale instance rows", "count", n, "stale_after", staleAfter)
		}
	}
	// Sweep once the moment the brain starts, then on the (long) interval. The
	// interval is a steady-state cadence, not a grace period: a freshly promoted
	// leader — or a restart of the only one — should reconcile the registry now
	// rather than let already-abandoned rows linger up to a full day for the
	// first tick.
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
