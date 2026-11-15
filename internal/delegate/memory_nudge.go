package delegate

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// memoryNudgeTag opens the memory-update note and is how a later turn
// recognizes one in the transcript. It is its own tag, not the artifact
// note's, so neither nudge reads the other's note as its question answered.
const memoryNudgeTag = `<system-note kind="memory-update">`

// memoryNudgeNote is what the agent is asked when its memory file may have
// fallen behind the conversation. It asks for no reply because the run
// station shows the reply as a turn of its own, addressed to a person who
// never wrote the note.
const memoryNudgeNote = memoryNudgeTag + "\n" +
	"The conversation has moved on since your notes in _tfac/memory.md were last written. " +
	"If they no longer say what whoever comes next needs to know, rewrite them now; if they do, leave them as they are. " +
	"Then end your turn without a reply: the person did not write this note.\n" +
	"</system-note>"

// memoryNudgeAfterPersonMessages is how many of the person's messages may
// arrive after the memory file last changed before the agent is asked about
// it. It counts the person's messages and not model turns: one message can
// drive dozens of model calls, so a count of calls would pass any threshold
// within a single request and ask after nearly every turn.
const memoryNudgeAfterPersonMessages = 3

// memoryUpdateNudge builds the would-stop hook that asks a conversation a
// person works in to bring its memory file up to date: the agent is asked
// when the turn it is ending made tool calls, its memory file has never been
// written or has not changed in memoryNudgeAfterPersonMessages of the
// person's messages, and it has not been asked since the person last wrote.
//
// A conversation a person works in never completes, so the memory a boundary
// files for it is whatever the agent last wrote, and nothing else ever
// generates a fresher one. A delegation is checked for a memory when it
// completes, so it is not asked here.
//
// A turn with no tool calls changed nothing the notes could be behind on, so
// a question answered from what the agent already knew is never followed by a
// note. "The file was written" means the conversation's memory row holds the
// agent's own content: a row TF generated, or one that records nothing, is
// not notes the agent wrote.
//
// Like the artifact contract, it reads only stored state — the transcript,
// the memory row and the person's messages — so a first engagement, a resume
// and a crash's successor all answer the same, and it re-arms on human input
// and only on human input.
func (s *Spawner) memoryUpdateNudge(orgID, conversationID string, interactive bool) func(context.Context, int, string) string {
	if !interactive || s.conversations == nil || s.taskMemory == nil {
		return nil
	}
	return func(ctx context.Context, _ int, _ string) string {
		// Fail quiet on every read: a nudge sent on a read that failed asks
		// the agent to rework notes that may be current.
		rows, err := s.conversations.ListForAssemblySystem(ctx, orgID, conversationID)
		if err != nil {
			delegateLog.Warn("read transcript for the turn-end memory check failed; not nudging", "conversation", conversationID, "error", err)
			return ""
		}
		if !toolCallsSinceHumanInput(rows) || askedSinceHumanInput(rows, memoryNudgeTag) {
			return ""
		}
		mem, err := s.taskMemory.GetForConversationSystem(ctx, orgID, conversationID)
		if err != nil {
			delegateLog.Warn("read the conversation's memory for the turn-end memory check failed; not nudging", "conversation", conversationID, "error", err)
			return ""
		}
		if mem == nil || mem.Source != domain.MemorySourceAgent {
			return memoryNudgeNote
		}
		since, err := s.conversations.CountPersonMessagesSinceSystem(ctx, orgID, conversationID, mem.UpdatedAt)
		if err != nil {
			delegateLog.Warn("count the person's messages for the turn-end memory check failed; not nudging", "conversation", conversationID, "error", err)
			return ""
		}
		if since < memoryNudgeAfterPersonMessages {
			return ""
		}
		return memoryNudgeNote
	}
}

// toolCallsSinceHumanInput reports whether the agent made a tool call after
// the newest row a person wrote: whether the turn now ending did anything
// beyond answering. A transcript with no human row is read whole.
func toolCallsSinceHumanInput(rows []domain.Message) bool {
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		switch {
		case r.Role == "tool", r.Role == "assistant" && len(r.ToolCalls) > 0:
			return true
		case r.Role == "user" && agentloop.IsHumanInput(r):
			return false
		}
	}
	return false
}

// turnEndNudges is the would-stop hook both runtimes ask: the native loop as
// its ShouldStopAfterTurn hook, and the SDK driver through turnEndNudge. Each
// call sends at most one note, so the checks are asked in order and the first
// note wins. The artifact contract goes first: it asks for the run's output,
// and a memory written ahead of that output would describe work that is not
// done yet.
func (s *Spawner) turnEndNudges(orgID, conversationID string, task domain.Task, finalStep, interactive bool) func(context.Context, int, string) string {
	return firstNudge(
		s.artifactContractNudge(orgID, conversationID, task, finalStep),
		s.memoryUpdateNudge(orgID, conversationID, interactive),
	)
}

// firstNudge asks each hook in order and answers with the first note one
// returns. Nil hooks are skipped, and nil when every hook is nil, so a run no
// check applies to asks nothing at all.
func firstNudge(hooks ...func(context.Context, int, string) string) func(context.Context, int, string) string {
	var armed []func(context.Context, int, string) string
	for _, h := range hooks {
		if h != nil {
			armed = append(armed, h)
		}
	}
	if len(armed) == 0 {
		return nil
	}
	return func(ctx context.Context, turn int, finalText string) string {
		for _, h := range armed {
			if note := h(ctx, turn, finalText); note != "" {
				return note
			}
		}
		return ""
	}
}
