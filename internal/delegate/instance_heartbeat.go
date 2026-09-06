// The instance-registry heartbeat loop. Every ~4s it renews this
// instance's row in the fleet membership registry and refreshes the
// capacity + admission snapshot the dispatch memory guardrail otherwise
// kept process-local: host memory headroom, the dispatch memory gate, and
// the off-dispatcher concurrency semaphore's occupancy/cap.

package delegate

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/hostmem"
)

// DefaultInstanceHeartbeatInterval is how often RunInstanceHeartbeat
// renews this instance's registry row. Within the spec's ~3-5s window.
const DefaultInstanceHeartbeatInterval = 4 * time.Second

// DefaultSelfFenceDeadline is TF_SELF_FENCE_SEC's default: the own-
// monotonic-clock deadline since the last successful heartbeat WRITE past
// which an instance self-fences the partition case (spec §4.3's pre-made
// numbers — 4s heartbeat < 15s self-fence < 30s reaper staleness).
const DefaultSelfFenceDeadline = 15 * time.Second

// defaultHeartbeatWriteTimeout caps how long a single heartbeat WRITE may
// block before heartbeatOnce treats it as a failure — the bound that makes
// the partition self-fence actually able to fire. Without it, a heartbeat
// call carries the caller's ctx unmodified (no deadline of its own), so a
// silently black-holed connection (a partition with no RST — packets just
// vanish, as opposed to a refused/reset connection, which fails fast on its
// own) can hang the call indefinitely: the ticker loop stalls, and
// checkPartitionSelfFence never runs at all, defeating the whole mechanism
// — the reaper can requeue this instance's claimed runs while it sits here
// still "trying" forever. See heartbeatWriteTimeout for how this scales
// with a configured TF_SELF_FENCE_SEC.
const defaultHeartbeatWriteTimeout = 5 * time.Second

// heartbeatWriteTimeout derives the per-call write timeout from the
// configured self-fence deadline: capped at defaultHeartbeatWriteTimeout so
// a single hung write can never block for minutes, and floored at 1s so an
// aggressively short TF_SELF_FENCE_SEC (tests, or an unusually tight
// deployment) doesn't shrink it to something that fails healthy-but-slow
// writes. A third of the deadline leaves room for at least a couple of
// timed-out attempts before the deadline itself elapses — a SINGLE timeout
// consuming the whole deadline would let one hung write silently absorb
// the entire tolerance window with no chance to retry.
func (s *Spawner) heartbeatWriteTimeout() time.Duration {
	d := s.selfFenceDeadlineOrDefault() / 3
	if d > defaultHeartbeatWriteTimeout {
		return defaultHeartbeatWriteTimeout
	}
	if d < time.Second {
		return time.Second
	}
	return d
}

// ParseSelfFenceDeadline parses TF_SELF_FENCE_SEC. Empty maps to
// DefaultSelfFenceDeadline; anything else must parse as a positive integer
// second count. internal/app cross-validates the result against the
// reaper's staleness threshold (TF_REAPER_STALE_SEC) — this function only
// knows its own env var, mirroring internal/lease's per-knob parsers.
func ParseSelfFenceDeadline(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return DefaultSelfFenceDeadline, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid TF_SELF_FENCE_SEC=%q (want a positive integer number of seconds)", raw)
	}
	return time.Duration(n) * time.Second, nil
}

// SetSelfFenceDeadline installs the partition self-fence deadline. Zero (the
// NewSpawner default) falls back to DefaultSelfFenceDeadline at check time —
// see selfFenceDeadlineOrDefault.
func (s *Spawner) SetSelfFenceDeadline(d time.Duration) {
	s.mu.Lock()
	s.selfFenceDeadline = d
	s.mu.Unlock()
}

func (s *Spawner) selfFenceDeadlineOrDefault() time.Duration {
	s.mu.Lock()
	d := s.selfFenceDeadline
	s.mu.Unlock()
	if d <= 0 {
		return DefaultSelfFenceDeadline
	}
	return d
}

// SetOnSupersessionFence wires the callback fenceIdentity invokes, once,
// synchronously, right after killing this instance's live sandboxes on a
// supersession — the second half of fence completion (spec §4.1(4)).
// internal/app wires this to a loud log line + os.Exit with a distinct
// code, keeping process-lifecycle decisions out of this package. nil (the
// default) is a safe no-op.
func (s *Spawner) SetOnSupersessionFence(f func()) {
	s.mu.Lock()
	s.onSupersessionFence = f
	s.mu.Unlock()
}

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

	// Baseline for the partition self-fence's elapsed-time measurement
	// (checkPartitionSelfFence): "time since we last had good contact with
	// the registry, OR since this loop started trying" — set here rather
	// than left at zero so a process that fails EVERY heartbeat from boot
	// (partitioned from birth) still self-fences at the deadline instead of
	// never triggering (lastHeartbeatWriteNanos alone would stay 0 forever
	// in that case, which is indistinguishable from "no deadline yet").
	s.mu.Lock()
	s.lastGoodContactAt = time.Now()
	s.mu.Unlock()

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
// see domain.InstanceHeartbeat), and reads back the operator-intent
// draining flag so this instance learns a drain within one heartbeat
// interval. Returns false when the renewal matched no row: another
// process re-registered this instance id with a newer boot_epoch — a
// duplicated state root (cloned volume, copied ~/.triagefactory, a second
// host mounting the same directory over a share) that the boot-time flock
// cannot see. That is the split-identity case: two live processes now own
// one identity, and the newer boot's self-sweep will requeue rows this
// process is still executing. fenceIdentity() reacts: stop claiming,
// refuse resumes, kill this process's live sandboxes (the "existing
// cancel machinery" fence completion re-uses), then exit — see
// fenceIdentity and SetOnSupersessionFence.
//
// A heartbeat WRITE FAILURE (the error branch, distinct from the
// zero-rows-matched supersession case above) instead feeds
// checkPartitionSelfFence: the reaper can't tell a partitioned executor
// from a dead one, so past a deadline this instance self-fences the same
// way — but reversibly, and without exiting (see PartitionFenced).
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

	// Bound the write itself — see defaultHeartbeatWriteTimeout for why
	// this is load-bearing, not just hygiene: without it, a silently
	// black-holed connection can hang this call indefinitely and the
	// self-fence deadline below never gets evaluated at all.
	hbCtx, cancel := context.WithTimeout(ctx, s.heartbeatWriteTimeout())
	matched, draining, err := s.instances.Heartbeat(hbCtx, id, bootEpoch, hb)
	cancel()
	if err != nil {
		dispatchLog.Warn("instance heartbeat failed", "instance", id, "error", err)
		s.mu.Lock()
		elapsed := time.Since(s.lastGoodContactAt)
		s.mu.Unlock()
		s.checkPartitionSelfFence(id, elapsed)
		return true // transient DB trouble is not evidence of supersession
	}
	if !matched {
		s.fenceIdentity(id, bootEpoch)
		return false
	}

	// A successful write is not, by itself, proof this instance was never
	// partitioned: if the write took long enough to land AFTER the
	// self-fence deadline had already elapsed since our last known-good
	// contact (a slow-but-not-fully-severed connection, or this goroutine
	// itself stalled for a while before the call returned), the reaper may
	// already have requeued this instance's claimed runs while the write
	// was still in flight. Measure the gap BEFORE overwriting the
	// baseline, so a late success still gets caught and reacts exactly
	// like a live-detected partition (kill sandboxes) instead of silently
	// resuming as if nothing happened.
	s.mu.Lock()
	gap := time.Since(s.lastGoodContactAt)
	s.lastGoodContactAt = time.Now()
	s.mu.Unlock()
	s.checkPartitionSelfFence(id, gap)

	// Record the successful write time for the executor healthz probe
	// (last_heartbeat_write_age_sec).
	s.lastHeartbeatWriteNanos.Store(time.Now().UnixNano())
	if s.partitionFenced.CompareAndSwap(true, false) {
		dispatchLog.Warn("instance un-fenced: heartbeat write succeeded again after a partition — resuming claims", "instance", id)
	}
	// Log the drain flip so an operator's CLI drain/resume shows up in the
	// executor's own log. CompareAndSwap fires only on a real transition.
	switch {
	case draining && s.draining.CompareAndSwap(false, true):
		dispatchLog.Info("instance draining: operator asked this executor to quiesce — no new conversations will be claimed; live engagements finish or park at their turn end", "instance", id)
	case !draining && s.draining.CompareAndSwap(true, false):
		dispatchLog.Info("instance resumed: drain cleared — claiming new conversations again", "instance", id)
	}
	return true
}

// fenceIdentity latches the identity fence: this process's (id,
// boot_epoch) no longer owns its registry row, so it must not start any
// new work stamped with that identity. Sticky by design — there is no
// un-fence short of a restart (which re-registers and mints a fresh
// epoch); a transient cause (a DB restored from backup) also requires the
// restart to re-register, so auto-clearing would only mask the split-
// identity case the fence exists for. Idempotent (CompareAndSwap): the
// kill + exit sequence below runs exactly once per process life,
// regardless of how many further heartbeats observe the same supersession.
//
// Fence completion (spec §4.1(4), second half): kill every live sandbox
// via the same cancel machinery Cancel(conversationID) uses, THEN invoke
// onSupersessionFence (wired by internal/app to a loud log + os.Exit with
// a distinct code) — so a zombie's in-flight work can't keep running and
// double the reaper-requeued attempt's external writes. Ordered
// deliberately: kill before exit, never the reverse.
func (s *Spawner) fenceIdentity(id string, bootEpoch int64) {
	if !s.identityFenced.CompareAndSwap(false, true) {
		return
	}
	dispatchLog.Error("instance identity superseded — fencing: no new conversations will be claimed or resumed by this process; restart it to re-register. Likely cause: a second process booted from a copy of this state root (cloned volume / duplicated data directory), or the database was restored from a backup.",
		"instance", id, "boot_epoch", bootEpoch)
	killed := s.killAllLiveSandboxes()
	dispatchLog.Warn("killed live sandboxes after identity supersession", "instance", id, "count", killed)
	s.mu.Lock()
	onFence := s.onSupersessionFence
	s.mu.Unlock()
	if onFence != nil {
		onFence()
	}
}

// IdentityFenced reports whether the identity fence has latched. Consulted
// by the dispatcher's claim loop and the resume paths; everything else
// (live runs, HTTP serving, the control-plane half of an =all process)
// continues untouched.
func (s *Spawner) IdentityFenced() bool {
	return s.identityFenced.Load()
}

// checkPartitionSelfFence reacts once elapsed — the caller-computed time
// since this instance's last known-good contact with the registry
// (lastGoodContactAt: a successful write, or loop start) — crosses
// selfFenceDeadline: this instance self-fences the partition case, same
// reaction as fenceIdentity (kill live sandboxes, stop claiming), but
// reversible (heartbeatOnce un-fences on the next successful write, often
// the very next line after the call that triggered this) and never exits:
// connectivity may return, and a restart would lose warm worktrees for
// nothing. Idempotent per episode via CompareAndSwap — a burst of
// deadline-crossing calls kills sandboxes only once, not on every one.
//
// elapsed is measured by the caller (heartbeatOnce) against
// lastGoodContactAt — a plain time.Time under s.mu, deliberately NOT an
// atomic.Int64 of UnixNano: time.Time obtained from time.Now() carries a
// monotonic-clock reading, and time.Since keeps using it as long as
// neither value round-trips through a wall-clock representation (Unix/
// UnixNano) first. Round-tripping would silently fall back to wall-clock
// subtraction — vulnerable to NTP steps and manual clock changes, exactly
// the kind of "own monotonic clock, same discipline as leader demotion"
// guarantee this deadline depends on (see internal/lease, which keeps its
// own lastSuccessfulRenewal as a time.Time for the identical reason).
func (s *Spawner) checkPartitionSelfFence(id string, elapsed time.Duration) {
	deadline := s.selfFenceDeadlineOrDefault()
	if elapsed < deadline {
		return
	}
	if !s.partitionFenced.CompareAndSwap(false, true) {
		return
	}
	killed := s.killAllLiveSandboxes()
	dispatchLog.Error("instance heartbeat exceeded the self-fence deadline — fencing: live sandboxes killed to avoid a duplicate execution; the reaper may already have requeued this instance's claimed conversations",
		"instance", id, "self_fence_deadline", deadline, "elapsed_since_last_good_contact", elapsed, "sandboxes_killed", killed)
}

// PartitionFenced reports whether the partition self-fence has latched.
// Unlike IdentityFenced this is NOT sticky: heartbeatOnce clears it
// automatically the next time a heartbeat write succeeds. Consulted by the
// dispatcher's claim loop alongside IdentityFenced.
func (s *Spawner) PartitionFenced() bool {
	return s.partitionFenced.Load()
}

// killAllLiveSandboxes SIGKILLs every run this instance currently has a
// live process for, via the same registered ctx-cancel handle
// inProcessController.Cancel uses for a single run (process_registry.go) —
// fence completion (spec §4.1(4)) reuses the existing cancel machinery
// rather than inventing a second kill path. Returns the count killed. A
// control pod (which never populates s.cancels for delegated runs — it
// runs no dispatcher) or an executor with nothing in flight kills zero,
// harmlessly. The goroutine actually running each cancelled step observes
// ctx.Err() and parks its own run (parkConversationOpen) exactly as a
// user-initiated Cancel does; this function only fires the signal.
func (s *Spawner) killAllLiveSandboxes() int {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.cancels))
	for _, cancel := range s.cancels {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels)
}
