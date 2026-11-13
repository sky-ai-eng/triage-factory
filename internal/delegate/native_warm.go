// A native engagement from agent-live to its ending, with warm claims: after a
// turn a person took part in, the turn writes exactly what it always writes,
// the verdict included, and the engagement then keeps its claim, its cell and
// its jail for the warm window instead of letting go. The conversation is not
// touched by any of that; it reads as the turn left it. A person's message
// resumes the conversation as it would any other, and the engagement still
// holding the claim runs the next turn, which saves the new claim's bring-up.
//
// The wait sits here, around engine.Run, rather than inside the loop. Re-entering
// Run is already how every claim drives a transcript, a re-entry after a long
// gap takes the loop's cold compaction on the way in, and the claim, the
// watchdog operation and the registry are this package's.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/wsevent/toast"
)

// warmWaitOp is the watchdog operation a warm claim's wait is reported under.
// It does not start with agentloop.ToolActivityOpPrefix, so the tool-overrun
// hook leaves it alone, and its bound is the window's, so the idle limit does
// not apply while it runs.
const warmWaitOp = "warm_wait"

// warmWaitBackstop is how often a warm engagement rereads its conversation
// without being woken, in case a wake was lost. A var so tests can shorten it.
var warmWaitBackstop = 15 * time.Second

// nativeTail is a native engagement past agent-live: what its turns run with,
// and what its ending needs.
type nativeTail struct {
	orgID          string
	conversationID string
	task           domain.Task
	cfg            runConfig
	namespace      string
	claudeCwd      string
	creatorUserID  string
	startTime      time.Time
	mirror         *memoryMirror

	// run drives one turn: engine.Run. params is what the engagement's first
	// turn runs under, and its Model is the one every turn runs on.
	run        func(context.Context, agentloop.Params) agentloop.Result
	params     agentloop.Params
	transcript *nativeTranscript
	// ckpt is the engagement's checkpointer, nil when it takes none.
	ckpt *checkpointer
	// release stops the jail and closes the tool host. Every ending reads the
	// tree or gives up the claim on it, so it runs before any of them.
	release func()
}

// runNativeTail runs the engagement's turns. After each one that opens a
// window, the turn's state is written and the claim is kept warm; a person's
// message runs the next turn on it, and anything else lets it go.
func (s *Spawner) runNativeTail(ctx context.Context, n nativeTail) engagementDisposition {
	params := n.params
	result := n.run(ctx, params)
	for {
		// The window is measured from the turn end, so a long turn a message
		// started still gets all of it, and the turn's own writes come out of
		// it.
		deadline := time.Now().Add(s.WarmWindow())
		if !s.warmWindowOpens(ctx, n, result, n.transcript.takePersonDelivered()) {
			return s.endNativeTurn(ctx, n, result)
		}
		switch held := s.concludeKeepingClaim(ctx, n, result); held {
		case holdFenced:
			return s.endWarmFenced(n, result)
		case holdNotKept:
			// A stop or a shutdown that cancelled the turn-end snapshot
			// reached a turn that had already concluded, and the conclusion
			// is what is recorded, on a context the cancel does not reach: on
			// the cancelled one the ending's own snapshot would fail too. A
			// fence is left on ctx, where recordNativeResult writes nothing.
			if ctx.Err() != nil && !leaseFenced(ctx) {
				return s.endNativeTurn(context.WithoutCancel(ctx), n, result)
			}
			return s.endNativeTurn(ctx, n, result)
		}

		switch s.waitWarm(ctx, n, deadline) {
		case warmResumed:
			conv := s.warmResumeModelHolds(context.WithoutCancel(ctx), n)
			if conv == nil {
				return s.letGoWarm(context.WithoutCancel(ctx), n, result)
			}
			resumed, err := s.conversations.ResumeWarmClaimSystem(context.WithoutCancel(ctx), n.orgID, n.conversationID, n.cfg.claimID)
			if errors.Is(err, db.ErrClaimReleased) {
				return s.endWarmFenced(n, result)
			}
			if err != nil || !resumed {
				if err != nil {
					delegateLog.Warn("resume the conversation on its warm claim failed; letting the claim go",
						"conversation", n.conversationID, "claim", n.cfg.claimID, "error", err)
				}
				return s.letGoWarm(context.WithoutCancel(ctx), n, result)
			}
			s.broadcastConversationUpdate(n.orgID, n.conversationID, domain.StatusRunning)
			// A close while the claim was warm told only the conversations
			// that were working, which this one was not. The loop reads the
			// note at the turn's first model call, with the person's message.
			s.tellClosedTask(context.WithoutCancel(ctx), n.orgID, n.conversationID, n.task.ID)
			// The first turn's params say how this engagement started: in a
			// sandbox launched for it, in a workspace built or restored for
			// it. Neither is true of a later turn, which runs in the sandbox and
			// tree the last one left, so it asserts neither.
			params.NewSandbox = false
			params.Workspace = domain.WorkspaceProvenanceUnknown
			params.WorkspaceAsOf = nil
			params.ExecutorChanged = false
			result = accumulateTurns(result, n.run(ctx, params))
		case warmLetGo:
			return s.letGoWarm(ctx, n, result)
		case warmShutdown:
			return s.letGoWarm(context.WithoutCancel(ctx), n, result)
		case warmStopped:
			return s.endWarmStopped(ctx, n, result)
		case warmFenced:
			return s.endWarmFenced(n, result)
		}
	}
}

// endNativeTurn is the ending a turn has without a warm claim: the jail
// stopped, then the result recorded and the claim released with it.
func (s *Spawner) endNativeTurn(ctx context.Context, n nativeTail, result agentloop.Result) engagementDisposition {
	n.release()
	return s.recordNativeResult(ctx, n.orgID, n.conversationID, n.task, n.cfg, n.namespace, n.claudeCwd, n.creatorUserID, n.startTime, result, n.mirror)
}

// accumulateTurns is the engagement's result after another turn: that turn's
// ending, carrying the turns, time and spend of every Run before it. The time
// spent waiting between them is in no Run, so it is in neither figure.
func accumulateTurns(earlier, next agentloop.Result) agentloop.Result {
	next.NumTurns += earlier.NumTurns
	next.DurationMs += earlier.DurationMs
	next.CostUSD += earlier.CostUSD
	return next
}

// warmWindowOpens reports whether a turn's end keeps the claim warm: the turn
// ended on its own, a person's message reached the model during it, its
// ending advances and finishes nothing, and the executor is not going away. A
// turn that fails any of these ends as every turn did before there were warm
// claims.
func (s *Spawner) warmWindowOpens(ctx context.Context, n nativeTail, result agentloop.Result, personDelivered bool) bool {
	if result.Kind != agentloop.ResultConcluded || result.Outcome != domain.ConversationOutcomeContinue {
		return false
	}
	if !personDelivered || s.WarmWindow() <= 0 || s.Draining() {
		return false
	}
	// A stop, a fence or a shutdown that landed as the turn ended is answered
	// by the ending it would have had without a window.
	if ctx.Err() != nil {
		return false
	}
	// The turn-end snapshot is taken with the jail still running, so it has to
	// be a frozen capture, which only a checkpointer takes. With a blob store
	// and no checkpointer there is nothing to capture, and the turn ends as it
	// always has.
	if n.ckpt == nil && s.Storage() != nil {
		return false
	}
	if n.cfg.blueprintRunID == "" {
		return true
	}
	// A running blueprint is one the reactor advances or finishes on this
	// verdict, so holding the claim would hold the next step back with it.
	br, err := s.blueprints.GetRunSystem(ctx, n.orgID, n.cfg.blueprintRunID)
	if err != nil {
		delegateLog.Warn("read the blueprint run at a turn end failed; ending the turn without a warm claim",
			"conversation", n.conversationID, "blueprint_run", n.cfg.blueprintRunID, "error", err)
		return false
	}
	return br == nil || br.Status != domain.BlueprintRunStatusRunning
}

// holdOutcome is what concludeKeepingClaim came to.
type holdOutcome int

const (
	// holdKept: the turn's state is written and the claim is warm.
	holdKept holdOutcome = iota
	// holdNotKept: nothing was written, and the turn ends as it would have
	// without a window.
	holdNotKept
	// holdFenced: the claim is no longer this engagement's.
	holdFenced
)

// concludeKeepingClaim writes the turn's conclusion as recordNativeResult
// writes it, in the same order, with the claim kept warm instead of released.
//
// The one difference is the snapshot. The jail is still running, so the tree
// is captured frozen, by a checkpoint covering the turn's newest row, and it
// comes first: a snapshot that does not land keeps nothing, and the turn ends
// through recordNativeResult after all.
func (s *Spawner) concludeKeepingClaim(ctx context.Context, n nativeTail, result agentloop.Result) holdOutcome {
	if leaseFenced(ctx) {
		return holdFenced
	}
	if !s.snapshotTurnEnd(ctx, n) {
		return holdNotKept
	}
	if fileState := n.mirror.settle(ctx); fileState != memoryFilePresent {
		delegateLog.Debug("no usable memory file at termination (no memory row written)", "conversation", n.conversationID, "state", fileState)
	}
	bgCtx := context.WithoutCancel(ctx)
	s.attachConversationMemoryEntities(bgCtx, n.orgID, n.conversationID, n.task.EntityID)
	updated, err := s.conversations.ConcludeKeepingClaimSystem(bgCtx, n.orgID, n.conversationID, n.cfg.claimID,
		result.DurationMs, result.NumTurns, result.ResultSummary, string(result.Outcome), result.OutcomeReason)
	if errors.Is(err, db.ErrClaimReleased) {
		delegateLog.Error("engagement fenced out at conclusion; a successor owns the conversation",
			"conversation", n.conversationID, "claim", n.cfg.claimID)
		return holdFenced
	}
	if err != nil {
		delegateLog.Warn("record a conclusion keeping the claim warm failed; ending the turn without a warm claim",
			"conversation", n.conversationID, "claim", n.cfg.claimID, "error", err)
		return holdNotKept
	}
	status := domain.StatusOpen
	if updated != nil {
		status = updated.Status
	}
	s.broadcastConversationUpdate(n.orgID, n.conversationID, status)
	toast.FireUser(s.wsHub, n.orgID, n.creatorUserID, toast.LevelSuccess, "", fmt.Sprintf("Run %s completed", shortConversationID(n.conversationID)))
	return holdKept
}

// snapshotTurnEnd takes the turn-end snapshot a warm claim needs: a frozen
// checkpoint of the tree as the turn left it, at the turn's newest row. With
// no blob store there is nothing to snapshot to, as at any other ending.
func (s *Spawner) snapshotTurnEnd(ctx context.Context, n nativeTail) bool {
	if n.ckpt == nil {
		return true
	}
	rows, err := s.conversations.ListForAssemblySystem(ctx, n.orgID, n.conversationID)
	if err != nil || len(rows) == 0 {
		delegateLog.Warn("read the transcript for the turn-end snapshot failed; ending the turn without a warm claim",
			"conversation", n.conversationID, "error", err)
		return false
	}
	if !n.ckpt.checkpointTurnEnd(newestAssemblyKey(rows)) {
		delegateLog.Warn("the turn-end snapshot did not cover the tree; ending the turn without a warm claim",
			"conversation", n.conversationID, "claim", n.cfg.claimID)
		return false
	}
	return true
}

// warmEnd is how a warm claim's wait ended.
type warmEnd int

const (
	// warmResumed: a person's message is waiting, and the next turn reads it.
	warmResumed warmEnd = iota
	// warmLetGo: the window expired, the registry let the claim go, or the
	// conversation ended.
	warmLetGo
	// warmShutdown: the dispatcher is shutting down.
	warmShutdown
	// warmStopped: the engagement was stopped, by a person or the watchdog.
	warmStopped
	// warmFenced: the claim is no longer this engagement's.
	warmFenced
)

// waitWarm holds the warm claim until deadline or until something ends the
// wait. It registers before its first read, so a wake sent after that read
// still reaches it, and a message or ending that landed before registration
// is seen by the read.
func (s *Spawner) waitWarm(ctx context.Context, n nativeTail, deadline time.Time) warmEnd {
	wait := s.warmWaits.Register(n.conversationID, n.orgID)
	defer wait.Unregister()
	defer s.activityFor(n.conversationID).begin(warmWaitOp, time.Until(deadline)+backstopMargin)()

	// look reads the conversation and returns how the wait ends; ok is false
	// if it continues. An ended conversation is let go even with a message
	// waiting, since no message resumes it. A waiting message resumes it,
	// unless the registry already let this engagement go, in which case the
	// queued conversation goes to the next claim.
	look := func() (end warmEnd, ok bool) {
		st := s.readWarmWait(ctx, n)
		switch {
		case st.Ended:
			return warmLetGo, true
		case !st.PersonMessageWaiting:
			return 0, false
		case wait.TakeForResume():
			return warmResumed, true
		default:
			return warmLetGo, true
		}
	}
	if end, ok := look(); ok {
		return end
	}
	window := time.NewTimer(time.Until(deadline))
	defer window.Stop()
	backstop := time.NewTicker(warmWaitBackstop)
	defer backstop.Stop()
	for {
		select {
		case <-wait.Wake():
			if end, ok := look(); ok {
				return end
			}
		case <-backstop.C:
			if end, ok := look(); ok {
				return end
			}
		case <-window.C:
			// One last look. A message whose ring is still on its way, or
			// that landed as the timer fired, is read rather than left to a
			// cold claim.
			if end, ok := look(); ok {
				return end
			}
			return warmLetGo
		case <-wait.LetGo():
			return warmLetGo
		case <-ctx.Done():
			switch {
			case leaseFenced(ctx):
				return warmFenced
			case shutdownCancelled(ctx):
				return warmShutdown
			default:
				return warmStopped
			}
		}
	}
}

// readWarmWait reads what the wait acts on: a person's message waiting, and
// the conversation having ended, which no follow-up resumes, so a warm claim
// on it serves nothing. A conversation that is gone reads as ended. A read
// that fails answers that neither holds, and the next ring or backstop tick
// asks again.
func (s *Spawner) readWarmWait(ctx context.Context, n nativeTail) db.WarmWaitState {
	st, err := s.conversations.WarmWaitStateSystem(ctx, n.orgID, n.conversationID)
	if errors.Is(err, db.ErrNoSuchConversation) {
		return db.WarmWaitState{Ended: true}
	}
	if err != nil {
		delegateLog.Warn("read the warm claim's conversation failed; asking again on the next ring or tick",
			"conversation", n.conversationID, "error", err)
		return db.WarmWaitState{}
	}
	return st
}

// warmResumeModelHolds asks the model question a claim asks (modelForClaim)
// before a warm resume, and returns the conversation it read when the answer
// is the model the engagement was built with. Otherwise it returns nil and
// the claim is let go: the resumed conversation is left queued with no claim,
// and the cold claim that drives it makes the decision as every claim does,
// switching models or refusing the one the team no longer enables. A failed
// read answers nil too, so the cold claim decides.
func (s *Spawner) warmResumeModelHolds(ctx context.Context, n nativeTail) *domain.Conversation {
	conv, err := s.conversations.GetSystem(ctx, n.orgID, n.conversationID)
	if err != nil || conv == nil {
		delegateLog.Warn("read the conversation before a warm resume failed; letting the claim go",
			"conversation", n.conversationID, "claim", n.cfg.claimID, "error", err)
		return nil
	}
	var br *domain.BlueprintRun
	if n.cfg.blueprintRunID != "" {
		if br, err = s.blueprints.GetRunSystem(ctx, n.orgID, n.cfg.blueprintRunID); err != nil {
			delegateLog.Warn("read the blueprint run before a warm resume failed; letting the claim go",
				"conversation", n.conversationID, "blueprint_run", n.cfg.blueprintRunID, "error", err)
			return nil
		}
	}
	model, err := s.modelForClaim(ctx, n.orgID, br, *conv)
	if err != nil {
		delegateLog.Info("the model check refused a warm resume; letting the claim go for a cold claim to decide",
			"conversation", n.conversationID, "claim", n.cfg.claimID, "engagement_model", n.params.Model, "error", err)
		return nil
	}
	if model != n.params.Model {
		delegateLog.Info("a claim would now run on another model than the warm engagement's; letting the claim go",
			"conversation", n.conversationID, "claim", n.cfg.claimID, "engagement_model", n.params.Model, "model", model)
		return nil
	}
	return conv
}

// letGoWarm ends an engagement keeping its claim warm: the jail stopped, then
// the claim released. The conversation is not written; it has held its
// turn-end state since the turn ended.
//
// The jail stops before the release, as it does at every other ending, so no
// claim that comes after this one finds this engagement's processes in the
// tree. A message that resumes the conversation while the jail stops resumes
// it onto a claim about to go, and the release then leaves the conversation
// for the queue.
//
// It records the engagement's terminal span, as recordNativeResult does for
// every other ending. result is the last turn's ending, carrying the turns,
// time and spend of the whole engagement.
func (s *Spawner) letGoWarm(ctx context.Context, n nativeTail, result agentloop.Result) (disp engagementDisposition) {
	_, span := s.startNativeEngagementSpan(ctx, n.orgID, n.conversationID, n.task.ID, result)
	defer func() {
		terminal := "concluded_" + string(result.Outcome)
		if disp.fenced {
			terminal = "fenced"
		}
		span.SetAttributes(telemetry.Outcome(terminal))
		span.End()
	}()
	n.release()
	n.ckpt.stop()
	s.releaseActivity(n.conversationID, n.cfg.claimID)
	released, err := s.conversations.ReleaseWarmClaimSystem(context.WithoutCancel(ctx), n.orgID, n.conversationID, n.cfg.claimID)
	if err != nil {
		delegateLog.Warn("release the warm claim failed; its lease lapses and the takeover releases it",
			"conversation", n.conversationID, "claim", n.cfg.claimID, "error", err)
		return engagementDisposition{}
	}
	if !released {
		return engagementDisposition{fenced: true}
	}
	return engagementDisposition{}
}

// endWarmStopped ends a warm claim's engagement that was stopped. A
// conversation that has its turn-end state has nothing to stop, and the claim
// is let go. One a follow-up resumed before the engagement took it up is a
// conversation mid-flight under this claim, and it takes the stopped park, as
// a stop of any turn does.
func (s *Spawner) endWarmStopped(ctx context.Context, n nativeTail, result agentloop.Result) engagementDisposition {
	conv, err := s.conversations.GetSystem(context.WithoutCancel(ctx), n.orgID, n.conversationID)
	if err == nil && conv != nil && conv.Status == domain.StatusQueued {
		return s.endNativeTurn(ctx, n, agentloop.Result{
			Kind:       agentloop.ResultCancelled,
			NumTurns:   result.NumTurns,
			DurationMs: result.DurationMs,
			CostUSD:    result.CostUSD,
		})
	}
	return s.letGoWarm(context.WithoutCancel(ctx), n, result)
}

// endWarmFenced ends an engagement whose claim went to a successor: the jail
// stopped and the checkpointer and watchdog ended, with nothing written but
// the engagement's terminal span.
func (s *Spawner) endWarmFenced(n nativeTail, result agentloop.Result) engagementDisposition {
	_, span := s.startNativeEngagementSpan(context.Background(), n.orgID, n.conversationID, n.task.ID, result)
	span.SetAttributes(telemetry.Outcome("fenced"))
	span.End()
	n.release()
	n.ckpt.stop()
	s.releaseActivity(n.conversationID, n.cfg.claimID)
	return engagementDisposition{fenced: true}
}

// isPersonMessage reports whether a transcript row is a message a person
// wrote: a user row with an author and a subtype in the human set. Every row
// TF writes itself carries a subtype outside that set, and a step's opening
// rows carry their own.
func isPersonMessage(r domain.Message) bool {
	return r.Role == "user" && r.UserID != "" && agentloop.IsHumanInput(r)
}

// newestAssemblyKey is the assembly position of the last row in rows.
func newestAssemblyKey(rows []domain.Message) float64 {
	newest := 0.0
	for i, r := range rows {
		key := float64(r.ID)
		if r.Seq != nil {
			key = *r.Seq
		}
		if i == 0 || key > newest {
			newest = key
		}
	}
	return newest
}
