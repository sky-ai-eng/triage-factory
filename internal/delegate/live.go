// Live-run execution: every run executes as a long-lived agentproc.LiveRun
// (a streaming-input process you can message/interrupt) rather than a
// one-shot blocking call. The driver here turns one live invocation into a
// terminal disposition — a turn-terminal result, an idle hibernation, or a
// process exit — and feeds the shared post-stream branching (processCompletion)
// exactly as the one-shot path did.
//
// Two execution backends share one disposition shape (liveOutcome): the
// LiveRun driver (both local direct runs and multi-mode gVisor-sandboxed runs
// drive through it — the sandbox's bidirectional stdio channel is validated
// end-to-end) and the one-shot fallback, which runAgent and ResumeWithMessage
// select via agentproc.InteractiveSupported(). That's unconditionally true
// today, so the one-shot path is a vestigial seam kept for a future host that
// can't support streaming input, not a live sandbox behavior.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
)

// liveProc is the slice of *agentproc.LiveRun the driver loop needs. Pulled
// out as an interface so driveLiveRun is unit-testable with a fake process
// (no subprocess) — the real *agentproc.LiveRun satisfies it. Send delivers a
// follow-up message into the same warm process (used by the invalid-envelope
// re-prompt-to-fix).
type liveProc interface {
	Done() <-chan struct{}
	Result() *agentproc.Result
	SessionID() string
	Stderr() string
	Err() error
	Send(ctx context.Context, text string) error
	Close() error
}

// liveParkContext carries the identity an idle hibernation needs to snapshot
// the workspace and park the run to open.
type liveParkContext struct {
	orgID         string
	runID         string
	taskID        string
	namespace     string // blueprint_run_id — the snapshot/worktree key
	claudeCwd     string
	triggerType   string
	creatorUserID string
	// claimID names the engagement writing the park, routing it through the
	// claim fence. Empty keeps the unfenced write — the live driver's idle
	// park has no claim in scope.
	claimID string
	// reason is why: db.ParkIdle() for a turn that simply ended,
	// db.ParkStopped(...) for a cancel. See ConversationStore.ParkOpen.
	reason db.Park
}

// liveOutcome is the disposition of one agent invocation, produced
// identically by the LiveRun driver and the one-shot fallback so runAgent /
// ResumeWithMessage branch on a single shape:
//
//   - result set        → a turn produced a valid conclusion (or an IsError /
//     crash result); the caller runs processCompletion (finalize / advance /
//     fail-with-reason).
//   - hibernated true    → the live process went idle and was parked to
//     open (snapshot written, status flipped); the caller returns dormant,
//     keeping the warm worktree.
//   - err set, no result → the process errored / was cancelled before any
//     terminal result, or the agent never corrected an invalid conclusion
//     envelope within the bound; the caller routes through parkRunOpen /
//     failRun.
type liveOutcome struct {
	result     *agentproc.Result
	sessionID  string
	stderr     string
	hibernated bool
	err        error
}

// liveRunSpec bundles everything runLiveAndDrive needs to spawn, register,
// drive, and (on idle) hibernate one live agent invocation.
type liveRunSpec struct {
	park        liveParkContext
	opts        agentproc.RunOptions
	perms       agentproc.PermissionHandler
	sink        agentproc.Sink
	idleTimeout time.Duration // <=0 disables idle hibernation (e.g. the gate's bounded resume)
}

// resultsBufferDepth sizes the OnResult channel runLiveAndDrive hands the
// driver. Each turn produces one result and the driver consumes one per loop
// iteration; the bounded re-prompt loop is sequential (read a turn, then send
// its correction), so only a couple of results are ever unread at once. The
// depth is defined as maxCompletionRetries plus headroom purely as a defensive
// lower bound — it ties the two constants together so bumping the retry bound
// can never silently drop the buffer below the in-flight turn count and start
// shedding live turns. (The non-blocking send only ever sheds a turn the driver
// has already moved past; see OnResult.)
const resultsBufferDepth = maxCompletionRetries + 5

// runLiveAndDrive starts an interactive agent process for the run, registers
// it in the process registry so control ops can reach it, stamps executor
// ownership, and drives it to a terminal result or an idle hibernation. The
// process is closed and the handle deregistered by the time this returns.
// Shared by the initial run path and the resume path so every run executes
// uniformly as a LiveRun.
func (s *Spawner) runLiveAndDrive(ctx context.Context, spec liveRunSpec) liveOutcome {
	// Buffered so the reader goroutine's OnResult / activity callbacks never
	// block on a driver that's momentarily not selecting (both use a
	// non-blocking send, but a buffer keeps the common case lock-free).
	results := make(chan *agentproc.Result, resultsBufferDepth)
	activity := make(chan struct{}, 64)

	spec.opts.OnResult = func(r *agentproc.Result) {
		// The driver consumes a result per turn: a valid conclusion closes the
		// process and returns, an invalid one is re-prompted (the next turn
		// produces the next result), and a no-conclusion turn keeps the process
		// warm. A full buffer means results are arriving faster than the driver
		// selects them; a non-blocking send keeps the reader goroutine moving.
		// The driver only ever acts on the result it reads next, so an overflow
		// drop loses a stale turn, never the one it will decide on — and
		// resultsBufferDepth stays above the in-flight turn count by
		// construction (see its definition).
		select {
		case results <- r:
		default:
		}
	}
	sink := newActivitySink(spec.sink, activity)

	lr, err := agentproc.RunInteractive(ctx, spec.opts, sink, spec.perms)
	if err != nil {
		return liveOutcome{err: err}
	}
	s.registerProc(spec.park.orgID, spec.park.runID, lr)
	defer s.deregisterProc(spec.park.runID)
	// Stamp run→executor ownership now the process is live (N=1 instance id;
	// the lease layer horizontal scaling adds builds on this column).
	s.stampExecutor(spec.park.orgID, spec.park.runID)

	out := s.driveLiveRun(ctx, spec.park, lr, results, activity, spec.idleTimeout)
	// Capture the final session id / stderr off the (now-closed) process for
	// the caller's completion + failure paths.
	out.sessionID = lr.SessionID()
	out.stderr = lr.Stderr()
	// The driver hands back the per-turn result it decided on; the live process
	// folds every turn (pause turns, re-prompt corrections, the conclusion)
	// into its merged Result. Take ONLY the accounting fields from that fold —
	// disposition (IsError, Subtype, StopReason, the envelope text,
	// Interrupted) stays with the turn the driver classified. Replacing the
	// result wholesale would let MergeResult's sticky IsError from a benign
	// interrupted (pause) turn flip a valid conclusion into status=failed
	// downstream — processCompletion checks IsError before the envelope.
	if out.result != nil {
		if merged := lr.Result(); merged != nil {
			out.result = foldAccounting(out.result, merged)
		}
	}
	return out
}

// foldAccounting returns the classified turn's result carrying the
// conversation-cumulative accounting (cost, duration, turns) from the merged
// fold. Everything else — the disposition processCompletion acts on — is the
// classified turn's own.
func foldAccounting(classified, merged *agentproc.Result) *agentproc.Result {
	r := *classified
	r.CostUSD = merged.CostUSD
	r.DurationMs = merged.DurationMs
	r.NumTurns = merged.NumTurns
	return &r
}

// driveLiveRun is the select loop that resolves a live process into a
// disposition by classifying each turn-end into one of three buckets:
//
//   - valid conclusion → close the process and hand the result back for
//     orchestration (finalize / advance / fail-with-reason).
//   - invalid conclusion attempt (envelope-shaped but malformed / missing a
//     required field) → re-prompt the same warm process to fix it, up to
//     maxCompletionRetries; fail the run if it never corrects.
//   - no conclusion (prose / nothing) → the run is open: keep the process
//     warm, reset the idle timer, and loop. Whether the OS process stays warm
//     is in-memory only; the status only flips to open on idle hibernation.
//
// The idle timer resets on every stream activity, so a slow-but-working agent
// (constant tool/text output) never hibernates — only a genuinely quiet
// process does. idleTimeout<=0 disables hibernation AND the multi-turn loop:
// it's the bounded-resume backstop (ResumeWithMessage), which expects exactly
// one turn, so any result there is terminal and an open turn-end is handed
// back rather than looped (no idle timer would ever close it).
//
// The idle window is armed at entry (process spawn). The first stream event —
// typically system/init, sub-second — resets it, so idleTimeout is effectively
// the grace a *no-output* process gets before hibernating; keep it well above
// agent-startup latency (the 5-min default is; a tiny injected value will
// hibernate before the first turn, which is exactly what the idle test leans
// on).
//
// Pulled out from runLiveAndDrive so it can be driven with a fake proc +
// hand-fed channels in tests, without spawning a subprocess.
func (s *Spawner) driveLiveRun(ctx context.Context, park liveParkContext, proc liveProc, results <-chan *agentproc.Result, activity <-chan struct{}, idleTimeout time.Duration) liveOutcome {
	idle, idleC := newIdleTimer(idleTimeout)
	if idle != nil {
		defer idle.Stop()
	}
	// keepWarmOnNone distinguishes a long-lived autonomous run (a no-conclusion
	// turn leaves the process warm and loops; idle later closes it) from a
	// bounded backstop resume (no idle timer, so a no-conclusion turn is handed
	// straight back rather than looped forever). Both hold a live process, so an
	// invalid envelope is re-prompted in place either way — only the
	// no-conclusion handling differs. Keyed off idleTimeout so the two callers
	// stay on one driver.
	keepWarmOnNone := idleTimeout > 0
	invalidAttempts := 0

	for {
		select {
		case <-ctx.Done():
			// Hard cancel: the registered ctx cancel SIGKILLed the process.
			// Close (idempotent) and surface the ctx error so the caller routes
			// through its cancelled path.
			_ = proc.Close()
			return liveOutcome{err: ctx.Err()}

		case r := <-results:
			// A turn we interrupted ourselves is a pause, not a failure. The SDK
			// wire-labels it is_error/error_during_execution — shape-identical
			// to a real runtime error — but the agentproc reader marks it
			// first-class (Result.Interrupted, from the wrapper's
			// control/interrupted ack), the same signal Claude Code's own UI
			// uses to render Esc gracefully. The session survives an interrupt,
			// so park the run open with the process warm for the composer's
			// next message.
			if r.Interrupted {
				if !keepWarmOnNone {
					// Bounded resume: no idle timer will ever close the warm
					// process — close it and park to a durable resume.
					_ = proc.Close()
					_ = s.parkRunOpen(park, proc.SessionID())
					return liveOutcome{hibernated: true}
				}
				_ = s.markRunOpen(park)
				resetIdleTimer(idle, idleTimeout)
				continue
			}
			// An IsError result (max-turns, runtime error) is terminal
			// regardless of envelope shape — hand it back; processCompletion
			// fails it.
			if r.IsError {
				_ = proc.Close()
				return liveOutcome{result: r}
			}
			class, _ := classifyAgentResult(r.Result)
			switch class {
			case turnValid:
				// A valid conclusion. Close the process (freeing the session) and
				// hand it to processCompletion to orchestrate.
				_ = proc.Close()
				return liveOutcome{result: r}

			case turnInvalid:
				if invalidAttempts >= maxCompletionRetries {
					// Exhausted the re-prompt bound — hand the unfixed result back.
					// processCompletion records the failure (a knowable error) with
					// the totals the live process folded across the correction turns,
					// rather than dropping them on a bare error return.
					_ = proc.Close()
					return liveOutcome{result: r}
				}
				// An envelope attempt that didn't validate — correct it in place on
				// the warm process and wait for the next turn. Identical for an
				// autonomous run and a bounded resume: both hold a live process.
				invalidAttempts++
				if err := proc.Send(ctx, invalidEnvelopeCorrection()); err != nil {
					_ = proc.Close()
					return liveOutcome{err: fmt.Errorf("re-prompt invalid completion envelope: %w", err)}
				}
				resetIdleTimer(idle, idleTimeout) // sending is activity

			case turnNone:
				if !keepWarmOnNone {
					// Bounded resume: no idle timer would ever close a warm process,
					// so don't loop — hand the no-conclusion result back and let
					// processCompletion mark the run open.
					_ = proc.Close()
					return liveOutcome{result: r}
				}
				// The turn ended without a conclusion → the run is open (not
				// executing, not concluded). Flip the status now: the process stays
				// warm in s.procs for a follow-up message, but whether it's warm is
				// never a status — so a crash here leaves an `open` run that the boot
				// reconcile correctly leaves alone (nothing to resume). No retry, no
				// fail, no claim about why. Re-arm idle and loop; idle later closes
				// the warm process (status stays open).
				_ = s.markRunOpen(park)
				resetIdleTimer(idle, idleTimeout)
			}

		case <-activity:
			resetIdleTimer(idle, idleTimeout)

		case <-idleC:
			// Quiet past the threshold with no input to act on — park the run open
			// to a durable resume. The status flips to open here; whether a process
			// was warm was never a status.
			_ = proc.Close()
			_ = s.parkRunOpen(park, proc.SessionID())
			return liveOutcome{hibernated: true}

		case <-proc.Done():
			// The process exited on its own (crash, or a Close from elsewhere).
			// Hand back whatever terminal result was folded, else the error.
			return liveOutcome{result: proc.Result(), err: proc.Err()}
		}
	}
}

// invalidEnvelopeCorrection is the message the driver re-prompts a warm run
// with after a malformed / incomplete completion envelope. It names exactly
// the contract the agent owes; the agent's position-specific system prompt
// already told it which outcomes apply, so this just demands a well-formed
// envelope. Kept terse because it rides the same session, not a fresh prompt.
func invalidEnvelopeCorrection() string {
	return "Your final message was not a valid completion envelope. Reply with ONLY a JSON object " +
		"whose \"outcome\" is one of \"continue\", \"finish\", or \"abort\", carrying a \"summary\" " +
		"(on finish/continue) or a \"reason\" (on abort), and no other text."
}

// markRunOpen flips a run's status to `open` under a race guard, then nudges
// the board + UI. The shared flip for every park: the warm path (the live
// driver's no-conclusion turn, where the process stays warm in s.procs and
// there's nothing to snapshot yet), parkRunOpen (process gone — snapshots
// first), and a cancel (park.reason names the stop). Flipping on the
// no-conclusion turn, rather than only at idle, is what makes a crash in the
// warm window recover correctly: the boot reconcile leaves `open` runs alone,
// since a restart provides no input to resume them. Nil-safe so the no-DB
// driver tests can exercise the loop.
//
// Routing has three arms, and which one a park takes says who is speaking. An
// engagement parking its own run (claimID set) goes through the claim fence,
// so a zombie executor cannot park a conversation its successor holds. A
// manual run with no claim in scope writes under the creator's synthetic
// claims. Everything else is a system write on the admin pool.
//
// Returns fenced: true when the fence refused the write because this
// engagement's claim was released. Nothing was recorded or broadcast, and the
// caller must not act on the run's state either — it belongs to whoever holds
// the claim now. Always false on the two unfenced arms.
func (s *Spawner) markRunOpen(park liveParkContext) (fenced bool) {
	if s.agentRuns == nil {
		return false // test fixture with no DB wired
	}
	bgCtx := context.Background()
	var flipped bool
	var err error
	switch {
	case park.claimID != "":
		flipped, err = s.agentRuns.ParkOpenForClaimSystem(bgCtx, park.orgID, park.runID, park.claimID, park.reason)
	case park.triggerType == "manual":
		err = s.tx.SyntheticClaimsWithTx(bgCtx, park.orgID, park.creatorUserID, func(ts db.TxStores) error {
			f, e := ts.Conversations.ParkOpen(bgCtx, park.orgID, park.runID, park.reason)
			flipped = f
			return e
		})
	default:
		flipped, err = s.agentRuns.ParkOpenSystem(bgCtx, park.orgID, park.runID, park.reason)
	}
	if errors.Is(err, db.ErrClaimReleased) {
		// Whoever holds the claim now owns the conversation, so this park is
		// not this engagement's state to report. The workspace stays too — it
		// may be the one they are running in.
		//
		// A deliberate stop says so at INFO, because there the refusal is the
		// design working. Control parks the row and releases the claim the
		// instant the user asks, and this engagement's teardown then arrives
		// to find the state already recorded by the actor who asked for it —
		// every cross-pod stop produces exactly one of these. Alarming on it
		// would train the reader to ignore the line that matters.
		//
		// An idle park has no such outside actor, so a refusal there means a
		// successor really did take the conversation out from under a live
		// engagement. That keeps ERROR.
		if park.reason.Deliberate() {
			delegateLog.Info("claim fence refused the park after a deliberate stop — the stopping actor already parked this conversation; recording nothing further",
				"run", park.runID, "claim_id", park.claimID, "org_id", park.orgID)
			return true
		}
		delegateLog.Error("claim fence refused the park — a successor owns this conversation; recording nothing",
			"run", park.runID, "claim_id", park.claimID, "org_id", park.orgID, "error", err)
		return true
	}
	if err != nil {
		delegateLog.Warn("mark run open failed", "run", park.runID, "error", err)
		return false
	}
	if !flipped {
		// A racing terminal flip won, or this is an idle re-park of a row
		// already `open` — leave its status and say nothing.
		return false
	}
	s.broadcastRunUpdate(park.orgID, park.runID, "open")
	s.recomputeTaskBoardColumn(park.orgID, park.taskID)
	return false
}

// parkRunOpen records a run as `open` when its process is gone — the live
// driver idle-closed it, a one-shot/resume turn ended without a conclusion, or
// someone cancelled it. It snapshots the workspace (the cold-resume backstop)
// BEFORE the flip so a resume that lands without the warm worktree can rebuild
// it, then flips the status via markRunOpen. markRunOpen is the warm-process
// sibling (no snapshot — the process is still alive to take the next message).
// "open" makes no claim about why the run stopped or who continues it; any
// later input resumes it on the same ResumeWithMessage path.
//
// The cancel path is this same function, and that is the point: the old
// cancel handler wrote its own terminal and removed the worktree on its way
// out, which threw away the one thing a user who just killed a wedged run is
// likely to want back. A stop is a park with a reason attached.
func (s *Spawner) parkRunOpen(park liveParkContext, sessionID string) (fenced bool) {
	// Snapshot BEFORE the flip: once dormant the run can resume on a host
	// without the warm worktree, so the blob must exist by the time the
	// status commits. Best-effort — the kept warm worktree is the fast path.
	// Skipped when there is no workspace yet (a cancel during setup), which
	// snapshotWorkspace would reject anyway.
	//
	// The ordering carries a second load in the control/executor split, and
	// it is the whole reason a cross-pod stop stays resumable. There the row
	// is already parked and the claim already released — by control, on the
	// user's behalf (see Spawner.stop) — so this engagement's flip below is
	// refused by the fence. The unfenced snapshot is then the only thing this
	// teardown still has to contribute, and it is the thing the follow-up
	// needs. Flipping first and snapshotting after would leave a
	// deliberately-stopped run with no workspace at all.
	snapshotted := false
	if park.claudeCwd != "" && park.namespace != "" {
		if err := s.snapshotWorkspace(context.Background(), park.orgID, park.namespace, park.claudeCwd, sessionID); err != nil {
			delegateLog.Warn("snapshot workspace before parking open failed", "run", park.runID, "error", err)
		} else {
			snapshotted = true
		}
	}
	if fenced := s.markRunOpen(park); fenced {
		// The fenced teardown's one contribution just landed, and it is the one
		// a follow-up needs. The actor who stopped this run parked the row and
		// announced `open` before the blob existed, so every watcher read it as
		// unresumable and stopped asking — this says the workspace is here now.
		//
		// Two conditions, and both are about not announcing something untrue.
		// A snapshot that actually succeeded, because there is otherwise
		// nothing to announce. And a DELIBERATE park, because that is the
		// refusal whose `open` is real: control recorded it on the user's
		// behalf. The other refusal — a successor taking the conversation out
		// from under a live engagement — leaves it running under someone else,
		// and repeating a parked status there would be this teardown reporting
		// a state that isn't the row's.
		if snapshotted && park.reason.Deliberate() {
			s.broadcastRunResumable(park.orgID, park.runID)
		}
		return true
	}
	// Only the idle park toasts. A deliberate stop terminates the blueprint
	// behind it, so "resumes on the next message" would be a promise this
	// build cannot keep — the claim gate refuses a parked step under a
	// finished blueprint until the resume work lands.
	if !park.reason.Deliberate() {
		toast.Info(s.wsHub, park.orgID, fmt.Sprintf("Run %s is open — resumes on the next message", shortRunID(park.runID)))
	}
	return false
}

// runOneShot wraps the blocking one-shot agentproc.Run into the shared
// liveOutcome shape. The fallback backend for hosts where interactive runs
// aren't supported yet (multi-mode gVisor sandbox); behavior is byte-for-byte
// the historical one-shot path.
func (s *Spawner) runOneShot(ctx context.Context, opts agentproc.RunOptions, sink agentproc.Sink) liveOutcome {
	outcome, err := agentproc.Run(ctx, opts, sink)
	out := liveOutcome{err: err}
	if outcome != nil {
		out.result = outcome.Result
		out.sessionID = outcome.SessionID
		out.stderr = outcome.Stderr
	}
	return out
}

// newIdleTimer returns a started idle timer and its fire channel, or
// (nil, nil) when hibernation is disabled (d<=0) — a nil channel never
// selects, so the driver's idle case is simply unreachable then.
func newIdleTimer(d time.Duration) (*time.Timer, <-chan time.Time) {
	if d <= 0 {
		return nil, nil
	}
	t := time.NewTimer(d)
	return t, t.C
}

// resetIdleTimer re-arms an idle timer on stream activity, draining a
// concurrently-fired tick so the next idle window is the full timeout. A nil
// timer (hibernation disabled) is a no-op.
func resetIdleTimer(t *time.Timer, d time.Duration) {
	if t == nil {
		return
	}
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// activitySink decorates a Sink so the driver's idle timer resets on every
// stream message — the signal that the agent is actively working. The
// non-blocking bump means a wedged driver never back-pressures the reader
// goroutine that owns the sink.
type activitySink struct {
	inner    agentproc.Sink
	activity chan<- struct{}
}

func newActivitySink(inner agentproc.Sink, activity chan<- struct{}) activitySink {
	return activitySink{inner: inner, activity: activity}
}

func (a activitySink) OnSession(id string) error {
	a.bump()
	return a.inner.OnSession(id)
}

func (a activitySink) OnMessage(m *domain.Message) error {
	a.bump()
	return a.inner.OnMessage(m)
}

func (a activitySink) bump() {
	select {
	case a.activity <- struct{}{}:
	default:
	}
}

// Compile-time check that activitySink satisfies the Sink contract.
var _ agentproc.Sink = activitySink{}
