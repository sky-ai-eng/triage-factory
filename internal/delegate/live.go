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
	"slices"

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
	namespace      string // task id — the snapshot/worktree key
	claudeCwd      string
	// claimID names the engagement writing the park, and is required: the
	// park goes through the claim fence and nowhere else. Every park a
	// dispatched run writes carries one — the idle turn-end as much as the
	// cancel, since a zombie's idle park would flip a conversation its
	// successor is mid-turn on.
	claimID string
	// reason is why: db.ParkIdle() for a turn that simply ended,
	// db.ParkStopped(...) for a cancel. See
	// ConversationStore.ParkOpenForClaimSystem.
	reason db.Park
	// costUSD is the spend this engagement's process reported — the SDK's
	// running total for the process, which is this claim's own figure since a
	// fresh process starts at zero — settled on the claim's rows before the
	// flip (see settleEngagementSpend). Zero settles nothing: the native
	// driver prices each assistant row as it goes and always passes zero.
	costUSD float64
	// runtime is the conversation's engine, for the workspace-snapshot span
	// family (see snapshotWorkspace). Each construction site states it from
	// what it structurally is — the SDK and native drivers each build their
	// own park — rather than re-reading the row.
	runtime string
	// mirror is the parking engagement's memory mirror, checked one last time
	// before the park writes anything. A park is an ending the agent may have
	// written right up to, and this is the last moment anyone holds its tree.
	// Nil for a caller with no tree — a cancel during bring-up, a fixture —
	// where there is nothing to file.
	mirror *memoryMirror
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
//     (snapshot written, status flipped) after a paused turn with nothing
//     queued behind it. The caller returns dormant, keeping the worktree.
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
	// costUSD is the process's reported spend on every exit, result or not —
	// the cancelled path parks with it, since a killed engagement reports its
	// total nowhere else. A dormant exit has already settled it in the park.
	costUSD float64
}

// liveRunSpec bundles everything runLiveAndDrive needs to spawn, register,
// drive, and park one live agent invocation.
type liveRunSpec struct {
	park  liveParkContext
	opts  agentproc.RunOptions
	perms agentproc.PermissionHandler
	sink  agentproc.Sink
	// mirror files the agent's memory file as it is written: the driver hands
	// it to the activity sink, which checks it on every tool row. The same
	// mirror is on park, so the engagement's ending sees whatever the last
	// tool call did not.
	mirror *memoryMirror
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
	// Buffered so the reader goroutine's OnResult callback never blocks on a
	// driver that's momentarily not selecting (it uses a non-blocking send,
	// but a buffer keeps the common case lock-free).
	results := make(chan *agentproc.Result, resultsBufferDepth)

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
	sink := newActivitySink(spec.sink, spec.mirror, s.activityFor(spec.park.conversationID), s.resolvedActivityTimings())

	lr, err := agentproc.RunInteractive(ctx, spec.opts, sink, spec.perms)
	if err != nil {
		return liveOutcome{err: err}
	}
	s.registerProc(spec.park.orgID, spec.park.conversationID, lr)
	defer s.deregisterProc(spec.park.conversationID)
	// Stamp run→executor ownership now the process is live (N=1 instance id;
	// the lease layer horizontal scaling adds builds on this column).
	s.stampExecutor(spec.park.orgID, spec.park.conversationID, spec.park.claimID)

	out := s.driveLiveConversation(ctx, spec.park, lr, results)
	// Capture the final session id / stderr off the (now-closed) process for
	// the caller's completion + failure paths.
	out.sessionID = lr.SessionID()
	out.stderr = lr.Stderr()
	out.costUSD = processSpend(lr)
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
// process-cumulative accounting (cost, duration, turns) from the merged
// fold. Everything else — the disposition processCompletion acts on — is the
// classified turn's own.
func foldAccounting(classified, merged *agentproc.Result) *agentproc.Result {
	r := *classified
	r.CostUSD = merged.CostUSD
	r.DurationMs = merged.DurationMs
	r.NumTurns = merged.NumTurns
	return &r
}

// processSpend is the spend a live process has reported so far: the running
// total the newest turn-end carried, zero before the first. Read at the
// moment the driver lets go of the process — a paused park, a cancel — so
// the engagement's figure rides into the park rather than dying with the
// process.
func processSpend(proc liveProc) float64 {
	if r := proc.Result(); r != nil {
		return r.CostUSD
	}
	return 0
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
// A process that has stopped producing without ending its turn is the
// engagement's stall watchdog's to decide (activity.go), not this loop's: the
// watchdog knows whether a tool call is in flight, which the stream cannot
// say, and it stops a stall by cancelling ctx, so the driver leaves through
// the same arm a user's stop takes. The same holds for the fresh run and the
// resume alike.
//
// Pulled out from runLiveAndDrive so it can be driven with a fake proc +
// hand-fed channels in tests, without spawning a subprocess.
func (s *Spawner) driveLiveConversation(ctx context.Context, park liveParkContext, proc liveProc, results <-chan *agentproc.Result) liveOutcome {
	tracker := s.activityFor(park.conversationID)
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
					continue
				}
				_ = proc.Close()
				park.costUSD = processSpend(proc)
				if s.parkConversationOpen(ctx, park, proc.SessionID()) {
					return liveOutcome{fenced: true, costUSD: park.costUSD}
				}
				return liveOutcome{hibernated: true, costUSD: park.costUSD}
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
				tracker.touch()

			case turnNone:
				if proc.QueuedTurns() > 0 {
					// The turn ended, but a message steered in while it ran is
					// queued and starts the next one — the conversation is
					// still being driven, under the same claim.
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
// which have no workspace to snapshot yet. A stop is a park too (park.reason
// names it, and a pending stop intent overrides the reason in the store).
// Nil-safe so the no-DB driver tests can exercise the loop.
//
// It writes through the claim fence and nothing else: a park is the holder's
// write, in one transaction with its claim release, so a zombie executor
// cannot park a conversation its successor holds. Every dispatched run
// reaches this with a claim, deliberate stop and idle turn-end alike, and a
// park with no claim to name is a caller bug — refused and logged at error,
// never written unfenced.
//
// Returns fenced: true when nothing was written because this engagement holds
// no live claim. Nothing was recorded or broadcast, and the caller must not
// act on the run's state either — it belongs to whoever holds the claim now,
// or, for a pending stop, to the dispatcher's settlement.
func (s *Spawner) markConversationOpen(ctx context.Context, park liveParkContext) (fenced bool) {
	if s.conversations == nil {
		return false // test fixture with no DB wired
	}
	if park.claimID == "" {
		delegateLog.Error("park without a claim id — every park is the holder's fenced write; recording nothing",
			"conversation", park.conversationID, "org_id", park.orgID)
		return true
	}
	// The same detachment this always had — a park must land even when the
	// run's ctx is already cancelled, which is the ordinary case here (a user
	// stop IS a cancel) — expressed as WithoutCancel so the write stays inside
	// the engagement's trace rather than orphaning into one of its own.
	bgCtx := context.WithoutCancel(ctx)
	flipped, err := s.conversations.ParkOpenForClaimSystem(bgCtx, park.orgID, park.conversationID, park.claimID, park.reason)
	if errors.Is(err, db.ErrClaimReleased) {
		// Whoever released the claim — expiry handling after a lapsed lease,
		// or a successor — owns what happens next, so this park is not this
		// engagement's state to report. The workspace stays too: it may be the
		// one a successor is running in. A stop that was pending stays pending
		// and the dispatcher settles it once no live claim holds the row.
		delegateLog.Error("claim fence refused the park — this engagement no longer holds the conversation; recording nothing",
			"conversation", park.conversationID, "claim_id", park.claimID, "org_id", park.orgID,
			"deliberate", park.reason.Deliberate, "error", err)
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
	return false
}

// parkConversationOpen records a run as `open` when its process is gone — a
// turn ended without a conclusion, the live driver closed a paused process,
// or someone (a person, or the stall watchdog) stopped it. It flips the status via
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
	if s.leaveConversation(ctx, park, sessionID, snapshotReasonPark, s.markConversationOpen) {
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

// handBackOnShutdown is the park an engagement takes when its dispatcher is
// shutting down: everything a park does, except that the conversation is not
// flipped `open`. Its claim is released 'requeued_shutdown' and the row stays
// mid-flight, so the next claim — on any executor — continues it at once,
// charged to neither budget: the native loop replays its transcript, the SDK
// resumes its session, and the workspace is this engagement's own, warm on the
// same host or rebuilt from the snapshot taken here. park.reason is not read.
//
// Parking instead would record a stop nobody made, and a parked conversation
// waits for a message nobody is going to send.
func (s *Spawner) handBackOnShutdown(ctx context.Context, park liveParkContext, sessionID string) (fenced bool) {
	return s.leaveConversation(ctx, park, sessionID, snapshotReasonShutdown, s.releaseClaimOnShutdown)
}

// leaveConversation is the ordered ending both of the above share, and
// release is the one step where they differ: the write that lets go of the
// claim. It returns release's answer — fenced when the engagement no longer
// held the claim, so nothing was written and the caller must not act on the
// conversation's state. reason names the ending on the snapshot's span.
//
// A native engagement's checkpointer has already been stopped and joined by
// the time this runs (recordNativeResult does it first): its record writes and
// its upload are the same writer to the key as the snapshot below, and one
// still in flight would close this ending's record or overwrite its blob.
func (s *Spawner) leaveConversation(ctx context.Context, park liveParkContext, sessionID, reason string, release func(context.Context, liveParkContext) bool) (fenced bool) {
	// Before anything else this ending writes: the agent's memory file, one
	// last time. It is an ending the agent may have written right up to, and
	// the snapshot below is not a substitute — it puts the file where only an
	// executor holding this tree can read it, while the row is what a handler
	// on another pod sees.
	//
	// It runs ahead of the release, so an ending the fence goes on to refuse
	// has filed anyway. That is the point rather than an oversight: an
	// engagement whose claim was released under it — a stop the dispatcher
	// will settle once expiry handling released the claim — is still the only
	// thing holding the file, and skipping it there would lose exactly the
	// notes this mirror exists to keep. A successor mid-flight is what
	// settle's unconditional write answers: the successor's own ending
	// re-asserts its file over anything a zombie filed first.
	park.mirror.settle(ctx)

	// The release does not wait on the capture, and the durable state record
	// is what makes that safe. It is opened FIRST — before the release, never
	// after — so no observer can see a row that says resumable with neither a
	// blob nor an account of one: the record names a persist in flight and who
	// owes it, which the wake gate reads as recoverable (see
	// workspaceRecoverable) and the next claim waits on (see ensureWorkspace).
	// Reversed, the window between release and record would answer "expired"
	// for a workspace that is being written.
	//
	// Everything the snapshot costs — a git capture, a tar, a blob PUT —
	// therefore falls after the release. For a park that is the status a
	// person is watching; for a shutdown it is the claim, which must be back
	// before the process's grace period can run out, or the conversation waits
	// out its lease and is counted as lost. Best-effort as ever, and skipped
	// entirely with no workspace to capture (a cancel during setup), which the
	// persist would reject anyway. A record that could not be opened does not
	// hold up the release either: the persist below retries the open on its
	// own way through.
	snapCtx := context.WithoutCancel(ctx)
	willSnapshot := park.claudeCwd != "" && park.namespace != "" && s.Storage() != nil
	leaseHeld := willSnapshot && s.beginSnapshotState(snapCtx, park.orgID, park.namespace, park.claimID)

	// Spend lands BEFORE the release: the broadcast after it is what makes
	// every watcher refetch, and the figure has to be on the ledger by then.
	// It also lands whether or not the release is refused: the engagement is
	// still the only holder of what its process spent.
	s.settleEngagementSpend(snapCtx, park)

	fenced = release(ctx, park)

	if willSnapshot {
		if err := s.persistWorkspaceSnapshot(snapCtx, snapshotWrite{
			orgID: park.orgID, conversationID: park.conversationID, keyID: park.namespace, claimID: park.claimID,
			wtPath: park.claudeCwd, sessionID: sessionID, runtime: park.runtime, reason: reason,
		}, leaseHeld); err != nil {
			delegateLog.Warn("snapshot workspace on leaving the conversation failed", "conversation", park.conversationID, "error", err)
		}
	}
	return fenced
}

// releaseClaimOnShutdown hands the engagement's claim back as a clean
// shutdown and leaves the conversation mid-flight. The write is fenced like a
// park's, and for the same reason: a successor that already holds the
// conversation owns it, and this engagement records nothing.
func (s *Spawner) releaseClaimOnShutdown(ctx context.Context, park liveParkContext) (fenced bool) {
	if s.conversationQueue == nil {
		return false // test fixture with no DB wired
	}
	if park.claimID == "" {
		delegateLog.Error("shutdown hand-back without a claim id — every release is the holder's fenced write; recording nothing",
			"conversation", park.conversationID, "org_id", park.orgID)
		return true
	}
	err := s.conversationQueue.ReleaseClaimOnShutdownSystem(context.WithoutCancel(ctx), park.orgID, park.conversationID, park.claimID)
	if errors.Is(err, db.ErrClaimReleased) {
		delegateLog.Error("claim fence refused the shutdown hand-back — this engagement no longer holds the conversation; recording nothing",
			"conversation", park.conversationID, "claim_id", park.claimID, "org_id", park.orgID, "error", err)
		return true
	}
	if err != nil {
		// The claim is still live, so the conversation is not lost: it is
		// taken over once the lease lapses, counted as a lost engagement.
		delegateLog.Warn("shutdown hand-back failed; the conversation is taken over after its claim lease",
			"conversation", park.conversationID, "claim_id", park.claimID, "error", err)
		return false
	}
	delegateLog.Info("handed the conversation back on shutdown; the next claim continues it",
		"conversation", park.conversationID, "claim_id", park.claimID)
	s.broadcastConversationUpdate(park.orgID, park.conversationID, domain.StatusQueued)
	return false
}

// settleEngagementSpend records what the parking engagement's process
// reported as spent, keyed to its claim, so a run that goes dormant counts
// toward the usage reads and the daily caps from the moment it parks rather
// than from whenever a resume happens to conclude. The SDK's figure is a
// per-process total and a resumed process starts at zero, so each
// engagement's lump is its own and engagements add on the ledger; the
// resumed claim's terminal write settles only its own row.
//
// Only a claim-holding engagement has a claim to key on, and only a figure
// above zero is a report at all (the native driver passes zero by design).
// Best-effort like the snapshot beside it: a failed settle is logged, never
// a reason to hold up the park. Nil-safe for the no-DB driver tests.
func (s *Spawner) settleEngagementSpend(ctx context.Context, park liveParkContext) {
	if s.conversations == nil || park.claimID == "" || park.costUSD == 0 {
		return
	}
	if err := s.conversations.SettleClaimCostSystem(ctx, park.orgID, park.conversationID, park.claimID, park.costUSD); err != nil {
		delegateLog.Warn("settle engagement spend at park failed; spend unrecorded",
			"conversation", park.conversationID, "claim_id", park.claimID, "cost_usd", park.costUSD, "error", err)
	}
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
		if outcome.Result != nil {
			out.costUSD = outcome.Result.CostUSD
		}
	}
	return out
}

// activitySink decorates a Sink so the engagement's stall tracker hears the
// stream as it is read: every line is activity, a tool_use is an operation
// in flight until its result, and a permission prompt is an operation of its
// own. It learns this through agentproc.StreamObserver, per line rather than
// per flushed message, because a message still streaming and a tool still
// running produce no Sink call at all.
//
// It is also where the SDK runtime mirrors the agent's memory file, on every
// tool row the stream produces. The stream is the only place this runtime
// learns that the agent did something: there is no per-call hook to hang the
// check on the way the native loop has one, and a tool row IS a resolved tool
// call (internal/agentproc/stream.go emits one per result).
//
// The observer methods run on the reader goroutine, which owns the tool
// bookkeeping below; nothing else touches it.
type activitySink struct {
	inner   agentproc.Sink
	mirror  *memoryMirror
	tracker *activityTracker
	timings activityTimings

	// pending is the tool calls whose result has not arrived, by tool-use id
	// in the order their tool_use was read, and toolNames their names.
	// endTool ends the operation the sink last began for them.
	pending   []string
	toolNames map[string]string
	endTool   func()
}

func newActivitySink(inner agentproc.Sink, mirror *memoryMirror, tracker *activityTracker, timings activityTimings) *activitySink {
	return &activitySink{
		inner:     inner,
		mirror:    mirror,
		tracker:   tracker,
		timings:   timings,
		toolNames: map[string]string{},
		endTool:   func() {},
	}
}

func (a *activitySink) OnSession(id string) error {
	return a.inner.OnSession(id)
}

func (a *activitySink) OnMessage(m *domain.Message) error {
	if err := a.inner.OnMessage(m); err != nil {
		// The row did not land, so this one is not a moment to file anything.
		// It matters most for the fence — a refused write means a successor
		// owns the conversation and this engagement may write nothing more
		// about it, the memory row included — and costs nothing on the
		// per-row failures, where the next tool row repeats the check.
		return err
	}
	if m != nil && m.Role == "tool" {
		// The reader goroutine has no caller context to bound this by, so it
		// gets its own: a check that blocks would stall the stream.
		ctx, cancel := context.WithTimeout(context.Background(), detachedWriteDeadline)
		a.mirror.check(ctx)
		cancel()
	}
	return nil
}

func (a *activitySink) OnLine() {
	a.tracker.touch()
}

func (a *activitySink) OnToolUse(id, name string) {
	if _, seen := a.toolNames[id]; !seen {
		a.pending = append(a.pending, id)
	}
	a.toolNames[id] = name
	a.trackTools()
}

func (a *activitySink) OnToolResult(id string) {
	if _, pending := a.toolNames[id]; !pending {
		return
	}
	delete(a.toolNames, id)
	a.pending = slices.DeleteFunc(a.pending, func(p string) bool { return p == id })
	a.trackTools()
}

// OnTurnEnd ends every tool call the turn left without a result: the turn is
// over, so none of them is still running.
func (a *activitySink) OnTurnEnd() {
	a.pending = a.pending[:0]
	clear(a.toolNames)
	a.trackTools()
}

// trackTools points the tracker at the pending calls, and ends the operation
// once none is left. The operation is named for the oldest pending call and
// runs a full tool bound from now, so every tool_use and every result starts
// it again: the stream does not say whether the calls still pending ran
// beside the one that returned or were queued behind it, and a queued one
// starts only now. The watchdog stops the batch once it goes a whole tool
// bound without a result.
func (a *activitySink) trackTools() {
	if len(a.pending) == 0 {
		a.endTool()
		a.endTool = func() {}
		return
	}
	a.endTool = a.tracker.begin("tool:"+a.toolNames[a.pending[0]], a.timings.toolCall)
}

// OnPermission brackets a permission prompt as its own operation. The prompt
// replaces the pending calls' operation while a person decides, and they are
// in flight again, from then, once the person has: an approved tool runs after
// the wait, and the time it runs is the tool's. A call whose tool_use has not
// been read yet begins when it is, as any other does. The reader goroutine is
// parked in the prompt, so no tool_use or result is read during it.
//
// The operation's deadline sits past the prompt's own timeout, so a prompt
// nobody answers is denied and the agent carries on; the watchdog only stops
// a wait that outlives its own timeout.
func (a *activitySink) OnPermission(string) func() {
	end := a.tracker.begin("permission", a.timings.permission+backstopMargin)
	return func() {
		end()
		a.trackTools()
	}
}

// Compile-time checks that activitySink satisfies the Sink contract and the
// observer the reader looks for.
var (
	_ agentproc.Sink           = (*activitySink)(nil)
	_ agentproc.StreamObserver = (*activitySink)(nil)
)
