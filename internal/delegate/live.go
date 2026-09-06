// Live-run execution: every run executes as a long-lived agentproc.LiveRun
// (a streaming-input process you can message/interrupt) rather than a
// one-shot blocking call. The driver here turns one live invocation into a
// terminal disposition — a turn-terminal result, a park, or a process exit —
// and feeds the shared post-stream branching (processCompletion) exactly as
// the one-shot path did.
//
// The process lives exactly as long as its engagement's claim. A turn that
// ends without concluding closes the process and parks the conversation,
// and a follow-up wakes a fresh claim that resumes the same session by id;
// the one thing that keeps a process alive past a turn boundary is a turn it
// already owes — a message steered in while the last one ran, which the SDK
// queues and starts next. Nothing is kept warm between turns, because a
// parked conversation has no claim, and every transcript write an engagement
// makes is fenced on its claim: a process left alive past its park would be
// killed by the fence on its next word.
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
// out as an interface so driveLiveConversation is unit-testable with a fake process
// (no subprocess) — the real *agentproc.LiveRun satisfies it. Send delivers a
// follow-up message into the same live process (used by the invalid-envelope
// re-prompt-to-fix); QueuedTurns is how many messages sent into it are still
// unanswered, which at a turn boundary is whether it owes another turn.
type liveProc interface {
	Done() <-chan struct{}
	Result() *agentproc.Result
	SessionID() string
	Stderr() string
	Err() error
	Send(ctx context.Context, text string) error
	QueuedTurns() int
	Close() error
}

// liveParkContext carries the identity a park needs to snapshot the
// workspace and flip the run to open.
type liveParkContext struct {
	orgID          string
	conversationID string
	taskID         string
	namespace      string // blueprint_run_id — the snapshot/worktree key
	claudeCwd      string
	triggerType    string
	creatorUserID  string
	// claimID names the engagement writing the park, routing it through the
	// claim fence. Every park a dispatched run writes carries one — the idle
	// turn-end as much as the cancel, since a zombie's idle park would flip a
	// conversation its successor is mid-turn on. Empty is the claimless
	// caller's unfenced write, and a test fixture's.
	claimID string
	// reason is why: db.ParkIdle() for a turn that simply ended,
	// db.ParkStopped(...) for a cancel. See ConversationStore.ParkOpen.
	reason db.Park
	// runtime is the conversation's engine, for the workspace-snapshot span
	// family (see snapshotWorkspace). Each construction site states it from
	// what it structurally is — the SDK and native drivers each build their
	// own park — rather than re-reading the row.
	runtime string
}

// liveOutcome is the disposition of one agent invocation, produced
// identically by the LiveRun driver and the one-shot fallback so runAgent /
// ResumeWithMessage branch on a single shape:
//
//   - result set        → a turn produced a valid conclusion (or an IsError /
//     crash result, or ended with no conclusion at all); the caller runs
//     processCompletion (finalize / advance / fail-with-reason / park open).
//     The process is already closed.
//   - hibernated true    → the driver parked the conversation open itself
//     (snapshot written, status flipped): a paused turn, or a process quiet
//     past the idle backstop. The caller returns dormant, keeping the
//     worktree.
//   - fenced true       → a park this driver tried to write was refused: the
//     engagement's claim is gone and a successor owns the conversation. Not
//     hibernated, because nothing was parked; the caller records nothing at
//     all and leaves the workspace where it is.
//   - err set, no result → the process errored / was cancelled before any
//     terminal result, or the agent never corrected an invalid conclusion
//     envelope within the bound; the caller routes through parkConversationOpen /
//     failConversation.
type liveOutcome struct {
	result     *agentproc.Result
	sessionID  string
	stderr     string
	hibernated bool
	fenced     bool
	err        error
}

// liveRunSpec bundles everything runLiveAndDrive needs to spawn, register,
// drive, and park one live agent invocation.
type liveRunSpec struct {
	park        liveParkContext
	opts        agentproc.RunOptions
	perms       agentproc.PermissionHandler
	sink        agentproc.Sink
	idleTimeout time.Duration // <=0 disables the idle backstop (the bounded resume)
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
// ownership, and drives it to a terminal result or a park. The process is
// closed and the handle deregistered by the time this returns.
// Shared by the initial run path and the resume path so every run executes
// uniformly as a LiveRun.
func (s *Spawner) runLiveAndDrive(ctx context.Context, spec liveRunSpec) liveOutcome {
	// Buffered so the reader goroutine's OnResult / activity callbacks never
	// block on a driver that's momentarily not selecting (both use a
	// non-blocking send, but a buffer keeps the common case lock-free).
	results := make(chan *agentproc.Result, resultsBufferDepth)
	activity := make(chan struct{}, 64)

	spec.opts.OnResult = func(r *agentproc.Result) {
		// The driver consumes a result per turn: a conclusion closes the
		// process and returns, an invalid one is re-prompted (the next turn
		// produces the next result), and a no-conclusion turn closes it too
		// unless a steered turn is queued behind it (then the next turn
		// produces the next result). A full buffer means results are
		// arriving faster than the driver selects them; a non-blocking send
		// keeps the reader goroutine moving.
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
	s.registerProc(spec.park.orgID, spec.park.conversationID, lr)
	defer s.deregisterProc(spec.park.conversationID)
	// Stamp run→executor ownership now the process is live (N=1 instance id;
	// the lease layer horizontal scaling adds builds on this column).
	s.stampExecutor(spec.park.orgID, spec.park.conversationID, spec.park.claimID)

	out := s.driveLiveConversation(ctx, spec.park, lr, results, activity, spec.idleTimeout)
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

// driveLiveConversation is the select loop that resolves a live process into a
// disposition by classifying each turn-end into one of three buckets:
//
//   - valid conclusion → close the process and hand the result back for
//     orchestration (finalize / advance / fail-with-reason).
//   - invalid conclusion attempt (envelope-shaped but malformed / missing a
//     required field) → re-prompt the same live process to fix it, up to
//     maxCompletionRetries; fail the run if it never corrects.
//   - no conclusion (prose / nothing) → the run is open: close the process
//     and hand the result back, and processCompletion parks the conversation
//     with a snapshot. The next message wakes a fresh claim that resumes the
//     same session by id.
//
// A turn-end only closes the process when the process owes nothing more. A
// message steered in while the turn ran is queued by the SDK and starts the
// next turn on its own (proc.QueuedTurns), and that turn belongs to this
// engagement — its claim is still live — so the driver stays and reads it
// like any other. That is the whole of what survives a turn boundary:
// nothing is kept warm for a message that has not arrived, because a parked
// conversation has released its claim and the fence would kill the process
// on its first write.
//
// A paused turn (our own interrupt ended it) is a turn-end like the others:
// with nothing queued the process closes and the conversation parks open,
// and a queued steer is read next.
//
// The idle timer is the backstop against a process that has stopped
// producing without ending its turn: it resets on every stream activity, so
// a slow-but-working agent never trips it, and a genuinely quiet one is
// closed and parked. idleTimeout<=0 disables it — the bounded resume runs
// without one, so a turn there is bounded by the process itself (its result,
// its exit, or a stop), never by a clock.
//
// The idle window is armed at entry (process spawn). The first stream event —
// typically system/init, sub-second — resets it, so idleTimeout is effectively
// the grace a *no-output* process gets before parking; keep it well above
// agent-startup latency (the 5-min default is; a tiny injected value will
// park before the first turn, which is exactly what the idle test leans on).
//
// Pulled out from runLiveAndDrive so it can be driven with a fake proc +
// hand-fed channels in tests, without spawning a subprocess.
func (s *Spawner) driveLiveConversation(ctx context.Context, park liveParkContext, proc liveProc, results <-chan *agentproc.Result, activity <-chan struct{}, idleTimeout time.Duration) liveOutcome {
	idle, idleC := newIdleTimer(idleTimeout)
	if idle != nil {
		defer idle.Stop()
	}
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
			// so the conversation parks open for the composer's next message
			// rather than failing.
			if r.Interrupted {
				if proc.QueuedTurns() > 0 {
					// The pause has a steered turn behind it, and the process
					// starts it next.
					resetIdleTimer(idle, idleTimeout)
					continue
				}
				_ = proc.Close()
				if s.parkConversationOpen(ctx, park, proc.SessionID()) {
					return liveOutcome{fenced: true}
				}
				return liveOutcome{hibernated: true}
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
				// the live process and wait for the next turn.
				invalidAttempts++
				if err := proc.Send(ctx, invalidEnvelopeCorrection()); err != nil {
					_ = proc.Close()
					return liveOutcome{err: fmt.Errorf("re-prompt invalid completion envelope: %w", err)}
				}
				resetIdleTimer(idle, idleTimeout) // sending is activity

			case turnNone:
				if proc.QueuedTurns() > 0 {
					// The turn ended, but a message steered in while it ran is
					// queued and starts the next one — the conversation is
					// still being driven, under the same claim.
					resetIdleTimer(idle, idleTimeout)
					continue
				}
				// The turn ended without a conclusion and nothing is queued
				// behind it → the run is open (not executing, not concluded).
				// Close the process and hand the result back; processCompletion
				// parks the conversation with a snapshot, and the status flips
				// only once the process is gone — so the row never reads open
				// while a process that could still write to it is alive.
				_ = proc.Close()
				return liveOutcome{result: r}
			}

		case <-activity:
			resetIdleTimer(idle, idleTimeout)

		case <-idleC:
			// Quiet past the threshold with no turn-end in sight — the process
			// has stopped producing. Close it and park the run open to a
			// durable resume.
			_ = proc.Close()
			if s.parkConversationOpen(ctx, park, proc.SessionID()) {
				return liveOutcome{fenced: true}
			}
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

// markConversationOpen flips a conversation's status to `open` under a race
// guard, then nudges the board + UI. The shared flip for every park:
// parkConversationOpen (the process is gone — it opens the snapshot record
// first, then flips, then persists), and the dispatcher's setup-time parks,
// which have no workspace to snapshot yet. A cancel is a park too
// (park.reason names the stop). Nil-safe so the no-DB driver tests can
// exercise the loop.
//
// Routing has three arms, and which one a park takes says who is speaking. An
// engagement parking its own run (claimID set) goes through the claim fence,
// so a zombie executor cannot park a conversation its successor holds — and
// every dispatched run reaches this with a claim, deliberate stop and idle
// turn-end alike. The other two arms are what is left when there is no
// engagement to name: a manual run with no claim writes under the creator's
// synthetic claims, and everything else is a system write on the admin pool.
// Neither is zombie-reachable, because a zombie by definition held a claim.
//
// Returns fenced: true when the fence refused the write because this
// engagement's claim was released. Nothing was recorded or broadcast, and the
// caller must not act on the run's state either — it belongs to whoever holds
// the claim now. Always false on the two unfenced arms.
func (s *Spawner) markConversationOpen(ctx context.Context, park liveParkContext) (fenced bool) {
	if s.conversations == nil {
		return false // test fixture with no DB wired
	}
	// The same detachment this always had — a park must land even when the
	// run's ctx is already cancelled, which is the ordinary case here (a user
	// stop IS a cancel) — expressed as WithoutCancel so the write stays inside
	// the engagement's trace rather than orphaning into one of its own.
	bgCtx := context.WithoutCancel(ctx)
	var flipped bool
	var err error
	switch {
	case park.claimID != "":
		flipped, err = s.conversations.ParkOpenForClaimSystem(bgCtx, park.orgID, park.conversationID, park.claimID, park.reason)
	case park.triggerType == "manual":
		err = s.tx.SyntheticClaimsWithTx(bgCtx, park.orgID, park.creatorUserID, func(ts db.TxStores) error {
			f, e := ts.Conversations.ParkOpen(bgCtx, park.orgID, park.conversationID, park.reason)
			flipped = f
			return e
		})
	default:
		flipped, err = s.conversations.ParkOpenSystem(bgCtx, park.orgID, park.conversationID, park.reason)
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
		if park.reason.Deliberate {
			delegateLog.Info("claim fence refused the park after a deliberate stop — the stopping actor already parked this conversation; recording nothing further",
				"conversation", park.conversationID, "claim_id", park.claimID, "org_id", park.orgID)
			return true
		}
		delegateLog.Error("claim fence refused the park — a successor owns this conversation; recording nothing",
			"conversation", park.conversationID, "claim_id", park.claimID, "org_id", park.orgID, "error", err)
		return true
	}
	if err != nil {
		delegateLog.Warn("mark conversation open failed", "conversation", park.conversationID, "error", err)
		return false
	}
	if !flipped {
		// A racing terminal flip won, or this is an idle re-park of a row
		// already `open` — leave its status and say nothing.
		return false
	}
	s.broadcastConversationUpdate(park.orgID, park.conversationID, "open")
	s.recomputeTaskBoardColumn(park.orgID, park.taskID)
	return false
}

// parkConversationOpen records a run as `open` when its process is gone — a
// turn ended without a conclusion, the live driver closed a paused or quiet
// process, or someone cancelled it. It flips the status via
// markConversationOpen and then snapshots the workspace (the cold-resume
// backstop) so a resume that lands without the worktree can rebuild it. The
// process is closed before this is reached, always: a status that reads open
// while a process could still write to the row is a status the fence would
// have to kill. "open" makes no claim about why the run stopped or who
// continues it; any later input resumes it on the same ResumeWithMessage path.
//
// The cancel path is this same function, and that is the point: the old
// cancel handler wrote its own terminal and removed the worktree on its way
// out, which threw away the one thing a user who just killed a wedged run is
// likely to want back. A stop is a park with a reason attached.
func (s *Spawner) parkConversationOpen(ctx context.Context, park liveParkContext, sessionID string) (fenced bool) {
	// The flip does not wait on the capture, and the durable state record is
	// what makes that safe. It is opened FIRST — before the flip, never after —
	// so no observer can see a row that says resumable with neither a blob nor
	// an account of one: the record names a persist in flight and who owes it,
	// which the wake gate reads as recoverable (see workspaceRecoverable).
	// Reversed, the window between flip and record would answer "expired" for a
	// workspace that is being written.
	//
	// Everything the snapshot costs — a git capture, a tar, a blob PUT —
	// therefore falls after the status a person is watching. Best-effort as
	// ever, and skipped entirely with no workspace to capture (a cancel during
	// setup), which the persist would reject anyway. A record that could not be
	// opened does not hold up the park either: the flip goes ahead and the
	// persist below retries the open on its own way through.
	snapCtx := context.WithoutCancel(ctx)
	willSnapshot := park.claudeCwd != "" && park.namespace != "" && s.Storage() != nil
	leaseHeld := willSnapshot && s.beginSnapshotState(snapCtx, park.orgID, park.namespace, park.claimID)

	fenced = s.markConversationOpen(ctx, park)
	if fenced && leaseHeld && park.reason.Deliberate {
		// The one refusal whose `open` is real: control parked this row on the
		// user's behalf and released the claim before this teardown arrived
		// (see Spawner.stop), announcing that park while no persist was yet
		// owed — so a watcher reading the run then saw no workspace anywhere
		// and disabled its composer. The record opened above is what changes
		// that answer, so this is the moment to say so, not when the blob
		// lands.
		//
		// Gated on the record having landed, without which there is nothing to
		// announce. And on a DELIBERATE park: the other refusal — a successor
		// taking the conversation out from under a live engagement — leaves it
		// running under someone else, and repeating a parked status there would
		// be this teardown reporting a state that isn't the row's.
		s.broadcastConversationResumable(park.orgID, park.conversationID)
	}

	if willSnapshot {
		if err := s.persistWorkspaceSnapshot(snapCtx, park.orgID, park.conversationID, park.namespace, park.claimID, park.claudeCwd, sessionID, park.runtime, leaseHeld); err != nil {
			delegateLog.Warn("snapshot workspace after parking open failed", "conversation", park.conversationID, "error", err)
		}
	}
	if fenced {
		return true
	}
	// Only the idle park toasts. A deliberate stop terminates the blueprint
	// behind it, so "resumes on the next message" would be a promise this
	// build cannot keep — the claim gate refuses a parked step under a
	// finished blueprint until the resume work lands.
	if !park.reason.Deliberate {
		toast.Info(s.wsHub, park.orgID, fmt.Sprintf("Run %s is open — resumes on the next message", shortConversationID(park.conversationID)))
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
