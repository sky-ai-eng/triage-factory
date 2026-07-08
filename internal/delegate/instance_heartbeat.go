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
// fleet registry until ctx is cancelled. A nil InstanceStore (no store
// wired) or an unset executor identity (SetExecutorID never called — the
// test / no-seam path) makes this a logged no-op; production always calls
// SetExecutorID before starting this loop. Purely observational: it never
// mutates dispatch/admission state, only reports it.
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
			s.heartbeatOnce(ctx)
		}
	}
}

// heartbeatOnce renews the current instance row once: last_heartbeat_at
// plus a fresh capacity + admission snapshot. draining is always false
// today — the drain flag lands with a later fleet-reaper phase.
func (s *Spawner) heartbeatOnce(ctx context.Context) {
	id, bootEpoch := s.executorIdentity()
	if id == "" {
		return
	}

	sem := s.semaphore()
	maxRuns, activeRuns := cap(sem), len(sem)
	gated := s.dispatchMemGated()

	hb := domain.InstanceHeartbeat{
		Draining:      false,
		MaxRuns:       &maxRuns,
		ActiveRuns:    &activeRuns,
		DispatchGated: &gated,
	}
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

	matched, err := s.instances.Heartbeat(ctx, id, bootEpoch, hb)
	if err != nil {
		dispatchLog.Warn("instance heartbeat failed", "instance", id, "error", err)
		return
	}
	if !matched {
		dispatchLog.Warn("instance heartbeat found no matching row — a newer boot of this instance id may have superseded it",
			"instance", id, "boot_epoch", bootEpoch)
	}
}
