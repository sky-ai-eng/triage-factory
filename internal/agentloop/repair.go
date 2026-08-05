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

// workspaceRestoredBody describes a reconstructed workspace exactly:
// snapshots are taken at graceful dormancy points, so everything up to the
// last park or step boundary survived — including uncommitted and untracked
// files — and precisely the interrupted engagement's work since that point is
// absent.
const workspaceRestoredBody = "Your workspace was restored from its last snapshot " +
	"(or built fresh if none existed yet). Everything committed or written up to that snapshot is present, " +
	"including uncommitted and untracked files. Any changes made after it — during the engagement that was " +
	"interrupted — are not present. Check the working tree and git log before building on what you remember doing."

// executorChangedSentence is said only when a predecessor engagement
// demonstrably ran elsewhere. It is a separate sentence rather than part of
// the body because it is a separate fact: a workspace can be rebuilt on the
// executor that parked it (a wiped run root, a startup sweep), and asserting
// a move that did not happen is the thing this whole notice must not do.
const executorChangedSentence = "This run resumed on a different executor. "

// workspaceRestoredNotice composes the claim-time notice for an engagement
// entering a tree that is a reconstruction rather than the one its
// predecessor left behind.
func workspaceRestoredNotice(executorChanged bool) string {
	notice := "<system-note>\n"
	if executorChanged {
		notice += executorChangedSentence
	}
	return notice + workspaceRestoredBody + "\n</system-note>"
}

// repairTranscript makes the conversation's transcript legal and honest
// before this engagement reads it. It runs unconditionally on every claim
// and is idempotent: on a healthy transcript both halves are no-ops.
//
// There is deliberately no "is this a resume?" branch. A crash can land
// anywhere, including places a resume flag would not be set, so the repair
// that must be correct after any crash is the repair that always runs.
func (e *Engine) repairTranscript(ctx context.Context, params Params) error {
	rows, err := e.Transcript.ListForAssembly(ctx, params.OrgID, params.ConversationID)
	if err != nil {
		return err
	}

	if err := e.repairDanglingToolCalls(ctx, params, rows); err != nil {
		return err
	}
	return e.noticeWorkspaceRestored(ctx, params, rows)
}

// repairDanglingToolCalls answers every tool call that has no persisted
// result with a synthetic is_error row — including a partially answered
// batch, where the expected ids are diffed against the persisted ones.
//
// It NEVER re-dispatches. A "missing" result may belong to a call that
// already pushed a branch or opened a pull request; running it again would
// duplicate an external side effect that the transcript cannot see and the
// restored workspace may not record.
func (e *Engine) repairDanglingToolCalls(ctx context.Context, params Params, rows []domain.Message) error {
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
		"conversation", params.ConversationID, "count", len(missing))
	for _, call := range missing {
		if err := e.insertToolResult(ctx, params, call, interruptedToolResult, true); err != nil {
			return fmt.Errorf("insert synthetic result for %s: %w", call.ID, err)
		}
	}
	return nil
}

// noticeWorkspaceRestored queues the workspace-restored notice when this
// engagement entered a rebuilt tree and the conversation has already done work
// under an earlier claim.
//
// Both gates say the same thing from opposite ends — there is work the model
// remembers doing, and the tree it did that work in is gone. Drop either and
// the notice is false. A warm resume keeps it silent because nothing was lost:
// the interrupted engagement's changes are right there, and the only genuine
// unknown, an interrupted call's effects, is already answered in-band by the
// dangling-call repair above. The prior-work gate keeps a credential-parking
// retry or a requeue-before-start silent for the same reason — there is
// nothing the model remembers to be wrong about.
//
// Telling an agent its intact workspace was restored costs a verification
// pass it did not need, and teaches it that system notes are worth
// second-guessing. That is the more expensive of the two errors, by far.
func (e *Engine) noticeWorkspaceRestored(ctx context.Context, params Params, rows []domain.Message) error {
	if !params.Workspace.Restored() {
		return nil
	}
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
	return e.insertPending(ctx, params, workspaceRestoredNotice(params.ExecutorChanged), domain.MessageSubtypeInjectionExecutorChanged)
}

// isDelivered mirrors the schema default: nil means delivered, only an
// explicit false is pending.
func isDelivered(m domain.Message) bool {
	return m.Delivered == nil || *m.Delivered
}
