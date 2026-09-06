// The live-process registry and the control-indirection seam.
//
// Every run executes as a long-lived agentproc.LiveRun (see run.go); while
// it runs, its handle lives in s.procs, keyed by run id, so a control
// request arriving on a later HTTP turn can reach the process the
// delegation goroutine spawned. The RunController interface is the seam
// between a control request (interrupt / steer / cancel) and that process:
// at N=1 the in-process impl resolves the handle here; horizontal scaling
// swaps it for a DB-signal to the executor that owns the run's lease
// (claims.executor_id), with no change at the call sites.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/hostmem"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// DefaultMaxConcurrentClaims is the process-wide cap on how many claims
// execute off the dispatcher at once, so a burst of queued steps doesn't
// fan into an unbounded number of agent subprocesses. 8 comfortably fits
// the ~256 MB/claim planning budget on ordinary hardware while still
// throttling API spend. Tunable via SetMaxConcurrentClaims before the
// dispatcher starts; deployments set it with the TF_MAX_CONCURRENT_CLAIMS
// env var (see ParseMaxConcurrentClaims).
const DefaultMaxConcurrentClaims = 8

// MaxConcurrentClaimsCeiling is the largest value the concurrency cap may
// take. It mirrors sandbox.MaxSandboxes, the sandbox subnet allocator's
// structural limit — each run owns a /24 out of one /16, so a higher
// dispatcher cap could never be honored.
const MaxConcurrentClaimsCeiling = sandbox.MaxSandboxes

// ParseMaxConcurrentClaims interprets the TF_MAX_CONCURRENT_CLAIMS env value.
// Empty → the default. Non-numeric or < 1 → the default plus an error the
// caller logs (a bad value must not brick boot). Values above the sandbox
// ceiling clamp to it; clamped reports whether that happened, so the caller
// can log a distinct signal for "you asked for more than the sandbox
// allocator can honor" instead of it reading identically to an operator who
// set the ceiling value directly.
func ParseMaxConcurrentClaims(raw string) (n int, clamped bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultMaxConcurrentClaims, false, nil
	}
	n, err = strconv.Atoi(raw)
	if err != nil || n < 1 {
		return DefaultMaxConcurrentClaims, false, fmt.Errorf(
			"invalid TF_MAX_CONCURRENT_CLAIMS %q (want an integer in [1,%d]); using default %d",
			raw, MaxConcurrentClaimsCeiling, DefaultMaxConcurrentClaims)
	}
	if n > MaxConcurrentClaimsCeiling {
		return MaxConcurrentClaimsCeiling, true, nil
	}
	return n, false, nil
}

// DefaultClaimMemoryBudgetMB is the planning budget for one concurrent
// run: the fleet-measured ~155MB marginal cost of the agent engine
// (read-only engine pages are shared across sandboxes and amortize
// per-host) plus the node supervisor and transcript-growth headroom.
// See docs/benchmarks/sandbox-bench.md for the measurements behind it.
const DefaultClaimMemoryBudgetMB = 256

// DefaultPlatformReserveMB is the instance memory the capacity rule sets
// aside for everything that isn't a run on an all-in-one box: the TF
// binary, Postgres, GoTrue, the object store, and safety headroom the
// dispatcher's memory floor defends at runtime. It models a co-resident
// platform stack — right for TF_ROLE=all, too large for a dedicated
// executor pod that hosts none of those services.
const DefaultPlatformReserveMB = 12288

// DefaultExecutorPlatformReserveMB is the reserve for a dedicated
// TF_ROLE=executor pod (TFAC-582). An executor hosts none of the
// co-resident platform stack the all-in-one reserve models — just the
// orchestrator + cap-broker + OS headroom — so with the 12 GB all-in-one
// reserve a normal ~8 GB executor pod would derive advisory capacity 0.
// Env-tunable via TF_PLATFORM_RESERVE_MB (ParsePlatformReserveMB).
const DefaultExecutorPlatformReserveMB = 2048

// DerivedRunCapacity applies the sizing rule against the all-in-one
// platform reserve. Kept as the DefaultPlatformReserveMB-pinned form for
// existing callers/tests; DerivedRunCapacityWithReserve is the role-aware
// variant.
func DerivedRunCapacity(totalMB int) int {
	return DerivedRunCapacityWithReserve(totalMB, DefaultPlatformReserveMB)
}

// DerivedRunCapacityWithReserve applies the sizing rule to this instance's
// total memory (hostmem.TotalMB — the cgroup limit when confined, host
// MemTotal otherwise) against a caller-supplied reserve:
// runs ≈ (RAM − reserve) ÷ budget, clamped to [0, ceiling]. This is
// advisory (boot log + over-provision warning) — the runtime protections
// are the concurrency cap and the dispatch memory floor.
func DerivedRunCapacityWithReserve(totalMB, reserveMB int) int {
	if totalMB <= reserveMB {
		return 0
	}
	n := (totalMB - reserveMB) / DefaultClaimMemoryBudgetMB
	if n > MaxConcurrentClaimsCeiling {
		return MaxConcurrentClaimsCeiling
	}
	return n
}

// ParsePlatformReserveMB interprets the TF_PLATFORM_RESERVE_MB env value,
// falling back to roleDefault (DefaultPlatformReserveMB for all, or
// DefaultExecutorPlatformReserveMB for an executor pod) when empty. A
// negative or non-numeric value returns roleDefault plus an error the
// caller logs — a bad value must not brick boot, and the reserve only
// tunes an advisory number anyway.
func ParsePlatformReserveMB(raw string, roleDefault int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return roleDefault, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return roleDefault, fmt.Errorf(
			"invalid TF_PLATFORM_RESERVE_MB %q (want an integer >= 0); using default %d",
			raw, roleDefault)
	}
	return n, nil
}

// DefaultDispatchMemFloorMB is the default MemAvailable floor below
// which the dispatcher defers claiming queued runs. Sized to the
// platform reserve the capacity sizing rule assumes (see
// docs/benchmarks/sandbox-bench.md): enough headroom that in-flight runs finish
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
			dispatchLog.Warn("dispatch paused: available memory below floor; queued conversations deferred until it recovers",
				"available_mb", avail, "floor_mb", floor)
		} else {
			dispatchLog.Info("dispatch resumed: available memory recovered above floor",
				"available_mb", avail, "floor_mb", floor)
		}
	}
	return gated
}

// noteCapAcquireBlocked marks the start of a cap-saturation episode: every
// concurrency slot is occupied and the dispatcher is about to block waiting
// for one, with work actually queued behind it. Logged once per episode (not
// per blocked acquire) so a busy host doesn't emit a line every scan tick,
// and skipped entirely when nothing is queued — four long engagements on an idle
// queue is full utilization, not a backlog worth announcing. This is the
// queue-side twin of dispatchMemGated's transition log: without it, a burst
// of delegations sits visibly "queued" in the UI while the log shows nothing,
// which reads as a hang rather than admission control.
func (s *Spawner) noteCapAcquireBlocked(ctx context.Context, capN int) {
	if s.capSaturated.Load() || s.conversationQueue == nil {
		return
	}
	queued, err := s.conversationQueue.CountQueuedSystem(ctx)
	if err != nil || queued == 0 {
		return
	}
	if s.capSaturated.CompareAndSwap(false, true) {
		dispatchLog.Info("claim concurrency cap reached; queued conversations start as slots free",
			"cap", capN, "queued", queued, "env", "TF_MAX_CONCURRENT_CLAIMS")
	}
}

// noteCapAcquireImmediate records that an acquire succeeded without waiting —
// the saturation episode, if one was running, is over. Transition-logged like
// its blocked twin above.
func (s *Spawner) noteCapAcquireImmediate(capN int) {
	if s.capSaturated.CompareAndSwap(true, false) {
		dispatchLog.Info("claim concurrency below cap; dispatching queued conversations immediately", "cap", capN)
	}
}

// DefaultIdleHibernateTimeout is how long a live run may go quiet — no
// stream activity, no turn end — before the driver gives up on it and parks
// the conversation to a durable resume. A backstop against a process that
// has stopped producing, not a between-turns keep-alive: the process is
// closed the moment a turn ends with nothing queued behind it, so this only
// ever fires on a turn that went silent. Tunable via SetIdleHibernateTimeout
// (tests inject a short value).
const DefaultIdleHibernateTimeout = 5 * time.Minute

// liveRunHandle wraps a run's live agent process plus the identity a
// control op needs to reach it. Held in s.procs for the lifetime of the
// run's live execution; the driver registers it when the process spawns
// and deregisters it when the process closes (terminal, park, or cancel).
type liveRunHandle struct {
	lr             *agentproc.LiveRun
	conversationID string
	orgID          string
}

// registerProc records a run's live process handle so control ops can
// reach it across HTTP turns. Mirrors the cancels-map registration the
// dispatcher already does, under the same s.mu.
//
// The SOLE writer of s.procs, and SendMessage's fast path depends on that: a
// registered handle is proof the conversation is an SDK one, so a message can
// be steered into the process without re-reading the runtime. A second driver
// registering here silently breaks that.
func (s *Spawner) registerProc(orgID, conversationID string, lr *agentproc.LiveRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.procs[conversationID] = &liveRunHandle{lr: lr, conversationID: conversationID, orgID: orgID}
}

// getProc returns the live handle for a run, or nil when the run has no
// live process (never started, parked, or terminated).
func (s *Spawner) getProc(conversationID string) *liveRunHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[conversationID]
}

// deregisterProc drops a run's live handle once the process is gone.
// Idempotent.
func (s *Spawner) deregisterProc(conversationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.procs, conversationID)
}

// stampExecutor records this spawner instance's executor identity + boot
// epoch on the engagement's claim when the run goes live (a system write —
// the run goroutine holds no JWT claims). This re-stamps what
// ConversationQueueStore.ClaimNextConversation already wrote atomically at claim on
// a fresh claim — cheap and harmless — but it is the ONLY stamp on the
// resume path, so it must write both columns, not
// just executor_id, to keep boot_epoch reflecting the most recent boot that
// touched the row rather than whatever epoch it was originally claimed
// under. Best-effort: a failure is logged, not fatal, because executor
// ownership is a forward-compat hook, not a correctness gate at N=1.
//
// Fenced on claimID, because "the process is live" is a claim about ownership
// and setup is where ownership is most likely to have moved on: a run reaped
// mid-clone whose process then comes up would otherwise stamp a dead executor
// onto the successor's claim, and the reaper reads that column. Empty claimID
// keeps the unfenced active-claim write for callers with no engagement in
// scope. A refusal is logged loudly and nothing else changes — the engagement
// carries on to its first transcript write, which meets the same fence and
// abandons the conversation properly.
func (s *Spawner) stampExecutor(orgID, conversationID, claimID string) {
	if s.conversations == nil {
		return
	}
	executorID, bootEpoch := s.executorIdentity()
	var err error
	if claimID != "" {
		_, err = s.conversations.SetExecutorForClaimSystem(context.Background(), orgID, conversationID, claimID, executorID, bootEpoch)
	} else {
		_, err = s.conversations.SetExecutorSystem(context.Background(), orgID, conversationID, executorID, bootEpoch)
	}
	if errors.Is(err, db.ErrClaimReleased) {
		delegateLog.Error("claim fence refused the go-live executor stamp — a successor owns this conversation",
			"executor", executorID, "conversation", conversationID, "claim_id", claimID, "org_id", orgID, "error", err)
	} else if err != nil {
		delegateLog.Warn("stamp executor on conversation failed", "executor", executorID, "conversation", conversationID, "error", err)
	}
}

// recordSandboxActuals is the RunOptions.RecordSandboxActuals recorder every
// delegated launch wires: it stamps what the launch's jail actually consumed
// (peak memory, CPU time — kernel truth from its cgroup) onto the claim that
// paid for it. agentproc calls it at teardown on a detached context and
// swallows the error, so this is a plain write with no retry: a lost stamp
// costs one run's accounting, and the alternative — failing a finished run
// over it — is strictly worse.
//
// Keyed on the claim id the dispatcher threaded through, never on "the
// conversation's active claim": by teardown the completion bookkeeping has
// already released it.
func (s *Spawner) recordSandboxActuals(ctx context.Context, orgID, claimID string, actuals sandbox.RunActuals) error {
	if s.conversations == nil {
		return nil
	}
	_, err := s.conversations.RecordClaimSandboxStatsSystem(ctx, orgID, claimID, actuals.PeakMemMB, actuals.CPUUsec)
	return err
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

// SetMaxConcurrentClaims resizes the off-dispatcher concurrency cap. Call
// once at startup before RunDispatcher runs; the in-flight goroutines
// close over the channel they acquired from, so a startup-time resize is
// safe. A value below 1 is clamped to 1.
func (s *Spawner) SetMaxConcurrentClaims(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runSem = make(chan struct{}, n)
}

// semaphore returns the current concurrency-cap channel. Callers capture
// it once and use the captured value for both acquire and release so a
// startup-time SetMaxConcurrentClaims can't strand a token on a replaced
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

// SetAwaitingCredentialsTimeout overrides the awaiting-credentials wait's
// deadline and poll cadence (TFAC-614). Tests inject short values to drive
// the timeout/requeue path deterministically without waiting out the real
// 2-minute default; pollInterval <= 0 leaves the poll cadence at its
// default.
func (s *Spawner) SetAwaitingCredentialsTimeout(timeout, pollInterval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.awaitingCredentialsTimeoutOverride = timeout
	s.awaitingCredentialsPollIntervalOverride = pollInterval
}

// awaitingCredentialsKnobs returns the effective (timeout, pollInterval)
// pair, falling back to the package defaults when unset.
func (s *Spawner) awaitingCredentialsKnobs() (time.Duration, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	timeout := awaitingCredentialsTimeout
	if s.awaitingCredentialsTimeoutOverride > 0 {
		timeout = s.awaitingCredentialsTimeoutOverride
	}
	poll := awaitingCredentialsPollInterval
	if s.awaitingCredentialsPollIntervalOverride > 0 {
		poll = s.awaitingCredentialsPollIntervalOverride
	}
	return timeout, poll
}

// RunController routes a live-process control op to wherever the run's
// process actually lives. Interrupt and Steer drive the live process;
// Cancel signals the hard-kill ctx. At N=1 the in-process impl resolves
// the target from s.procs / s.cancels; horizontal scaling replaces it with
// a DB-signal to the executor that owns the run's lease, leaving the
// callers (the stop verb, and P3's steer endpoint) unchanged.
type RunController interface {
	// Interrupt stops the run's current turn, leaving the process alive
	// for further input. Errors when the run has no live process.
	Interrupt(ctx context.Context, conversationID string) error
	// Steer delivers a free-form user message to a live run. Errors when
	// the run has no live process.
	Steer(ctx context.Context, conversationID, text string) error
	// Cancel signals the run's process to terminate (SIGKILL via the
	// registered ctx cancel). Reports whether a live handle was found; a
	// false result means the run has no in-process goroutine and the
	// caller must take the DB-only terminal path.
	Cancel(conversationID string) (found bool)
}

// ErrNoLiveProcess is the typed error the control seam returns when a run has
// no live process to reach — it terminated, parked, or never started. The
// interrupt/message endpoints map it to 409 Conflict, which tells the client
// to re-read the conversation: a run that just parked takes the same message
// through the queue.
var ErrNoLiveProcess = errors.New("run has no live process")

// inProcessController is the N=1 RunController: every run's process lives
// in this same process, so the lookups are direct map reads.
type inProcessController struct{ s *Spawner }

func (c inProcessController) Interrupt(ctx context.Context, conversationID string) error {
	h := c.s.getProc(conversationID)
	if h == nil {
		return fmt.Errorf("interrupt run %s: %w", conversationID, ErrNoLiveProcess)
	}
	return h.lr.Interrupt(ctx)
}

func (c inProcessController) Steer(ctx context.Context, conversationID, text string) error {
	h := c.s.getProc(conversationID)
	if h == nil {
		return fmt.Errorf("steer run %s: %w", conversationID, ErrNoLiveProcess)
	}
	return steerSendError(conversationID, h.lr.Send(ctx, text))
}

// steerSendError is what a steer reports of the process's answer. A process
// the driver is closing is one the registry still lists (the handle comes out
// after Close returns) but that can take no more input, and to the caller
// that is the same answer as no process at all — ErrNoLiveProcess, the
// re-read-and-retry signal — rather than a write fault, which would report a
// delivery that was never going to happen as an error in the sending.
func steerSendError(conversationID string, err error) error {
	if errors.Is(err, agentproc.ErrRunClosing) {
		return fmt.Errorf("steer run %s: %w", conversationID, ErrNoLiveProcess)
	}
	return err
}

func (c inProcessController) Cancel(conversationID string) bool {
	c.s.mu.Lock()
	cancel, ok := c.s.cancels[conversationID]
	c.s.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// Interrupt stops a live run's current turn through the control seam,
// leaving the process alive for further input — the in-turn pause, distinct
// from the stop verb, which ends the engagement and parks the conversation.
// Routing through s.controller (read via getController) is what keeps the
// horizontal-scaling swap additive.
func (s *Spawner) Interrupt(ctx context.Context, conversationID string) error {
	return s.getController().Interrupt(ctx, conversationID)
}

// Steer delivers a free-form user message to a live run through the
// control seam. The P3 message endpoint calls this.
func (s *Spawner) Steer(ctx context.Context, conversationID, text string) error {
	return s.getController().Steer(ctx, conversationID, text)
}
