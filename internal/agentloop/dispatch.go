package agentloop

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// fatalToolHostNotice is the result written for calls that cannot be
// attempted because the tool host is gone. It says so plainly rather than
// implying the tool failed: the distinction is what stops a model from
// "fixing" its arguments and retrying the call against a working tool host.
// It answers the calls a dead tool host took with it when the engine could
// not restart it: the engagement ends with ResultToolHostLost, and the model
// reads this on the claim that continues the conversation in a new cell.
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
// it sets a terminal outcome, so a model that pairs `stop_blueprint` with real
// work in one message gets the work done and keeps going rather than
// silently losing the call it made alongside.
//
// position is the assembly key of the last result the batch wrote. Every call
// is answered before this returns without an error, so it is the point in the
// transcript by which everything the batch did to the workspace is done.
//
// Each result row carries how long its call ran, measured around the tool
// host's call and nothing else: time a call spends held at the gate before
// it, or in the rewrite after it, is the loop's, not the tool's. A call that
// was never dispatched carries no duration.
func (e *Engine) dispatchBatch(ctx context.Context, params Params, ownerID int, calls []domain.ToolCall, deaths *toolHostDeaths) (result Result, terminated bool, position float64, err error) {
	var terminal *Result
	allTerminate := true
	at := toolResultPositions(ownerID, len(calls))

	for i, call := range calls {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, false, 0, ctxErr
		}

		// A TF tool resolves loop-side and never enters the sandbox. Its result row still lands, so the transcript stays legal
		// and a later reader sees why the run ended.
		if out, ran, ok := e.resolveLoopSide(ctx, call, params); ok {
			key, err := e.insertToolResult(ctx, params, call, out.Content, out.IsError, at(i), &ran)
			if err != nil {
				return Result{}, false, 0, err
			}
			position = key
			if out.Terminal == "" {
				// Either a correction (a bad type, a missing argument) or a
				// loop-side tool that simply did its job. Neither ends the
				// run, so this batch is no different from one that did work.
				allTerminate = false
				continue
			}
			if terminal == nil {
				terminal = &Result{
					Kind:          ResultConcluded,
					Outcome:       out.Terminal,
					OutcomeReason: out.Reason,
					ResultSummary: out.Summary,
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
				key, err := e.insertToolResult(ctx, params, call, deny, true, at(i), nil)
				if err != nil {
					return Result{}, false, 0, err
				}
				position = key
				continue
			}
		}

		out, ran, dispatchErr := e.callTool(ctx, call)
		if e.Hooks.AfterToolCall != nil {
			out = e.Hooks.AfterToolCall(ctx, call, out)
		}

		// A transport failure with no frame to classify is treated exactly
		// as a fatal protocol error: the connection is unusable either way.
		fatal := dispatchErr != nil || (out.Protocol != nil && out.Protocol.Fatal())
		if fatal {
			// A stopped engagement kills its jail, so a stop that lands
			// during a call ends it this way too. That is the stop's to
			// report, and the next claim's repair pass answers the calls.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Result{}, false, 0, ctxErr
			}
			// The restart comes before the answers, so each answer can say
			// where the conversation goes on. The lost call's duration was
			// taken when it failed, so the restart is not in it.
			lost := toolHostLostFor(dispatchErr, out.Protocol)
			restarted := e.restartToolHost(ctx, params, call, deaths)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Result{}, false, 0, ctxErr
			}
			key, err := e.answerLostBatch(ctx, params, calls, at, i, lost, restarted, ran)
			if err != nil {
				return Result{}, false, 0, err
			}
			if !restarted {
				return Result{}, false, 0, lost
			}
			return Result{}, false, key, nil
		}
		deaths.sinceCompleted = 0

		var key float64
		var insertErr error
		switch {
		case out.Protocol != nil:
			// Survivable: an unknown tool, a malformed frame, an over-cap
			// response. The model gets the message and the loop reads the
			// next frame.
			key, insertErr = e.insertToolResult(ctx, params, call, out.Protocol.Error(), true, at(i), &ran)
		case out.ToolError != "":
			key, insertErr = e.insertToolResult(ctx, params, call, out.ToolError, true, at(i), &ran)
		case len(out.Images) > 0:
			key, insertErr = e.insertToolResultWithImages(ctx, params, call, out.Content, out.Images, at(i), &ran)
		default:
			key, insertErr = e.insertToolResult(ctx, params, call, out.Content, false, at(i), &ran)
		}
		if insertErr != nil {
			return Result{}, false, 0, insertErr
		}
		position = key
	}

	if terminal != nil && allTerminate {
		return *terminal, true, position, nil
	}
	return Result{}, false, position, nil
}

// callTool dispatches into the jail, or reports that no tool host is wired.
// ran is the milliseconds from the request going to the tool host to its
// answer or its failure.
func (e *Engine) callTool(ctx context.Context, call domain.ToolCall) (out ToolOutcome, ran int, err error) {
	if e.Tools == nil {
		return ToolOutcome{}, 0, fmt.Errorf("agentloop: no tool host wired: %w", errRequestNotSent)
	}
	_ = ctx // the socket round trip carries its own deadline; ctx cancellation is observed by the caller between calls
	defer e.beginActivity(ToolActivityOpPrefix+call.Name, e.ActivityBounds.Tool)()
	started := e.now()
	out, err = e.Tools.Call(call.Name, call.Input)
	return out, e.msSince(started), err
}

// answerLostBatch answers the call at index from, which the tool host died
// in, with what is known about it, and every call after it as never run. Each
// answer ends on where the conversation goes on: restarted says the sandbox is
// back. Without these the transcript would carry an assistant message whose
// tool calls have no results, illegal on the next assembly. position is the
// assembly key of the last answer.
//
// ran is how long the call at from ran before the tool host died. The calls
// after it never ran and carry no duration.
func (e *Engine) answerLostBatch(ctx context.Context, params Params, calls []domain.ToolCall, at func(int) *float64, from int, lost *ToolHostLostError, restarted bool, ran int) (position float64, err error) {
	next, remaining := newSandboxSentence, fatalToolHostNotice
	if restarted {
		next, remaining = restartedSentence, restartedRemainingNotice
	}
	for i := from; i < len(calls); i++ {
		content, durationMs := remaining, (*int)(nil)
		if i == from {
			content, durationMs = lost.Notice+" "+next, &ran
		}
		// The insert context may itself be cancelled if the failure was a
		// shutdown; a detached write here would race the claim release, so
		// let the error propagate and leave the repair pass to finish the job
		// on the next claim.
		if position, err = e.insertToolResult(ctx, params, calls[i], content, true, at(i), durationMs); err != nil {
			return 0, err
		}
	}
	return position, nil
}
