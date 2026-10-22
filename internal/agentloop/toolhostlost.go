package agentloop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ToolHostLostError is a tool host that died during a call: the jail it runs
// in ended, or it closed its connection. The engine answers that call with
// Notice and the calls after it as never run, then restarts the tool host
// when it can and ends the engagement with ResultToolHostLost when it cannot.
type ToolHostLostError struct {
	// Notice is what happened to the call that was running, as the model
	// reads it. The engine adds what happens next: a restarted sandbox, or a
	// new one on the next claim.
	Notice string
	// Reason says what ended the tool host, in the words a person reads if
	// the conversation stops being continued.
	Reason string
	// MemoryLimit reports that the jail's memory ceiling ended it, the one
	// cause a person can do something about by raising a limit.
	MemoryLimit bool
	// Err is the failure the call returned.
	Err error
}

func (e *ToolHostLostError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *ToolHostLostError) Unwrap() error { return e.Err }

// lostToolHost reports the explanation a dead tool host carried, if any.
func lostToolHost(err error) (*ToolHostLostError, bool) {
	var lost *ToolHostLostError
	if errors.As(err, &lost) {
		return lost, true
	}
	return nil, false
}

// RestartableToolHost is a ToolHost that can replace a jail whose tool host
// died. Restart reclaims what is left of the old jail and launches a new one
// over the same run tree, so files under the work directory carry over and
// nothing that was running does. Calls after a Restart that returned nil go
// to the new tool host.
type RestartableToolHost interface {
	ToolHost
	Restart(ctx context.Context) error
}

// maxConsecutiveDeaths bounds how many times in a row a tool host may die
// with no call completing in between. A jail that keeps dying before anything
// finishes is not being ended by one command, so the death that reaches this
// count ends the engagement, and the next claim builds a new cell for the
// conversation.
const maxConsecutiveDeaths = 3

// restartedSentence is what the calls a dead tool host took with it are told
// once the engine has restarted it. The cell keeps the run tree and /tmp for
// every jail it launches, so the files survive; processes belong to the jail
// that died.
const restartedSentence = "The sandbox has been restarted, so no process from before is running; files under /work and /tmp were kept."

// newSandboxSentence is what those calls are told when the engine could not
// restart the tool host and the engagement ends: the next engagement runs in
// a jail of its own.
const newSandboxSentence = "The run will be picked up again in a new sandbox, where no process from before is running and /tmp is empty."

// restartedRemainingNotice answers a call after the one the tool host died
// in, once the sandbox is back. The model chose it expecting the earlier call
// to have finished, so it is left for the model to send again.
const restartedRemainingNotice = "This tool call was not executed: the sandbox stopped during an earlier call in this batch. " +
	restartedSentence + " Run it again if you still need it."

// lostInFlightNotice is the Notice of a call whose request reached the tool
// host, which then died before answering. Nothing says how far the call got.
const lostInFlightNotice = "The tool host stopped while this call ran, so its result is lost. The call may have run in part " +
	"or in full: check its effects before you repeat it."

// lostBeforeSentNotice is the Notice of a call whose request never reached
// the tool host.
const lostBeforeSentNotice = "This tool call was not executed: the tool host had stopped before it was sent."

// lostSandboxReason is the Reason of a tool host whose death nothing explains.
const lostSandboxReason = "the sandbox stopped unexpectedly"

// ToolCallOverranError is what a call returns when its driver ended it by
// stopping the jail, because the call had run past limit, the longest one
// tool call may run. The engine restarts the jail as it would one that died
// on its own, and the call is told why it ended, which nothing the call
// itself returned could say.
func ToolCallOverranError(limit time.Duration, err error) *ToolHostLostError {
	phrase := limitPhrase(limit)
	return &ToolHostLostError{
		Notice: "This tool call ran past the " + phrase + " limit on a single tool call, so the sandbox was stopped to " +
			"end it. Its result is lost, and it may have run in part or in full: check its effects before you repeat it.",
		Reason: "a tool call ran past the " + phrase + " limit on a single tool call",
		Err:    err,
	}
}

// limitPhrase is a deadline as the model reads it: "30-minute", or the
// duration itself when it is not a whole number of minutes.
func limitPhrase(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d-minute", int(d/time.Minute))
	}
	return d.String()
}

// toolHostLostFor describes a call that ended its engagement's tool host: the
// transport failure dispatchErr, or, when that is nil, the fatal protocol
// frame proto. An explanation the ToolHost already attached is kept.
func toolHostLostFor(dispatchErr error, proto *ProtocolError) *ToolHostLostError {
	if dispatchErr == nil {
		return &ToolHostLostError{
			Notice: fmt.Sprintf("This tool call was not executed: the tool host refused it and closed its connection (%s).", proto.Error()),
			Reason: "the sandbox's tool host refused a request and closed its connection",
			Err:    proto,
		}
	}
	if lost, ok := lostToolHost(dispatchErr); ok {
		return lost
	}
	notice := lostInFlightNotice
	if errors.Is(dispatchErr, errRequestNotSent) {
		notice = lostBeforeSentNotice
	}
	return &ToolHostLostError{Notice: notice, Reason: lostSandboxReason, Err: dispatchErr}
}

// toolHostDeaths is one engagement's count of tool host deaths since a call
// last completed, kept in Run's frame like the rest of an engagement's state.
type toolHostDeaths struct {
	sinceCompleted int
}

// restartToolHost replaces a tool host that died during call, and reports
// whether the engagement can go on. It cannot when the tool host is not
// restartable, when this is its maxConsecutiveDeaths-th death with no call
// completing between, or when the restart fails. A restarted tool host is
// configured again, since the policy the engagement set lived in the old one.
func (e *Engine) restartToolHost(ctx context.Context, params Params, call domain.ToolCall, deaths *toolHostDeaths) bool {
	host, ok := e.Tools.(RestartableToolHost)
	if !ok {
		return false
	}
	deaths.sinceCompleted++
	if deaths.sinceCompleted >= maxConsecutiveDeaths {
		e.warn("the tool host died again with no call completing since its last restart; ending the engagement",
			"conversation", params.ConversationID, "deaths", deaths.sinceCompleted)
		return false
	}
	// The restart is part of answering the call, so the watchdog sees the
	// call in flight until the new tool host is up.
	end := e.beginActivity(ToolActivityOpPrefix+call.Name, e.ActivityBounds.Tool)
	err := host.Restart(ctx)
	end()
	if err != nil {
		e.warn("restarting the tool host failed; ending the engagement",
			"conversation", params.ConversationID, "error", err)
		return false
	}
	e.info("restarted the tool host after it died during a call",
		"conversation", params.ConversationID, "tool", call.Name, "deaths", deaths.sinceCompleted)
	e.configureToolHost(params)
	return true
}
