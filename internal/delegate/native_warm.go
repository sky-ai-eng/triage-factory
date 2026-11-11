// A native engagement from agent-live to its ending: the turns it runs, and
// the warm waits between them. After a turn a person took part in, the
// engagement keeps its claim, its cell and its jail for the warm window, in
// claim phase awaiting_message, so the person's next message starts the next
// turn on the same engagement instead of on a new claim that rebuilds all of
// it. The ended turn's outcome is written only when the engagement lets go,
// exactly as it would have been at the turn end.
//
// The wait sits here, around engine.Run, rather than inside the loop. Re-entering
// Run is already how every claim drives a transcript, a re-entry after a long
// gap takes the loop's cold compaction on the way in, and the phase, the
// watchdog operation and the registry are this package's.

package delegate

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// warmWaitOp is the watchdog operation a warm wait is reported under. It does
// not start with agentloop.ToolActivityOpPrefix, so the tool-overrun hook
// leaves it alone, and its bound is the window's, so the idle limit does not
// apply while it runs.
const warmWaitOp = "warm_wait"

// warmWaitBackstop is how often a waiting engagement looks for a person's
// message without having been rung. The doorbell is a NOTIFY, and a dropped
// one costs at most this long.
const warmWaitBackstop = 15 * time.Second

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
	// turn runs under.
	run        func(context.Context, agentloop.Params) agentloop.Result
	params     agentloop.Params
	transcript *nativeTranscript
	// ckpt is the engagement's checkpointer, nil when it takes none.
	ckpt *checkpointer
	// release stops the jail and closes the tool host. Every ending reads the
	// tree, so it runs before any of them.
	release func()
}

// runNativeTail runs the engagement's turns, with a warm wait after each one
// that opens a window, then lets go with the last turn's result.
func (s *Spawner) runNativeTail(ctx context.Context, n nativeTail) engagementDisposition {
	result, endCtx, fenced := s.runNativeTurns(ctx, n)
	n.release()
	if fenced {
		// The checkpointer stops before the engagement says it is done, as it
		// does on every other ending, and a fenced engagement holds no claim
		// for its watchdog to stop.
		n.ckpt.stop()
		s.releaseActivity(n.conversationID, n.cfg.claimID)
		return engagementDisposition{fenced: true}
	}
	return s.recordNativeResult(endCtx, n.orgID, n.conversationID, n.task, n.cfg, n.namespace, n.claudeCwd, n.creatorUserID, n.startTime, result, n.mirror)
}

// runNativeTurns runs turns until one ends without a window, or a window ends
// without a message. It returns the result to record and the context to record
// it under, or fenced when the claim is no longer this engagement's and
// nothing may be recorded.
func (s *Spawner) runNativeTurns(ctx context.Context, n nativeTail) (result agentloop.Result, endCtx context.Context, fenced bool) {
	params := n.params
	result = n.run(ctx, params)
	for {
		// The window is measured from the turn end, so a long turn a message
		// started still gets all of it, and the checkpoint below comes out of
		// it.
		deadline := time.Now().Add(s.WarmWindow())
		if !s.warmWindowOpens(ctx, n, result, n.transcript.takePersonDelivered()) || !s.checkpointBeforeWaiting(ctx, n) {
			return result, ctx, false
		}
		if !time.Now().Before(deadline) {
			return result, ctx, false
		}
		switch s.waitWarm(ctx, n, deadline) {
		case warmWoken:
			if s.updatePhase(ctx, n.orgID, n.conversationID, n.cfg.claimID, "") {
				return result, ctx, true
			}
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
			return result, ctx, false
		case warmShutdown:
			// The executor is going away, and the ended turn's outcome is still
			// the one to record. A shutdown hand-back would leave the
			// conversation claimable with nothing for a successor to answer.
			return result, context.WithoutCancel(ctx), false
		case warmCancelled:
			return agentloop.Result{
				Kind:       agentloop.ResultCancelled,
				NumTurns:   result.NumTurns,
				DurationMs: result.DurationMs,
				CostUSD:    result.CostUSD,
			}, ctx, false
		case warmFenced:
			return result, ctx, true
		}
	}
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

// warmWindowOpens reports whether a turn's end opens a warm window, short of
// the checkpoint that has to precede it: the turn ended on its own, a person's
// message reached the model during it, the ending advances and finishes
// nothing, and the executor is not going away.
//
// A turn that fails any of these lets go at once, exactly as every turn end
// did before there were windows.
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
	if n.cfg.blueprintRunID == "" {
		return true
	}
	// A running blueprint is one the reactor advances or finishes on this
	// verdict, so holding it back would hold the next step back with it.
	br, err := s.blueprints.GetRunSystem(ctx, n.orgID, n.cfg.blueprintRunID)
	if err != nil {
		delegateLog.Warn("read the blueprint run at a turn end failed; letting go rather than waiting warm",
			"conversation", n.conversationID, "blueprint_run", n.cfg.blueprintRunID, "error", err)
		return false
	}
	return br == nil || br.Status != domain.BlueprintRunStatusRunning
}

// checkpointBeforeWaiting writes the turn-end checkpoint a window needs, and
// reports whether the window may open. A waiting engagement's tree is on this
// executor alone, so a crash during the wait resumes the conversation from the
// blob, and a window opens only once the blob covers the turn.
//
// An engagement with no checkpointer, because checkpoints are off or there is
// no blob store, opens its window without one: the exposure a running turn
// already has under that setting.
func (s *Spawner) checkpointBeforeWaiting(ctx context.Context, n nativeTail) bool {
	if n.ckpt == nil {
		return true
	}
	rows, err := s.conversations.ListForAssemblySystem(ctx, n.orgID, n.conversationID)
	if err != nil || len(rows) == 0 {
		delegateLog.Warn("read the transcript for the turn-end checkpoint failed; letting go rather than waiting warm",
			"conversation", n.conversationID, "error", err)
		return false
	}
	if !n.ckpt.checkpointTurnEnd(newestAssemblyKey(rows)) {
		delegateLog.Warn("the turn-end checkpoint did not cover the tree; letting go rather than waiting warm",
			"conversation", n.conversationID, "claim", n.cfg.claimID)
		return false
	}
	return true
}

// warmEnd is how a warm wait ended.
type warmEnd int

const (
	// warmWoken: a person's message is waiting, and the next turn reads it.
	warmWoken warmEnd = iota
	// warmLetGo: the window expired, or the registry let the engagement go.
	warmLetGo
	// warmShutdown: the dispatcher is shutting down.
	warmShutdown
	// warmCancelled: the engagement was stopped, by a person or the watchdog.
	warmCancelled
	// warmFenced: the claim is no longer this engagement's.
	warmFenced
)

// waitWarm holds the engagement until deadline, or until something ends the
// wait first.
//
// The entry order is what keeps a message from being missed. The engagement
// registers before anything can see its phase, so a doorbell rung by a sender
// that saw awaiting_message reaches it; and it looks for a waiting message
// once it is registered, so a message whose doorbell rang before then is
// read anyway.
func (s *Spawner) waitWarm(ctx context.Context, n nativeTail, deadline time.Time) warmEnd {
	wait := s.warmWaits.Register(n.conversationID, n.orgID)
	defer wait.Unregister()
	// The write broadcasts the phase it sets, which is the status the views
	// show while the engagement waits.
	if s.updatePhase(ctx, n.orgID, n.conversationID, n.cfg.claimID, domain.ClaimPhaseAwaitingMessage) {
		return warmFenced
	}
	defer s.activityFor(n.conversationID).begin(warmWaitOp, time.Until(deadline)+backstopMargin)()

	if s.personMessagePending(ctx, n.orgID, n.conversationID) {
		return warmWoken
	}
	window := time.NewTimer(time.Until(deadline))
	defer window.Stop()
	backstop := time.NewTicker(warmWaitBackstop)
	defer backstop.Stop()
	for {
		select {
		case <-wait.Wake():
			if s.personMessagePending(ctx, n.orgID, n.conversationID) {
				return warmWoken
			}
		case <-backstop.C:
			if s.personMessagePending(ctx, n.orgID, n.conversationID) {
				return warmWoken
			}
		case <-window.C:
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
				return warmCancelled
			}
		}
	}
}

// personMessagePending reports whether a person's message is waiting on the
// conversation. Only a person's message ends a wait: an injection or a staged
// note stays queued and reaches the model with the person's next turn, as it
// does for a parked conversation. A read that fails answers no, and the next
// ring or backstop tick asks again.
func (s *Spawner) personMessagePending(ctx context.Context, orgID, conversationID string) bool {
	rows, err := s.conversations.ListForAssemblySystem(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("read the transcript for a waiting message failed; asking again on the next ring",
			"conversation", conversationID, "error", err)
		return false
	}
	for _, r := range rows {
		if r.Delivered != nil && !*r.Delivered && isPersonMessage(r) {
			return true
		}
	}
	return false
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
