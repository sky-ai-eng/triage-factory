package delegate

import (
	"context"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// memoryNudgeTag opens the memory-update notes and is how a later turn
// recognizes one in the transcript. It is its own tag, not the artifact
// note's, so neither nudge reads the other's note as its question answered.
const memoryNudgeTag = `<system-note kind="memory-update">`

// memoryNudgeNote and memoryUnwrittenNote are what the agent is asked when its
// notes may have fallen behind the conversation, and when this conversation
// has not written any. They share the tag and differ in what each can say
// truthfully: in a conversation that never wrote its notes, a file at the
// path is earlier work's, and calling it the agent's own invites the agent to
// leave it as it is. Both ask for no reply: the person did not write the
// note, and a reply would be addressed to them.
const (
	memoryNudgeNote = memoryNudgeTag + "\n" +
		"The conversation has moved on since your notes in _tfac/memory.md were last written. " +
		"If they no longer say what whoever comes next needs to know, rewrite them now; if they do, leave them as they are. " +
		"Then end your turn without a reply: the person did not write this note.\n" +
		"</system-note>"
	memoryUnwrittenNote = memoryNudgeTag + "\n" +
		"You have not written your notes for this conversation to _tfac/memory.md yet. " +
		"Write them now, for whoever comes next on this task. If the file already holds notes, they are from earlier work: rewrite it rather than leaving it as it is. " +
		"Then end your turn without a reply: the person did not write this note.\n" +
		"</system-note>"
)

// memoryNudgeAfterPersonMessages is how many of the person's messages may
// arrive after the memory file last changed, or after the agent was last asked
// about it, before the agent is asked again. It counts the person's messages
// and not model turns: one message can drive dozens of model calls, so a count
// of calls would pass any threshold within a single request and ask after
// nearly every turn.
const memoryNudgeAfterPersonMessages = 5

// memoryUpdateNudge builds the would-stop hook that asks a conversation a
// person works in to bring its memory file up to date. The agent is asked when
// the turn it is ending made tool calls and either its notes have never been
// written and it has not been asked yet, or memoryNudgeAfterPersonMessages of
// the person's messages have arrived since the later of the notes' last change
// and the last time it was asked.
//
// A conversation a person works in never completes, so the memory a boundary
// files for it is whatever the agent last wrote, and nothing else ever
// generates a fresher one. A delegation is checked for a memory when it
// completes, so it is not asked here.
//
// The window restarts at an ask as well as at a write because the note offers
// the agent the choice of leaving current notes as they are. Counting from the
// write alone would ask again after every later message once the agent had
// taken it. A turn with no tool calls changed nothing the notes could be
// behind on, so a question answered from what the agent already knew is never
// followed by a note. "Written" means the conversation's memory row holds the
// agent's own content: a row TF generated, or one that records nothing, is not
// notes the agent wrote.
//
// Like the artifact contract, it reads only stored state — the memory row, the
// person's messages and the transcript — so a first engagement, a resume and a
// crash's successor all answer the same.
func (s *Spawner) memoryUpdateNudge(orgID, conversationID string, interactive bool) func(context.Context, int, string) string {
	if !interactive || s.conversations == nil || s.taskMemory == nil {
		return nil
	}
	return func(ctx context.Context, _ int, _ string) string {
		// Fail quiet on every read: a nudge sent on a read that failed asks
		// the agent to rework notes that may be current.
		mem, err := s.taskMemory.GetForConversationSystem(ctx, orgID, conversationID)
		if err != nil {
			delegateLog.Warn("read the conversation's memory for the turn-end memory check failed; not nudging", "conversation", conversationID, "error", err)
			return ""
		}
		written := mem != nil && mem.Source == domain.MemorySourceAgent
		// The window can only start later than the notes' last change, so a
		// count under the threshold from there settles it without reading the
		// transcript, which is the costly read and the common answer.
		if written && !s.memoryNoteDue(ctx, orgID, conversationID, mem.UpdatedAt) {
			return ""
		}
		rows, err := s.conversations.ListForAssemblySystem(ctx, orgID, conversationID)
		if err != nil {
			delegateLog.Warn("read transcript for the turn-end memory check failed; not nudging", "conversation", conversationID, "error", err)
			return ""
		}
		if !toolCallsSinceHumanInput(rows) || askedSinceHumanInput(rows, memoryNudgeTag) {
			return ""
		}
		if asked, ok := lastAskedAt(rows, memoryNudgeTag); ok && (!written || asked.After(mem.UpdatedAt)) &&
			!s.memoryNoteDue(ctx, orgID, conversationID, asked) {
			return ""
		}
		if !written {
			return memoryUnwrittenNote
		}
		return memoryNudgeNote
	}
}

// memoryNoteDue reports whether memoryNudgeAfterPersonMessages of the person's
// messages have arrived since since. A count that cannot be read is not due.
func (s *Spawner) memoryNoteDue(ctx context.Context, orgID, conversationID string, since time.Time) bool {
	n, err := s.conversations.CountPersonMessagesSinceSystem(ctx, orgID, conversationID, since)
	if err != nil {
		delegateLog.Warn("count the person's messages for the turn-end memory check failed; not nudging", "conversation", conversationID, "error", err)
		return false
	}
	return n >= memoryNudgeAfterPersonMessages
}

// lastAskedAt is when the newest note the nudge tagged tag sent was written.
// ok is false when the transcript holds none.
func lastAskedAt(rows []domain.Message, tag string) (time.Time, bool) {
	for i := len(rows) - 1; i >= 0; i-- {
		if isNudgeNote(rows[i], tag) {
			return rows[i].CreatedAt, true
		}
	}
	return time.Time{}, false
}

// toolCallsSinceHumanInput reports whether the agent made a tool call after
// the newest message a person wrote: whether the turn now ending did anything
// beyond answering. A transcript with no person message is read whole.
func toolCallsSinceHumanInput(rows []domain.Message) bool {
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		switch {
		case r.Role == "tool", r.Role == "assistant" && len(r.ToolCalls) > 0:
			return true
		case isPersonMessage(r):
			return false
		}
	}
	return false
}

// turnAnswer is the summary a turn of a conversation a person works in
// records: summary, the turn's own last text, unless the turn ended by
// answering the memory note. Then it is the newest text the agent wrote
// before the note, the answer the person got. The note asks for no reply, so
// what follows it is not an account of the turn. A transcript that cannot be
// read leaves summary as it is.
func (s *Spawner) turnAnswer(ctx context.Context, orgID, conversationID, summary string) string {
	rows, err := s.conversations.ListForAssemblySystem(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("read transcript for the turn's summary failed; it records the turn's last text", "conversation", conversationID, "error", err)
		return summary
	}
	if answer, ok := answerBeforeMemoryNote(rows); ok {
		return answer
	}
	return summary
}

// answerBeforeMemoryNote returns the newest assistant text written before the
// memory note, when that note came after the person's newest message. ok is
// false when the turn was not asked about its memory, or wrote no text before
// it was.
func answerBeforeMemoryNote(rows []domain.Message) (string, bool) {
	noted := false
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		switch {
		case isPersonMessage(r):
			return "", false
		case isNudgeNote(r, memoryNudgeTag):
			noted = true
		case noted && r.Role == "assistant" && strings.TrimSpace(r.Content) != "":
			return r.Content, true
		}
	}
	return "", false
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
