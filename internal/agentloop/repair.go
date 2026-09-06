package agentloop

import (
	"context"
	"fmt"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// interruptedToolResult composes the synthetic result written for a tool call
// whose real result never reached the transcript. Its exact claim matters:
// the window sits between executing a tool and persisting its result, so the
// call may have completed and had effects — and the tree the agent now reads
// may not reflect them either way.
//
// Built from the same two facts as the rebuilt-workspace notice, and held to
// the same rule: it says what happened to the tree and nothing more. The
// common shape is a stop on the executor that parked, resumed onto the warm
// tree — no restore, no move — and a result that asserted either would be
// telling the agent to distrust a workspace that is exactly as its last call
// left it.
func interruptedToolResult(prov domain.WorkspaceProvenance, executorChanged bool) string {
	s := "interrupted: the engagement running this call ended before its result was recorded"
	if executorChanged {
		s += " and this one runs on a different executor"
	}
	switch prov {
	case domain.WorkspaceProvenanceRehydrated:
		s += "; the workspace was restored to its last snapshot point"
	case domain.WorkspaceProvenanceFresh:
		s += "; the workspace was rebuilt from scratch"
	}
	return s + "; this call's result is unknown and its effects may be partially present or absent — " +
		"verify state before repeating any side-effectful action."
}

// snapshotRestoredBody describes a workspace rebuilt from its snapshot
// exactly: snapshots are taken at graceful dormancy points, so everything up
// to the last park or step boundary survived — including uncommitted and
// untracked files — and precisely the interrupted engagement's work since that
// point is absent.
const snapshotRestoredBody = "Your workspace was restored from its last snapshot. " +
	"Everything committed or written up to that snapshot is present, " +
	"including uncommitted and untracked files. Any changes made after it — during the engagement that was " +
	"interrupted — are not present. Check the working tree and git log before building on what you remember doing."

// builtFreshBody is the other rebuild, and it is a strictly larger loss: no
// snapshot was involved, so the tree holds what the remote holds and nothing
// else. Saying "restored from its last snapshot" here would promise
// uncommitted and untracked files that were never captured, which is the same
// class of falsehood as claiming a restore that never happened.
const builtFreshBody = "Your workspace was built from scratch — there was no snapshot to restore from. " +
	"Only what reached the remote is present. Anything the interrupted engagement did not push — local commits, " +
	"uncommitted edits, untracked files, scratch under the run root — is gone. " +
	"What you lost is the workspace, not this conversation: everything above is the record of what was done, " +
	"and it is why the tree may not match it. " +
	"Check the working tree and git log before building on what you remember doing."

// executorChangedSentence is said only when a predecessor engagement
// demonstrably ran elsewhere. It is a separate sentence rather than part of
// either body because it is a separate fact: a workspace can be rebuilt on the
// executor that parked it (a wiped run root, a startup sweep), and asserting
// a move that did not happen is the thing this whole notice must not do.
const executorChangedSentence = "This run resumed on a different executor. "

// workspaceRebuiltNotice composes the claim-time notice for an engagement
// entering a tree built where its predecessor's workspace used to be, saying
// which rebuild it was — the two lose different things, and a notice that
// blurred them would overstate what survived exactly when the agent has least
// to work from.
func workspaceRebuiltNotice(prov domain.WorkspaceProvenance, executorChanged bool) string {
	notice := "<system-note>\n"
	if executorChanged {
		notice += executorChangedSentence
	}
	body := builtFreshBody
	if prov == domain.WorkspaceProvenanceRehydrated {
		body = snapshotRestoredBody
	}
	return notice + body + "\n</system-note>"
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
	return e.noticeWorkspaceRebuilt(ctx, params, rows)
}

// repairDanglingToolCalls answers every tool call that has no persisted
// result with a synthetic is_error row — including a partially answered
// batch, where the expected ids are diffed against the persisted ones.
//
// It NEVER re-dispatches. A "missing" result may belong to a call that
// already pushed a branch or opened a pull request; running it again would
// duplicate an external side effect that the transcript cannot see and the
// restored workspace may not record.
//
// Each synthetic result is placed at the assembly position its call's answer
// belongs at, not at the transcript's tail. The two coincide after a bare
// crash, but rows can legally arrive between a tool call and the repair that
// closes it — a queued follow-up, the note a stop writes — and a tail append
// would then assemble the answer BEHIND them. The provider requires the
// result in the message immediately after the call and rejects anything else
// deterministically, so the whole conversation would fail on its first call
// of every subsequent claim.
func (e *Engine) repairDanglingToolCalls(ctx context.Context, params Params, rows []domain.Message) error {
	// Placement is read off adjacency, so the order this walks in is
	// load-bearing. The store already returns assembly order; sorting here
	// keeps the repair correct on its own terms, the same way the drain
	// re-sorts the rows whose flush order it decides.
	ordered := make([]domain.Message, len(rows))
	copy(ordered, rows)
	sort.SliceStable(ordered, func(i, j int) bool { return assemblyKey(ordered[i]) < assemblyKey(ordered[j]) })

	answered := make(map[string]struct{})
	for _, r := range ordered {
		if r.Role == "tool" && r.ToolCallID != "" {
			answered[r.ToolCallID] = struct{}{}
		}
	}

	var repairs []toolResultPlacement
	for i, r := range ordered {
		if r.Role != "assistant" {
			continue
		}
		var missing []domain.ToolCall
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
		// Every interrupted assistant turn anchors to its own call, so a
		// transcript carrying several of them (crash, resume, crash again)
		// repairs each in place instead of stacking every answer at one point.
		repairs = append(repairs, placeAfterAnswers(ordered, i, missing)...)
	}
	if len(repairs) == 0 {
		return nil
	}

	e.info("repairing interrupted tool calls on claim",
		"conversation", params.ConversationID, "count", len(repairs))
	for _, rep := range repairs {
		if err := e.insertToolResult(ctx, params, rep.call, interruptedToolResult(params.Workspace, params.ExecutorChanged), true, rep.seq); err != nil {
			return fmt.Errorf("insert synthetic result for %s: %w", rep.call.ID, err)
		}
	}
	return nil
}

// toolResultPlacement is one synthetic result and where it assembles: a
// fractional seq, or nil for an ordinary tail append.
type toolResultPlacement struct {
	call domain.ToolCall
	seq  *float64
}

// placeAfterAnswers positions one assistant row's synthetic results directly
// behind the answers its batch already has.
//
// The anchor is the end of the run of tool rows following the owner, not the
// owner itself: a partially answered batch keeps its real results first, and
// the synthetics fill in after them in call order. Only the synthetics are
// positioned — the user rows that arrived in the meantime keep their own
// places, because their order is the record of what the user actually said
// and when.
//
// A nil seq (the owner's answer block is the transcript's tail) is an
// ordinary append. That is the plain crash: the anchor and the tail are the
// same row, so the placement adds nothing and the column stays NULL rather
// than carrying a fraction no ordering needs.
//
// The fractions divide effective positions rather than ids, so a repair
// landing among rows a compaction already re-seqed works on the same terms as
// one landing among freshly appended ones. There is always room to divide:
// every seq any writer mints — the compaction commit's, these — is strictly
// between two ids, so two adjacent rows never share a position.
func placeAfterAnswers(rows []domain.Message, owner int, missing []domain.ToolCall) []toolResultPlacement {
	if len(missing) == 0 {
		return nil
	}
	anchor := owner
	for j := owner + 1; j < len(rows) && rows[j].Role == "tool"; j++ {
		anchor = j
	}

	out := make([]toolResultPlacement, 0, len(missing))
	if anchor == len(rows)-1 {
		for _, call := range missing {
			out = append(out, toolResultPlacement{call: call})
		}
		return out
	}

	lo, hi := assemblyKey(rows[anchor]), assemblyKey(rows[anchor+1])
	for i, call := range missing {
		seq := lo + (hi-lo)*float64(i+1)/float64(len(missing)+1)
		out = append(out, toolResultPlacement{call: call, seq: &seq})
	}
	return out
}

// noticeWorkspaceRebuilt queues the workspace notice when this engagement
// entered a rebuilt tree and the conversation has already done work under an
// earlier claim.
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
func (e *Engine) noticeWorkspaceRebuilt(ctx context.Context, params Params, rows []domain.Message) error {
	if !params.Workspace.Rebuilt() {
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
	return e.insertPending(ctx, params, workspaceRebuiltNotice(params.Workspace, params.ExecutorChanged), domain.MessageSubtypeInjectionExecutorChanged)
}

// isDelivered mirrors the schema default: nil means delivered, only an
// explicit false is pending.
func isDelivered(m domain.Message) bool {
	return m.Delivered == nil || *m.Delivered
}
