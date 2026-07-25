package agentloop

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// fatalToolHostNotice is the result written for calls that cannot be
// attempted because the tool host is gone. It says so plainly rather than
// implying the tool failed: the distinction is what stops a model from
// "fixing" its arguments and retrying into a dead socket.
const fatalToolHostNotice = "This tool call was not executed: the tool host became unavailable and this engagement " +
	"cannot dispatch further calls. The run will be picked up again; do not treat this as a failure of the call itself."

// dispatchBatch runs one assistant message's tool calls, serially and in
// call order, inserting each result as it returns.
//
// Serial is a correctness property, not a simplification: a tool's effects
// are visible to the next tool through the shared worktree, and the wire
// protocol allows exactly one in-flight request per engagement.
//
// terminated reports that a flow-control call ended the run. It follows the
// all-results-terminate contract: the batch terminates iff EVERY result in
// it sets a terminal outcome, so a model that pairs `stop_run` with real
// work in one message gets the work done and keeps going rather than
// silently losing the call it made alongside.
func (e *Engine) dispatchBatch(ctx context.Context, spec Spec, calls []domain.ToolCall) (result Result, terminated bool, err error) {
	var terminal *Result
	allTerminate := true

	for _, call := range calls {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, false, ctxErr
		}

		// Flow control resolves loop-side and never enters the sandbox. Its
		// result row still lands, so the transcript stays legal and a later
		// reader sees why the run ended.
		if outcome, reason, ok := flowControlOutcome(call); ok {
			ack := flowControlAck(outcome, reason)
			if err := e.insertToolResult(ctx, spec, call, ack.text, ack.isError); err != nil {
				return Result{}, false, err
			}
			if ack.isError {
				// A malformed flow-control call (a bad type, a missing reason)
				// is not a termination — the model is told what it owes and
				// continues.
				allTerminate = false
				continue
			}
			if terminal == nil {
				terminal = &Result{
					Disposition:   DispositionConcluded,
					Outcome:       outcome,
					OutcomeReason: reason,
				}
			}
			continue
		}

		allTerminate = false

		// The gate. A denial becomes an is_error result in-band: the model
		// sees why it was refused and can choose differently, which is
		// strictly more useful than killing the run.
		if e.Hooks.BeforeToolCall != nil {
			if deny := e.Hooks.BeforeToolCall(ctx, call); deny != "" {
				if err := e.insertToolResult(ctx, spec, call, deny, true); err != nil {
					return Result{}, false, err
				}
				continue
			}
		}

		out, dispatchErr := e.callTool(ctx, call)
		if e.Hooks.AfterToolCall != nil {
			out = e.Hooks.AfterToolCall(ctx, call, out)
		}

		// A transport failure with no frame to classify is treated exactly
		// as a fatal protocol error: the connection is unusable either way.
		fatal := dispatchErr != nil || (out.Protocol != nil && out.Protocol.Fatal())
		if fatal {
			cause := dispatchErr
			if cause == nil {
				cause = out.Protocol
			}
			if ferr := e.failRemainingCalls(ctx, spec, calls, call.ID); ferr != nil {
				return Result{}, false, ferr
			}
			return Result{}, false, fmt.Errorf("tool host is unusable: %w", cause)
		}

		switch {
		case out.Protocol != nil:
			// Survivable: an unknown tool, a malformed frame, an over-cap
			// response. The model gets the message and the loop reads the
			// next frame.
			if err := e.insertToolResult(ctx, spec, call, out.Protocol.Error(), true); err != nil {
				return Result{}, false, err
			}
		case out.ToolError != "":
			if err := e.insertToolResult(ctx, spec, call, out.ToolError, true); err != nil {
				return Result{}, false, err
			}
		case len(out.Images) > 0:
			if err := e.insertToolResultWithImages(ctx, spec, call, out.Content, out.Images); err != nil {
				return Result{}, false, err
			}
		default:
			if err := e.insertToolResult(ctx, spec, call, out.Content, false); err != nil {
				return Result{}, false, err
			}
		}
	}

	if terminal != nil && allTerminate {
		return *terminal, true, nil
	}
	return Result{}, false, nil
}

// callTool dispatches into the jail, or reports that no tool host is wired.
func (e *Engine) callTool(ctx context.Context, call domain.ToolCall) (ToolOutcome, error) {
	if e.Tools == nil {
		return ToolOutcome{}, fmt.Errorf("agentloop: no tool host wired")
	}
	_ = ctx // the socket round trip carries its own deadline; ctx cancellation is observed by the caller between calls
	return e.Tools.Call(call.Name, call.Input)
}

// failRemainingCalls answers every call from failedID onward with the
// tool-host-unavailable notice. Without this the transcript would carry an
// assistant message whose tool calls have no results — illegal on the next
// assembly, and something the repair pass would then have to invent an
// explanation for.
func (e *Engine) failRemainingCalls(ctx context.Context, spec Spec, calls []domain.ToolCall, failedID string) error {
	reached := false
	for _, call := range calls {
		if call.ID == failedID {
			reached = true
		}
		if !reached {
			continue
		}
		// The insert context may itself be cancelled if the failure was a
		// shutdown; a detached write here would race the claim release, so
		// let the error propagate and leave the repair pass to finish the job
		// on the next claim.
		if err := e.insertToolResult(ctx, spec, call, fatalToolHostNotice, true); err != nil {
			return err
		}
	}
	return nil
}

// flowControlAck is the tool-result text a flow-control call gets back.
type flowControlAckResult struct {
	text    string
	isError bool
}

// flowControlAck acknowledges a stop_run call, or tells the model what its
// call was missing. Both arguments are enforced here rather than by the
// schema alone because a schema is advisory: a model can and does emit a
// call without one, and an unenforced stop with no reason would land a
// terminal state carrying no explanation for the human who inherits it.
//
// The correction for a bad type restates the whole contract, since a model
// that reached for a type that does not exist has usually reached for
// `continue` — and the answer to that is not another tool.
func flowControlAck(outcome domain.RunOutcome, reason string) flowControlAckResult {
	if outcome != domain.RunOutcomeFinish && outcome != domain.RunOutcomeAbort {
		return flowControlAckResult{
			text: "stop_run needs a type of \"finish\" or \"abort\". If you meant to finish your part normally " +
				"and hand off whatever comes next, do not call this at all — reply with a message and no tool calls.",
			isError: true,
		}
	}
	if reason == "" {
		return flowControlAckResult{
			text: "stop_run requires a reason. Call it again with a plain statement of why you are stopping here " +
				"and what a human needs to know or do next.",
			isError: true,
		}
	}
	if outcome == domain.RunOutcomeAbort {
		return flowControlAckResult{text: "Stopping this run. Recorded: " + reason}
	}
	return flowControlAckResult{text: "Ending the task here. Recorded: " + reason}
}
