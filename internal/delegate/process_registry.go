// The live-process registry and the control-indirection seam.
//
// Every run executes as a long-lived agentproc.LiveRun (see run.go); while
// it runs, its handle lives in s.procs, keyed by run id, so a control
// request arriving on a later HTTP turn can reach the process the
// delegation goroutine spawned. The RunController interface is the seam
// between a control request (interrupt / steer / cancel) and that process:
// at N=1 the in-process impl resolves the handle here; horizontal scaling
// swaps it for a DB-signal to the executor that owns the run's lease
// (runs.executor_id), with no change at the call sites.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/hostmem"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// DefaultMaxConcurrentRuns is the conservative process-wide cap on how
// many runs execute off the dispatcher at once, so a burst of queued steps
// doesn't fan into an unbounded number of agent subprocesses. Tunable via
// SetMaxConcurrentRuns before the dispatcher starts; deployments set it
// with the TF_MAX_CONCURRENT_RUNS env var (see ParseMaxConcurrentRuns).
const DefaultMaxConcurrentRuns = 4

// MaxConcurrentRunsCeiling is the largest value the concurrency cap may
// take. It mirrors sandbox.MaxSandboxes, the sandbox subnet allocator's
// structural limit — each run owns a /24 out of one /16, so a higher
// dispatcher cap could never be honored.
const MaxConcurrentRunsCeiling = sandbox.MaxSandboxes

// ParseMaxConcurrentRuns interprets the TF_MAX_CONCURRENT_RUNS env value.
// Empty → the default. Non-numeric or < 1 → the default plus an error the
// caller logs (a bad value must not brick boot). Values above the sandbox
// ceiling clamp to it; clamped reports whether that happened, so the caller
// can log a distinct signal for "you asked for more than the sandbox
// allocator can honor" instead of it reading identically to an operator who
// set the ceiling value directly.
func ParseMaxConcurrentRuns(raw string) (n int, clamped bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultMaxConcurrentRuns, false, nil
	}
	n, err = strconv.Atoi(raw)
	if err != nil || n < 1 {
		return DefaultMaxConcurrentRuns, false, fmt.Errorf(
			"invalid TF_MAX_CONCURRENT_RUNS %q (want an integer in [1,%d]); using default %d",
			raw, MaxConcurrentRunsCeiling, DefaultMaxConcurrentRuns)
	}
	if n > MaxConcurrentRunsCeiling {
		return MaxConcurrentRunsCeiling, true, nil
	}
	return n, false, nil
}

// DefaultRunMemoryBudgetMB is the planning budget for one concurrent
// run: the fleet-measured ~155MB marginal cost of the agent engine
// (read-only engine pages are shared across sandboxes and amortize
// per-host) plus the node supervisor and transcript-growth headroom.
// See docs/sandbox-bench.md for the measurements behind it.
const DefaultRunMemoryBudgetMB = 256

// DefaultPlatformReserveMB is the host memory the capacity rule sets
// aside for everything that isn't a run: the TF binary, Postgres,
// GoTrue, the object store, and safety headroom the dispatcher's
// memory floor defends at runtime.
const DefaultPlatformReserveMB = 12288

// DerivedRunCapacity applies the sizing rule to a host's MemTotal:
// runs ≈ (RAM − reserve) ÷ budget, clamped to [0, ceiling]. This is
// advisory (boot log + over-provision warning) — the runtime
// protections are the concurrency cap and the dispatch memory floor.
func DerivedRunCapacity(totalMB int) int {
	if totalMB <= DefaultPlatformReserveMB {
		return 0
	}
	n := (totalMB - DefaultPlatformReserveMB) / DefaultRunMemoryBudgetMB
	if n > MaxConcurrentRunsCeiling {
		return MaxConcurrentRunsCeiling
	}
	return n
}

// DefaultDispatchMemFloorMB is the default MemAvailable floor below
// which the dispatcher defers claiming queued runs. Sized to the
// platform reserve the capacity sizing rule assumes (see
// docs/sandbox-bench.md): enough headroom that in-flight runs finish
// and the host never swaps, small enough not to strand capacity on
// modest hosts.
const DefaultDispatchMemFloorMB = 4096

// ParseDispatchMemFloorMB interprets the TF_DISPATCH_MEM_FLOOR_MB env
// value. Empty → the default. 0 → disabled (no gating). Negative or
// non-numeric → the default plus an error the caller logs; a bad value
// must not brick boot.
func ParseDispatchMemFloorMB(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultDispatchMemFloorMB, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return DefaultDispatchMemFloorMB, fmt.Errorf(
			"invalid TF_DISPATCH_MEM_FLOOR_MB %q (want an integer >= 0; 0 disables); using default %d",
			raw, DefaultDispatchMemFloorMB)
	}
	return n, nil
}

// SetDispatchMemFloor sets the dispatch memory guardrail (MiB of host
// MemAvailable below which claims are deferred). Call once at startup
// before RunDispatcher runs; 0 disables.
func (s *Spawner) SetDispatchMemFloor(mb int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memFloorMB = mb
}

// dispatchMemGated reports whether the memory guardrail should defer
// claiming right now, logging only on state transitions so a gated
// host doesn't emit a line per scan tick. Fails open: a disabled
// floor or an unreadable probe never gates.
func (s *Spawner) dispatchMemGated() bool {
	s.mu.Lock()
	floor := s.memFloorMB
	probe := s.memAvailMB
	s.mu.Unlock()
	if floor <= 0 || probe == nil {
		return false
	}
	avail := probe()
	if avail == hostmem.Unknown {
		return false
	}
	gated := avail < floor
	if s.memGated.CompareAndSwap(!gated, gated) {
		if gated {
			dispatchLog.Warn("dispatch paused: host memory below floor; queued runs deferred until it recovers",
				"available_mb", avail, "floor_mb", floor)
		} else {
			dispatchLog.Info("dispatch resumed: host memory recovered above floor",
				"available_mb", avail, "floor_mb", floor)
		}
	}
	return gated
}

// DefaultIdleHibernateTimeout is how long a live run may go quiet (no
// stream activity) before it hibernates to a durable resume. Capacity-
// driven, not a cache TTL — a warm-but-idle process holds an execution
// slot, not the server-side prompt cache. Tunable via
// SetIdleHibernateTimeout (tests inject a short value).
const DefaultIdleHibernateTimeout = 5 * time.Minute

// liveRunHandle wraps a run's live agent process plus the identity a
// control op needs to reach it. Held in s.procs for the lifetime of the
// run's live execution; the driver registers it when the process spawns
// and deregisters it when the process closes (terminal, hibernation, or
// cancel).
type liveRunHandle struct {
	lr    *agentproc.LiveRun
	runID string
	orgID string
}

// registerProc records a run's live process handle so control ops can
// reach it across HTTP turns. Mirrors the cancels-map registration the
// dispatcher and ResumeOpenRun already do, under the same s.mu.
func (s *Spawner) registerProc(orgID, runID string, lr *agentproc.LiveRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.procs[runID] = &liveRunHandle{lr: lr, runID: runID, orgID: orgID}
}

// getProc returns the live handle for a run, or nil when the run has no
// live process (never started, already hibernated, or terminated).
func (s *Spawner) getProc(runID string) *liveRunHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[runID]
}

// deregisterProc drops a run's live handle once the process is gone.
// Idempotent.
func (s *Spawner) deregisterProc(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.procs, runID)
}

// stampExecutor records this spawner instance's executor identity on the
// run row when the run goes live (an unguarded system write — the run
// goroutine holds no JWT claims). Best-effort: a failure is logged, not
// fatal, because executor ownership is a forward-compat hook, not a
// correctness gate at N=1.
func (s *Spawner) stampExecutor(orgID, runID string) {
	if s.agentRuns == nil {
		return
	}
	executorID, _ := s.executorIdentity()
	if err := s.agentRuns.SetExecutorSystem(context.Background(), orgID, runID, executorID); err != nil {
		delegateLog.Warn("stamp executor on run failed", "executor", executorID, "run_id", runID, "error", err)
	}
}

// SetExecutorID overrides this spawner's executor identity with the
// persistent instance-registry id + boot epoch main resolved at startup,
// replacing the constructor's random per-boot uuid fallback. Call once at
// startup, before RunDispatcher / RunInstanceHeartbeat start; tests that
// never call this keep NewSpawner's random default.
func (s *Spawner) SetExecutorID(id string, bootEpoch int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executorID = id
	s.bootEpoch = bootEpoch
}

// executorIdentity returns the current (executorID, bootEpoch) pair under
// lock — the read side of SetExecutorID, used by stampExecutor and the
// instance heartbeat loop.
func (s *Spawner) executorIdentity() (string, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.executorID, s.bootEpoch
}

// SetMaxConcurrentRuns resizes the off-dispatcher concurrency cap. Call
// once at startup before RunDispatcher runs; the in-flight goroutines
// close over the channel they acquired from, so a startup-time resize is
// safe. A value below 1 is clamped to 1.
func (s *Spawner) SetMaxConcurrentRuns(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runSem = make(chan struct{}, n)
}

// semaphore returns the current concurrency-cap channel. Callers capture
// it once and use the captured value for both acquire and release so a
// startup-time SetMaxConcurrentRuns can't strand a token on a replaced
// channel.
func (s *Spawner) semaphore() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runSem
}

// SetIdleHibernateTimeout overrides the idle-hibernation threshold. Tests
// inject a short value to drive the idle path deterministically.
func (s *Spawner) SetIdleHibernateTimeout(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idleHibernateTimeout = d
}

// idleTimeout returns the effective idle-hibernation threshold, falling
// back to the default when unset.
func (s *Spawner) idleTimeout() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleHibernateTimeout > 0 {
		return s.idleHibernateTimeout
	}
	return DefaultIdleHibernateTimeout
}

// RunController routes a live-process control op to wherever the run's
// process actually lives. Interrupt and Steer drive the warm process;
// Cancel signals the hard-kill ctx. At N=1 the in-process impl resolves
// the target from s.procs / s.cancels; horizontal scaling replaces it with
// a DB-signal to the executor that owns the run's lease, leaving the
// callers (Cancel, and P3's interrupt/steer endpoints) unchanged.
type RunController interface {
	// Interrupt stops the run's current turn, leaving the process alive
	// for further input. Errors when the run has no live process.
	Interrupt(ctx context.Context, runID string) error
	// Steer delivers a free-form user message to a live run. Errors when
	// the run has no live process.
	Steer(ctx context.Context, runID, text string) error
	// Cancel signals the run's process to terminate (SIGKILL via the
	// registered ctx cancel). Reports whether a live handle was found; a
	// false result means the run has no in-process goroutine and the
	// caller must take the DB-only terminal path.
	Cancel(runID string) (found bool)
}

// ErrNoLiveProcess is the typed error the control seam returns when a run has
// no live process to reach — it terminated, idle-hibernated, or never started.
// The P3 interrupt/message endpoints map it to 409 Conflict.
var ErrNoLiveProcess = errors.New("run has no live process")

// inProcessController is the N=1 RunController: every run's process lives
// in this same process, so the lookups are direct map reads.
type inProcessController struct{ s *Spawner }

func (c inProcessController) Interrupt(ctx context.Context, runID string) error {
	h := c.s.getProc(runID)
	if h == nil {
		return fmt.Errorf("interrupt run %s: %w", runID, ErrNoLiveProcess)
	}
	return h.lr.Interrupt(ctx)
}

func (c inProcessController) Steer(ctx context.Context, runID, text string) error {
	h := c.s.getProc(runID)
	if h == nil {
		return fmt.Errorf("steer run %s: %w", runID, ErrNoLiveProcess)
	}
	return h.lr.Send(ctx, text)
}

func (c inProcessController) Cancel(runID string) bool {
	c.s.mu.Lock()
	cancel, ok := c.s.cancels[runID]
	c.s.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// Interrupt stops a live run's current turn through the control seam,
// leaving the process alive for further input. The P3 message/pause
// endpoints call this; routing through s.controller is what keeps the
// horizontal-scaling swap additive.
func (s *Spawner) Interrupt(ctx context.Context, runID string) error {
	return s.controller.Interrupt(ctx, runID)
}

// Steer delivers a free-form user message to a live run through the
// control seam. The P3 message endpoint calls this.
func (s *Spawner) Steer(ctx context.Context, runID, text string) error {
	return s.controller.Steer(ctx, runID, text)
}
