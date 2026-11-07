// Live-run execution: every SDK run executes as a long-lived
// agentproc.LiveRun (a streaming-input process you can message/interrupt). The
// driver here turns one live invocation into a terminal disposition — a
// turn-terminal result, a park, or a process exit — and feeds the post-stream
// branching (processCompletion).
//
// The process lives exactly as long as its engagement's claim. A turn that
// ends without concluding closes the process and parks the conversation,
// and a follow-up wakes a fresh claim that resumes the same session by id;
// the one thing that keeps a process alive past a turn boundary is input
// waiting for it — a person's message or a new-event injection that arrived
// while the last turn ran and that no tool batch took — which the driver
// sends as the next turn. Nothing is kept warm between turns, because a
// parked conversation has no claim, and every transcript write an engagement
// makes is fenced on its claim: a process left alive past its park would be
// killed by the fence on its next word.
//
// Nothing is sent into a running turn. Claude Code folds a message sent
// during a tool call into the turn at its next tool batch, and queues one sent
// during a text-only stretch as a turn of its own, so the driver could not
// tell which turn answered a message. Input for a live process is written to
// the transcript as pending instead, and reaches the model at the next tool
// batch (notesAtToolBatch) or as the next turn the driver sends.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/tooldefs"
	"github.com/sky-ai-eng/triage-factory/internal/wsevent/toast"
)

// liveProc is the slice of *agentproc.LiveRun the driver loop needs. Pulled
// out as an interface so driveLiveConversation is unit-testable with a fake process
// (no subprocess) — the real *agentproc.LiveRun satisfies it. Send starts the
// process's next turn, and the driver calls it only between turns (the
// turn-end nudge, input waiting for the process).
type liveProc interface {
	Done() <-chan struct{}
	Result() *agentproc.Result
	SessionID() string
	Stderr() string
	Err() error
	Send(ctx context.Context, text string) error
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
	// cell is the engagement's cell, which can hand the tree to the
	// engagement's jail identity when the ending has to capture a tree no jail
	// holds (treeToCapture). Nil unjailed, and for a caller with no cell.
	cell treeHandOff
}

// treeHandOff is what an ending needs of its cell: handing the run tree to the
// engagement's jail identity, to be taken back with the cell's other grants.
// The engagement's cell.Cell in production.
type treeHandOff interface {
	HandOffRunTree(ctx context.Context, root string) error
}

// liveOutcome is the disposition of one agent invocation, the single shape
// runAgent / ResumeWithMessage branch on:
//
//   - result set        → a turn produced its step's verdict, ended on a stop
//     reason that is not an end of turn, or is an IsError / crash result; the
//     caller runs processCompletion (finalize / advance / fail-with-reason /
//     park open). The process is already closed.
//   - hibernated true    → the driver parked the conversation open itself
//     (snapshot written, status flipped) after a paused turn with nothing
//     queued behind it. The caller returns dormant, keeping the worktree.
//   - fenced true       → a park or nudge row this driver tried to write was
//     refused: the engagement's claim is gone and a successor owns the
//     conversation. Not hibernated, because nothing was parked; the caller
//     records nothing at all and leaves the workspace where it is.
//   - err set, no result → the process errored / was cancelled before any
//     terminal result; the caller routes through parkConversationOpen /
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
	// nudge is the would-stop hook (artifactContractNudge), asked before a
	// turn concludes on its own: a non-empty note is sent into the process
	// and the run goes on. Nil asks nothing.
	nudge func(context.Context) string
}

// sdkTFTools are the TF tools an SDK delegation is offered. Every SDK
// conversation the spawner drives is a delegation, which executes a
// blueprint, as a native one does.
func sdkTFTools() []tooldefs.Tool {
	return tooldefs.TFTools(tooldefs.Facts{HasBlueprint: true})
}

// resultsBufferDepth sizes the OnResult channel runLiveAndDrive hands the
// driver. Each turn produces one result and the driver consumes one per loop
// iteration, and the driver starts every turn after the first itself, between
// turns: for waiting input, or the nudge. So only a couple of results are
// ever unread at once, and the depth is headroom over that.
// (The non-blocking send only ever sheds a turn the driver has already moved
// past; see OnResult.)
const resultsBufferDepth = 8

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
		// The driver consumes a result per turn: a conclusion or a park
		// closes the process and returns, unless the driver starts another
		// turn (for input waiting for the process, or the nudge), and then
		// that turn produces the next result. A full buffer means results are
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
	// The notes stageNoteAtToolBoundary armed ride the next tool batch.
	spec.opts.OnToolBatch = func() agentproc.ToolBatchNotes {
		return s.notesAtToolBatch(spec.park.orgID, spec.park.conversationID, spec.park.claimID)
	}
	spec.opts.TFTools = sdkTFTools()
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

	out := s.driveLiveConversation(ctx, spec.park, lr, results, spec.nudge)
	// Capture the final session id / stderr off the (now-closed) process for
	// the caller's completion + failure paths.
	out.sessionID = lr.SessionID()
	out.stderr = lr.Stderr()
	out.costUSD = processSpend(lr)
	// The driver hands back the per-turn result it decided on; the live process
	// folds every turn (pause turns, steered and nudged turns, the conclusion)
	// into its merged Result. Take ONLY the accounting fields from that fold —
	// disposition (IsError, Subtype, StopReason, Terminal, the final text,
	// Interrupted) stays with the turn the driver classified. Replacing the
	// result wholesale would let MergeResult's sticky IsError from a benign
	// interrupted (pause) turn flip a valid conclusion into status=failed
	// downstream — processCompletion checks IsError before the verdict.
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
// disposition, one turn-end at a time:
//
//   - a turn our own interrupt ended → a pause: the process closes and the
//     conversation parks open itself (hibernated), unless a person's message
//     is waiting, which the pause does not stand over.
//   - an IsError result → handed back for processCompletion to fail.
//   - a turn stop_blueprint ended → its verdict, handed back to orchestrate.
//     Input waiting for the process stays on the transcript, and the wake
//     that follows the blueprint's reaction delivers it (wakeForUnreadInput).
//   - a turn the model ended itself → the step concludes `continue`, after
//     the two checks the native loop makes before the same conclusion (see
//     below), and is handed back.
//   - a turn that ended on any other stop reason → handed back for
//     processCompletion to park open with a note saying why.
//
// A turn-end only closes the process when nothing that keeps a finishing turn
// going is waiting for it (agentloop.KeepsAFinishingTurnGoing): a person's
// message or a new-event injection that arrived while the turn ran and that
// no tool batch took. The driver sends that input as the next turn, which
// belongs to this engagement — its claim is still live — and reads it like
// any other. That is the whole of what survives a turn boundary: nothing is
// kept warm for a message that has not arrived, because a parked conversation
// has released its claim and the fence would kill the process on its first
// write.
//
// Before a turn concludes on its own, the driver asks what the native loop's
// would-stop step asks. Input waiting for the process is work the model has
// not seen, so its turn runs first. Then nudge, the would-stop hook: a note it
// returns is sent into the process and recorded on the transcript, and the
// turn that answers it is the next one read. Once a note has been sent the
// hook is not asked again in this engagement: it reads the recorded note back
// to know it has asked, and a note that failed to record would otherwise be
// asked again on every turn. A stop_blueprint ending asks neither, because the
// call is the verdict, as it is natively.
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
func (s *Spawner) driveLiveConversation(ctx context.Context, park liveParkContext, proc liveProc, results <-chan *agentproc.Result, nudge func(context.Context) string) liveOutcome {
	tracker := s.activityFor(park.conversationID)
	nudged := false

	for {
		select {
		case <-ctx.Done():
			// Hard cancel: the registered ctx cancel SIGKILLed the process.
			// Close (idempotent) and surface the ctx error so the caller routes
			// through its cancelled path.
			s.handOnOrLetGo(ctx, park, proc, nil)
			return liveOutcome{err: ctx.Err()}

		case r := <-results:
			// A turn we interrupted ourselves is a pause, not a failure. The SDK
			// wire-labels it is_error/error_during_execution — shape-identical
			// to a real runtime error — but the agentproc reader marks it
			// first-class (Result.Interrupted, from the wrapper's
			// control/interrupted ack), the same signal Claude Code's own UI
			// uses to render Esc gracefully. The session survives an interrupt,
			// so the conversation parks open for the composer's next message
			// rather than failing. A person's message waiting for the process
			// is what they want done next, so it is the pause's next turn; a
			// new-event injection waits for the person's next message.
			if r.Interrupted {
				next, fenced := s.handOnOrLetGo(ctx, park, proc, startsATurnAfterAPause)
				if fenced {
					return liveOutcome{fenced: true, costUSD: processSpend(proc)}
				}
				if next {
					tracker.touch()
					continue
				}
				park.costUSD = processSpend(proc)
				if s.parkConversationOpen(ctx, park, proc.SessionID()) {
					return liveOutcome{fenced: true, costUSD: park.costUSD}
				}
				return liveOutcome{hibernated: true, costUSD: park.costUSD}
			}
			// An IsError result (max-turns, runtime error) is terminal —
			// hand it back; processCompletion fails it.
			if r.IsError {
				s.handOnOrLetGo(ctx, park, proc, nil)
				return liveOutcome{result: r}
			}
			// The model ended this turn, and input that arrived while it ran
			// is waiting: it is the next turn, ahead of the nudge. Whatever
			// this turn was about to conclude or park on, the model has not
			// seen it.
			starts := startsATurn
			if r.Terminal != nil {
				starts = nil
			} else {
				sent, fenced := s.sendWaitingInput(ctx, park, proc, starts)
				if fenced {
					s.handOnOrLetGo(ctx, park, proc, nil)
					return liveOutcome{fenced: true, costUSD: processSpend(proc)}
				}
				if sent {
					tracker.touch()
					continue
				}
			}
			if r.Terminal == nil && sdkTurnFinished(r.StopReason) && nudge != nil && !nudged {
				if note := nudge(ctx); note != "" {
					sent, fenced := s.sendNudge(ctx, park, proc, note)
					if fenced {
						s.handOnOrLetGo(ctx, park, proc, nil)
						return liveOutcome{fenced: true, costUSD: processSpend(proc)}
					}
					if sent {
						nudged = true
						tracker.touch()
						continue
					}
				}
			}
			// The step's verdict, or a turn processCompletion parks. The
			// process is let go of before the result is handed back, so the
			// row never reads open while a process that could still write to
			// it is alive. Input that arrived since the check above keeps the
			// turn going after all.
			next, fenced := s.handOnOrLetGo(ctx, park, proc, starts)
			if fenced {
				return liveOutcome{fenced: true, costUSD: processSpend(proc)}
			}
			if next {
				tracker.touch()
				continue
			}
			return liveOutcome{result: r}

		case <-proc.Done():
			// The process exited on its own (crash, or a Close from elsewhere).
			// Hand back whatever terminal result was folded, else the error.
			s.handOnOrLetGo(ctx, park, proc, nil)
			return liveOutcome{result: proc.Result(), err: proc.Err()}
		}
	}
}

// startsATurn and startsATurnAfterAPause say which input waiting for a live
// process at a turn end is worth a turn of its own. A turn the model ended
// goes on for what a finishing native loop goes on for. A pause goes on only
// for a person's message.
func startsATurn(r domain.Message) bool {
	return pendingRow(r) && agentloop.KeepsAFinishingTurnGoing(r)
}

func startsATurnAfterAPause(r domain.Message) bool {
	return waitingMessage(r)
}

// handOnOrLetGo decides, at a turn end, whether the process takes another
// turn. When starts finds input waiting, that input is sent as the next turn
// and next is true. Otherwise the process is let go of: closed, with its
// handle marked so that nothing more is queued for it. A nil starts lets go
// without looking. fenced reports that the claim fence refused delivering the
// input; the process is let go of then too.
//
// The decision is made under the handle's input lock, which queueForLiveProcess
// holds while it writes a person's message, so a message queued before the
// decision is seen by it and one queued after finds the process gone. The
// process is closed after the lock is released, so a send waiting on the lock
// is not held for the close.
func (s *Spawner) handOnOrLetGo(ctx context.Context, park liveParkContext, proc liveProc, starts func(domain.Message) bool) (next, fenced bool) {
	h := s.getProc(park.conversationID)
	if h != nil {
		h.input.Lock()
	}
	if starts != nil {
		next, fenced = s.sendWaitingInput(ctx, park, proc, starts)
	}
	if !next && h != nil {
		h.lettingGo = true
	}
	if h != nil {
		h.input.Unlock()
	}
	if !next {
		_ = proc.Close()
	}
	return next, fenced
}

// sendWaitingInput sends the input waiting for a live process as its next
// turn when starts finds any of it worth one: every staged note, bundled
// ahead of the people's messages, which is how a resume composes its first
// message. Every waiting row rides the turn once it is sent, the notes that
// would not have started one included.
//
// The rows are flushed, marked delivered, before the send, and put back if
// the send fails, as notesAtToolBatch does. sent reports that the turn
// started. fenced reports that the claim fence refused the flush: a successor
// owns the conversation and nothing was sent.
func (s *Spawner) sendWaitingInput(ctx context.Context, park liveParkContext, proc liveProc, starts func(domain.Message) bool) (sent, fenced bool) {
	rows, ok := s.waitingRows(ctx, park.orgID, park.conversationID)
	if !ok || !slices.ContainsFunc(rows, starts) {
		return false, false
	}
	messages, fenced := s.takeWaitingMessages(ctx, park.orgID, park.conversationID, park.claimID, rows)
	if fenced {
		return false, true
	}
	notes := s.flushStagedInjections(ctx, park.orgID, park.conversationID)
	if len(messages) == 0 && len(notes) == 0 {
		return false, false
	}
	var parts []string
	if len(notes) > 0 {
		parts = append(parts, domain.StagedInjectionBlock(notes))
	}
	for _, m := range messages {
		parts = append(parts, m.Content)
	}
	if err := proc.Send(ctx, strings.Join(parts, db.PendingInputSeparator)); err != nil {
		delegateLog.Warn("send the input waiting for the live process failed; it waits for the next engagement", "conversation", park.conversationID, "error", err)
		s.restoreStagedInjections(park.orgID, park.conversationID, notes)
		s.requeueMessages(park.orgID, park.conversationID, park.claimID, messages)
		return false, false
	}
	return true, false
}

// sendNudge sends the would-stop hook's note into the live process and records
// it on the transcript, where the hook reads it back to know it has asked
// (askedAboutArtifactAlready). Sent before it is recorded: a row written for a
// message the process never took would say the agent was asked something it
// was not.
//
// sent is false when the process would not take the note; the turn then
// concludes as it would have without one. fenced is true when the claim fence
// refused the row: a successor owns the conversation, and nothing more is this
// engagement's to write.
func (s *Spawner) sendNudge(ctx context.Context, park liveParkContext, proc liveProc, note string) (sent, fenced bool) {
	if err := proc.Send(ctx, note); err != nil {
		delegateLog.Warn("send the turn-end nudge failed; the turn concludes without it", "conversation", park.conversationID, "error", err)
		return false, false
	}
	return true, s.insertClaimNote(ctx, park.orgID, park.claimID, &domain.Message{
		ConversationID: park.conversationID,
		Role:           "user",
		Subtype:        domain.MessageSubtypeInjectionNudge,
		Content:        note,
	})
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
	fenced, _ = s.markConversationOpenFlipped(ctx, park)
	return fenced
}

// markConversationOpenFlipped is markConversationOpen, also reporting whether
// this call flipped the row open. flipped is false whenever fenced is, and
// also for a write that failed and for a row a racing terminal or an earlier
// park had already settled: none of those parked anything.
func (s *Spawner) markConversationOpenFlipped(ctx context.Context, park liveParkContext) (fenced, flipped bool) {
	if s.conversations == nil {
		return false, false // test fixture with no DB wired
	}
	if park.claimID == "" {
		delegateLog.Error("park without a claim id — every park is the holder's fenced write; recording nothing",
			"conversation", park.conversationID, "org_id", park.orgID)
		return true, false
	}
	// The same detachment this always had — a park must land even when the
	// run's ctx is already cancelled, which is the ordinary case here (a user
	// stop IS a cancel) — expressed as WithoutCancel so the write stays inside
	// the engagement's trace rather than orphaning into one of its own.
	bgCtx := context.WithoutCancel(ctx)
	s.releaseActivity(park.conversationID, park.claimID)
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
		return true, false
	}
	if err != nil {
		delegateLog.Warn("mark conversation open failed", "conversation", park.conversationID, "error", err)
		return false, false
	}
	if !flipped {
		// A racing terminal flip won, or this is an idle re-park of a row
		// already `open` — leave its status and say nothing.
		return false, false
	}
	s.broadcastConversationUpdate(park.orgID, park.conversationID, "open")
	return false, true
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
	fenced, _ = s.parkConversationOpenFlipped(ctx, park, sessionID)
	return fenced
}

// parkConversationOpenFlipped is parkConversationOpen, also reporting whether
// this call is the one that parked the row, in markConversationOpenFlipped's
// sense: a caller that counts or announces a park reads flipped, because
// fenced false does not mean the park landed.
func (s *Spawner) parkConversationOpenFlipped(ctx context.Context, park liveParkContext, sessionID string) (fenced, flipped bool) {
	release := func(ctx context.Context, park liveParkContext) bool {
		var refused bool
		refused, flipped = s.markConversationOpenFlipped(ctx, park)
		return refused
	}
	if s.leaveConversation(ctx, park, sessionID, snapshotReasonPark, release) {
		return true, false
	}
	// Only the idle park toasts. A deliberate stop terminates the blueprint
	// behind it, so "resumes on the next message" would be a promise this
	// build cannot keep — the claim gate refuses a parked step under a
	// finished blueprint until the resume work lands.
	if !park.reason.Deliberate {
		toast.Info(s.wsHub, park.orgID, fmt.Sprintf("Run %s is open — resumes on the next message", shortConversationID(park.conversationID)))
	}
	return false, flipped
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

// handBackOnUpstream is the shutdown hand-back for an engagement whose model
// provider stayed unavailable through its own retries: the same ordered
// ending, a snapshot of the tree included, except that the claim is released
// 'requeued_upstream' and the conversation waits delay before its next claim.
// The cause is still there, so a successor claimed at once would only meet it
// again. lastErr is the provider's last failure, kept on the row so a person
// looking at a run that is waiting can see what it is waiting out. sessionID
// is the SDK session the snapshot carries, "" for a native engagement.
//
// Nothing is written to the transcript: the next engagement continues it from
// the call that failed, and for the model nothing has happened yet.
func (s *Spawner) handBackOnUpstream(ctx context.Context, park liveParkContext, sessionID string, delay time.Duration, lastErr string) (fenced bool) {
	return s.leaveConversation(ctx, park, sessionID, snapshotReasonUpstream, func(ctx context.Context, park liveParkContext) bool {
		return s.handBackClaim(ctx, park, db.HandBackUpstream, delay, lastErr)
	})
}

// handBackOnLostJail is the shutdown hand-back for an engagement whose jail
// died under it and could not be restarted in place: the same ordered ending,
// a snapshot of the tree as the jail left it included, except that the claim
// is released 'requeued_jail_lost'. It is claimable at once: the next claim
// builds a new cell, and the model reads what ended the old jail on the call
// that was running. reason is kept on the row so a person looking at the
// queued run sees why it is starting over.
//
// Nothing is written to the transcript here: the loop already answered every
// call the jail took with it.
func (s *Spawner) handBackOnLostJail(ctx context.Context, park liveParkContext, reason string) (fenced bool) {
	return s.leaveConversation(ctx, park, "", snapshotReasonJailLost, func(ctx context.Context, park liveParkContext) bool {
		return s.handBackClaim(ctx, park, db.HandBackJailLost, 0, reason)
	})
}

// leaveConversation is the ordered ending every one of the above shares, and
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
	// persist would reject anyway, or with a tree that holds nothing the last
	// snapshot lacks (treeToCapture). A record that could not be opened does
	// not hold up the release either: the persist below retries the open on its
	// own way through. A key a newer engagement already holds is left to it,
	// and nothing is captured.
	snapCtx := context.WithoutCancel(ctx)
	willSnapshot := park.claudeCwd != "" && park.namespace != "" && s.Storage() != nil
	var adoptTree func(context.Context) error
	if willSnapshot {
		willSnapshot, adoptTree = s.treeToCapture(snapCtx, park)
	}
	leaseHeld := false
	if willSnapshot {
		record := s.beginSnapshotState(snapCtx, park.orgID, park.namespace, park.claimID)
		leaseHeld = record.owned()
		willSnapshot = record != snapshotSuperseded
	}

	// Spend lands BEFORE the release: the broadcast after it is what makes
	// every watcher refetch, and the figure has to be on the ledger by then.
	// It also lands whether or not the release is refused: the engagement is
	// still the only holder of what its process spent.
	s.settleEngagementSpend(snapCtx, park)

	fenced = release(ctx, park)

	// The release ended the stall watchdog with the claim, so the snapshot's
	// bound is the only thing that ends a capture or an upload that stops
	// answering. It is a workspace operation's bound, the same one a
	// snapshot taken before a terminal runs under.
	if willSnapshot {
		persistCtx, cancel := context.WithTimeout(snapCtx, s.resolvedActivityTimings().workspaceOp)
		err := s.persistWorkspaceSnapshot(persistCtx, snapshotWrite{
			orgID: park.orgID, conversationID: park.conversationID, keyID: park.namespace, claimID: park.claimID,
			wtPath: park.claudeCwd, sessionID: sessionID, runtime: park.runtime, reason: reason,
			adoptTree: adoptTree,
		}, leaseHeld)
		cancel()
		if err != nil {
			delegateLog.Warn("snapshot workspace on leaving the conversation failed", "conversation", park.conversationID, "error", err)
		}
	}
	return fenced
}

// treeToCapture decides whether an ending captures its tree, and returns the
// hand-off the capture needs first when it does.
//
// Where the agent is jailed, the capture runs as the jail identity holding the
// tree, which is this engagement's once its jail has run in it. A tree no jail
// holds — parked by the engagement before this one, or built by this one and
// never launched into — has nothing in it that the key's last snapshot lacks,
// unless that snapshot never landed: then this ending is the only thing left
// that can put the work in a blob, and it hands the tree to its own jail
// identity to capture it, through the network that takes the tree back with
// the rest of its grants. A record that cannot be read counts as one that did
// not land. Unjailed, the capture runs in-process and needs no identity.
func (s *Spawner) treeToCapture(ctx context.Context, park liveParkContext) (capture bool, adoptTree func(context.Context) error) {
	if !agentproc.WillSandbox() {
		return true, nil
	}
	if _, held := sandbox.RunTreeJailUID(park.claudeCwd); held {
		return true, nil
	}
	if _, err := os.Lstat(park.claudeCwd); err != nil {
		return false, nil
	}
	if st, err := s.snapshotStateFor(ctx, park.orgID, park.namespace); err == nil && st != nil && st.State == domain.WorkspaceSnapshotWritten {
		return false, nil
	}
	if park.cell == nil {
		delegateLog.Warn("the run tree holds work its last snapshot does not, and this ending has no jail identity to capture it as",
			"conversation", park.conversationID, "key_id", park.namespace)
		return false, nil
	}
	return true, func(ctx context.Context) error {
		return park.cell.HandOffRunTree(ctx, park.claudeCwd)
	}
}

// releaseClaimOnShutdown hands the engagement's claim back as a clean
// shutdown, claimable at once.
func (s *Spawner) releaseClaimOnShutdown(ctx context.Context, park liveParkContext) (fenced bool) {
	return s.handBackClaim(ctx, park, db.HandBackShutdown, 0, "")
}

// handBackClaim releases the engagement's claim with a hand-back outcome and
// leaves the conversation mid-flight, claimable after delay (at once for 0).
// The write is fenced like a park's, and for the same reason: a successor that
// already holds the conversation owns it, and this engagement records nothing.
// A lapsed lease is refused the same way unless a suspend recovery takes the
// claim back; the takeover then releases it as reaped.
func (s *Spawner) handBackClaim(ctx context.Context, park liveParkContext, outcome string, delay time.Duration, lastErr string) (fenced bool) {
	if s.conversationQueue == nil {
		return false // test fixture with no DB wired
	}
	if park.claimID == "" {
		delegateLog.Error("hand-back without a claim id — every release is the holder's fenced write; recording nothing",
			"conversation", park.conversationID, "org_id", park.orgID, "outcome", outcome)
		return true
	}
	s.releaseActivity(park.conversationID, park.claimID)
	writeCtx := context.WithoutCancel(ctx)
	err := s.retryAfterRecovery(writeCtx, park.claimID, func() error {
		return s.conversationQueue.HandBackClaimSystem(writeCtx, park.orgID, park.conversationID, park.claimID, outcome, delay, lastErr)
	})
	if errors.Is(err, db.ErrClaimReleased) {
		delegateLog.Error("claim fence refused the hand-back — this engagement no longer holds the conversation; recording nothing",
			"conversation", park.conversationID, "claim_id", park.claimID, "org_id", park.orgID, "outcome", outcome, "error", err)
		return true
	}
	if err != nil {
		// The claim is still live, so the conversation is not lost: it is
		// taken over once the lease lapses, counted as a lost engagement.
		delegateLog.Warn("hand-back failed; the conversation is taken over after its claim lease",
			"conversation", park.conversationID, "claim_id", park.claimID, "outcome", outcome, "error", err)
		return false
	}
	recordHandBack(park.orgID, outcome, 1)
	if delay > 0 {
		delegateLog.Info("handed the conversation back; the next claim continues it once the wait is over",
			"conversation", park.conversationID, "claim_id", park.claimID, "outcome", outcome, "delay", delay)
	} else {
		delegateLog.Info("handed the conversation back; the next claim continues it",
			"conversation", park.conversationID, "claim_id", park.claimID, "outcome", outcome)
	}
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
