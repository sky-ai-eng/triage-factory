// The instance-registry heartbeat loop. Every ~4s it renews this
// instance's row in the fleet membership registry and refreshes the
// capacity + admission snapshot the dispatch memory guardrail otherwise
// kept process-local: host memory headroom, the dispatch memory gate, and
// the off-dispatcher concurrency semaphore's occupancy/cap.

package delegate

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/hostmem"
)

// DefaultInstanceHeartbeatInterval is how often RunInstanceHeartbeat
// renews this instance's registry row. Within the spec's ~3-5s window.
const DefaultInstanceHeartbeatInterval = 4 * time.Second

// RunInstanceHeartbeat periodically renews this instance's row in the
// fleet registry until ctx is cancelled, or until a renewal proves this
// process's identity has been superseded (see heartbeatOnce), at which
// point the loop fences the instance and exits. A nil InstanceStore (no
// store wired) or an unset executor identity (SetExecutorID never called —
// the test / no-seam path) makes this a logged no-op; production always
// calls SetExecutorID before starting this loop. Purely observational on
// the happy path: it never mutates dispatch/admission state, only reports
// it — the identity fence is the one deliberate exception.
//
// A non-positive interval falls back to DefaultInstanceHeartbeatInterval
// rather than reaching time.NewTicker directly — NewTicker panics on a
// non-positive duration, and this is a background loop a caller could
// plausibly start with a zero-value or misconfigured interval.
func (s *Spawner) RunInstanceHeartbeat(ctx context.Context, interval time.Duration) {
	if s.instances == nil {
		dispatchLog.Warn("instance heartbeat not started: no InstanceStore wired")
		return
	}
	if id, _ := s.executorIdentity(); id == "" {
		dispatchLog.Warn("instance heartbeat not started: no executor identity set (SetExecutorID was never called)")
		return
	}
	if interval <= 0 {
		dispatchLog.Warn("instance heartbeat interval must be positive; using the default",
			"requested", interval, "default", DefaultInstanceHeartbeatInterval)
		interval = DefaultInstanceHeartbeatInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.heartbeatOnce(ctx) {
				return
			}
		}
	}
}

// heartbeatOnce renews the current instance row once: last_heartbeat_at
// plus a fresh capacity + admission snapshot (liveness + capacity only —
// operator-intent columns like draining/labels are never written here,
// see domain.InstanceHeartbeat). Returns false when the renewal matched
// no row: another process re-registered this instance id with a newer
// boot_epoch — a duplicated state root (cloned volume, copied
// ~/.triagefactory, a second host mounting the same directory over a
// share) that the boot-time flock cannot see. That is the split-identity
// case: two live processes now own one identity, and the newer boot's
// self-sweep will requeue rows this process is still executing. The only
// safe reaction is to stop STARTING work on the superseded identity —
// fenceIdentity() latches new claims and resumes off — and stop
// heartbeating (renewing a row that isn't ours anymore would fight the
// newer boot). In-flight runs are left to finish: killing them is the
// fleet-reaper phase's self-fence (spec §4.3), which owns sandbox
// teardown; until then a finished duplicate is absorbed the same way
// reaper-requeued duplicates are (artifact upserts, branch-push
// semantics).
func (s *Spawner) heartbeatOnce(ctx context.Context) bool {
	id, bootEpoch := s.executorIdentity()
	if id == "" {
		return false
	}

	// Capacity + admission snapshot — written only when this role backs
	// the numbers with a real dispatcher (executor/all). A pure-control
	// pod (SetReportCapacity(false)) heartbeats liveness alone, leaving
	// those executor-only columns NULL.
	var hb domain.InstanceHeartbeat
	if s.reportCapacity.Load() {
		sem := s.semaphore()
		maxRuns, activeRuns := cap(sem), len(sem)
		gated := s.dispatchMemGated()
		hb.MaxRuns = &maxRuns
		hb.ActiveRuns = &activeRuns
		hb.DispatchGated = &gated
		if total := hostmem.TotalMB(); total != hostmem.Unknown {
			hb.MemTotalMB = &total
		}
		s.mu.Lock()
		probe := s.memAvailMB
		s.mu.Unlock()
		if probe != nil {
			if avail := probe(); avail != hostmem.Unknown {
				hb.MemAvailableMB = &avail
			}
		}
	}

	matched, err := s.instances.Heartbeat(ctx, id, bootEpoch, hb)
	if err != nil {
		dispatchLog.Warn("instance heartbeat failed", "instance", id, "error", err)
		return true // transient DB trouble is not evidence of supersession
	}
	if !matched {
		s.fenceIdentity(id, bootEpoch)
		return false
	}
	// Record the successful write time for the executor healthz probe
	// (last_heartbeat_write_age_sec).
	s.lastHeartbeatWriteNanos.Store(time.Now().UnixNano())
	return true
}

// fenceIdentity latches the identity fence: this process's (id,
// boot_epoch) no longer owns its registry row, so it must not start any
// new work stamped with that identity. Sticky by design — there is no
// un-fence short of a restart (which re-registers and mints a fresh
// epoch); a transient cause (a DB restored from backup) also requires the
// restart to re-register, so auto-clearing would only mask the split-
// identity case the fence exists for. Idempotent; logs once.
func (s *Spawner) fenceIdentity(id string, bootEpoch int64) {
	if s.identityFenced.CompareAndSwap(false, true) {
		dispatchLog.Error("instance identity superseded — fencing: no new runs will be claimed or resumed by this process; restart it to re-register. Likely cause: a second process booted from a copy of this state root (cloned volume / duplicated data directory), or the database was restored from a backup.",
			"instance", id, "boot_epoch", bootEpoch)
	}
}

// IdentityFenced reports whether the identity fence has latched. Consulted
// by the dispatcher's claim loop and the resume paths; everything else
// (live runs, HTTP serving, the control-plane half of an =all process)
// continues untouched.
func (s *Spawner) IdentityFenced() bool {
	return s.identityFenced.Load()
}
