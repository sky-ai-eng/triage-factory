package agentloop

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// interruptedToolResult is the synthetic result written for a tool call
// whose real result never reached the transcript. Its exact claim matters:
// the crash window sits between executing a tool and persisting its result,
// so the call may have completed and had effects — and the restored
// workspace may not reflect them either way.
const interruptedToolResult = "interrupted: the executor changed and the workspace was restored to its last snapshot point; " +
	"this call's result is unknown and its effects may be partially present or absent — " +
	"verify state before repeating any side-effectful action."

// executorChangedNotice is the claim-time notice inserted when this
// engagement follows earlier assistant turns. It describes the workspace
// exactly: snapshots are taken at graceful dormancy points, so everything up
// to the last park or step boundary survived — including uncommitted and
// untracked files — and precisely the interrupted engagement's work since
// that point is absent.
const executorChangedNotice = "<system-note>\n" +
	"This run resumed on a different executor. Your workspace was restored from its last snapshot " +
	"(or built fresh if none existed yet). Everything committed or written up to that snapshot is present, " +
	"including uncommitted and untracked files. Any changes made after it — during the engagement that was " +
	"interrupted — are not present. Check the working tree and git log before building on what you remember doing.\n" +
	"</system-note>"

// repairTranscript makes the conversation's transcript legal and honest
// before this engagement reads it. It runs unconditionally on every claim
// and is idempotent: on a healthy transcript both halves are no-ops.
//
// There is deliberately no "is this a resume?" branch. A crash can land
// anywhere, including places a resume flag would not be set, so the repair
// that must be correct after any crash is the repair that always runs.
func (e *Engine) repairTranscript(ctx context.Context, spec Spec) error {
	rows, err := e.Transcript.ListForAssembly(ctx, spec.OrgID, spec.ConversationID)
	if err != nil {
		return err
	}

	if err := e.repairDanglingToolCalls(ctx, spec, rows); err != nil {
		return err
	}
	return e.noticeExecutorChanged(ctx, spec, rows)
}

// repairDanglingToolCalls answers every tool call that has no persisted
// result with a synthetic is_error row — including a partially answered
// batch, where the expected ids are diffed against the persisted ones.
//
// It NEVER re-dispatches. A "missing" result may belong to a call that
// already pushed a branch or opened a pull request; running it again would
// duplicate an external side effect that the transcript cannot see and the
// restored workspace may not record.
func (e *Engine) repairDanglingToolCalls(ctx context.Context, spec Spec, rows []domain.Message) error {
	answered := make(map[string]struct{})
	for _, r := range rows {
		if r.Role == "tool" && r.ToolCallID != "" {
			answered[r.ToolCallID] = struct{}{}
		}
	}

	var missing []domain.ToolCall
	for _, r := range rows {
		if r.Role != "assistant" {
			continue
		}
		for _, call := range r.ToolCalls {
			if call.ID == "" {
				continue
			}
			if _, ok := answered[call.ID]; ok {
				continue
			}
			// Flow-control calls resolve loop-side and terminate the
			// engagement, so one can only be unanswered if the process died
			// between persisting it and releasing the claim. It still needs a
			// result row — an unanswered tool_use is an illegal transcript
			// whatever the tool was.
			missing = append(missing, call)
			answered[call.ID] = struct{}{} // guard against a duplicated id in the log
		}
	}
	if len(missing) == 0 {
		return nil
	}

	e.info("repairing interrupted tool calls on claim",
		"conversation", spec.ConversationID, "count", len(missing))
	for _, call := range missing {
		if err := e.insertToolResult(ctx, spec, call, interruptedToolResult, true); err != nil {
			return fmt.Errorf("insert synthetic result for %s: %w", call.ID, err)
		}
	}
	return nil
}

// noticeExecutorChanged queues the workspace-restored notice when this
// conversation has already done work under an earlier claim.
//
// Gated on prior assistant rows existing, which is what keeps a
// credential-parking retry or a requeue-before-start silent: those re-claims
// changed nothing about the workspace the model has seen, so a notice would
// be noise the model has to reason about.
func (e *Engine) noticeExecutorChanged(ctx context.Context, spec Spec, rows []domain.Message) error {
	priorWork := false
	for _, r := range rows {
		if r.Role == "assistant" {
			priorWork = true
			break
		}
	}
	if !priorWork {
		return nil
	}
	// Idempotence: a claim that already queued the notice and then died
	// before consuming it must not queue a second one.
	for _, r := range rows {
		if r.Subtype == domain.MessageSubtypeInjectionExecutorChanged && !isDelivered(r) {
			return nil
		}
	}
	return e.insertPending(ctx, spec, executorChangedNotice, domain.MessageSubtypeInjectionExecutorChanged)
}

// isDelivered mirrors the schema default: nil means delivered, only an
// explicit false is pending.
func isDelivered(m domain.Message) bool {
	return m.Delivered == nil || *m.Delivered
}
