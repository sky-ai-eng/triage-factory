// The rows a delegated conversation opens with — the task's prior memories and
// the task context — and the budget that decides how many memories fit.

package delegate

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The injection budget: how much of a task's recorded history rides the
// opening turn rather than staying a file the agent may read on request.
//
// Count first, then bytes, and a memory is taken whole or not at all. Five,
// because the memory contract is cumulative — each memory is written with the
// earlier ones in context, so the newest already carries the running "what is
// left" — and because the longest shipped blueprint puts two prior steps in
// front of its last one; a task past five boundaries is one whose older
// attempts are background to read on request, not handoff. 32 KiB, because the
// guidance asks for 150–300 words (about 2 KB), so five nominal memories are
// ~10 KB and this is three times that: room for a memory that genuinely
// warrants expansion, a bound on a log dump. Nothing else bounds one — the
// memory file is read with no size limit.
const (
	maxInjectedMemories    = 5
	maxInjectedMemoryBytes = 32 << 10
)

// insertRow is the door mintOpeningRows writes through, supplied by the
// runtime that owns it: native's is the claim-fenced transcript insert, which
// also broadcasts. Passed in rather than resolved here because the two
// runtimes' doors differ in who they authenticate as, and that is the only
// thing about the opening that differs between them.
type insertRow func(context.Context, *domain.Message) error

// mintOpeningRows writes the rows a delegated conversation opens with — one
// per injected prior memory, oldest first, then the task context — and returns
// them in that order, or nil when the conversation already carries an opening.
//
// Runtime-neutral by construction: it takes whether the conversation has been
// opened, the transcript it must not re-read, the memories the launch already
// loaded, and the door to write through, so both engines mint the same rows in
// the same order and a surface reading the transcript cannot tell which
// produced it.
//
// The rows are DELIVERED rather than pending, which is deliberate on both
// runtimes. The native engine assembles delivered rows on its first call, so
// nothing needs to drain them; the drain would not touch them anyway, since it
// stamps steer only onto human rows mid-turn. And on the SDK an undelivered
// user row is the resume queue — writing the opening there would have a
// re-claim replay it as new input.
//
// The gate is opened, the caller's OpeningTaskContextSystem answer, rather
// than anything read off rows: rows are the assembly window, and a compaction
// takes the opening out of it without un-opening the conversation. The case the
// gate does not cover is a crash between the memory rows and the task-context
// row, which the next claim re-mints over — one duplicated memory in a
// transcript, against a per-row gate that would have to read metadata on every
// claim to prevent it.
//
// Those same rows are why the opening is SEQUENCED rather than just appended.
// A row's assembly position is COALESCE(seq, id) and these are inserted last,
// so on a transcript that already holds anything the model would read the
// follow-up before the task context explaining what the task is. Stamping the
// opening below the earliest key that is already there puts it first, which is
// what "the conversation opens with its context" has to mean to be worth
// anything. On the common empty transcript nothing is stamped and arrival
// order is the position, as it is for every other row.
//
// The memory rows carry the memory id and nothing else. Every other fact about
// a memory — its conversation, its entity, its blueprint run, the prompt that
// produced it — is one foreign key away from that id, and a copy here would be
// a second spelling of a column that can disagree with it.
func mintOpeningRows(
	ctx context.Context,
	insert insertRow,
	opened bool,
	rows []domain.Message,
	memories []domain.TaskMemory,
	conversationID, userID, taskContext string,
) ([]domain.Message, error) {
	if opened {
		return nil, nil
	}

	opening := make([]domain.Message, 0, maxInjectedMemories+1)
	for _, m := range selectInjectedMemories(memories) {
		delivered := true
		opening = append(opening, domain.Message{
			ConversationID: conversationID,
			UserID:         userID,
			Role:           "user",
			Subtype:        domain.MessageSubtypeInjectionMemory,
			Content:        m.Content,
			Metadata:       map[string]any{"memory_id": m.ID},
			Delivered:      &delivered,
		})
	}
	delivered := true
	opening = append(opening, domain.Message{
		ConversationID: conversationID,
		UserID:         userID,
		Role:           "user",
		Subtype:        domain.MessageSubtypeInjectionTaskContext,
		Content:        taskContext,
		Delivered:      &delivered,
	})

	if before, ok := earliestAssemblyKey(rows); ok {
		hoistBefore(opening, before)
	}

	for i := range opening {
		if err := insert(ctx, &opening[i]); err != nil {
			return nil, err
		}
	}
	return opening, nil
}

// earliestAssemblyKey is the assembly position of the first row already on the
// transcript, or ok=false for one with none.
//
// It takes the minimum rather than reading rows[0], so it does not rest on the
// caller having handed over an ordered read: the cost is one pass over a
// handful of rows, and the failure it rules out is an opening silently minted
// into the middle of a conversation.
func earliestAssemblyKey(rows []domain.Message) (float64, bool) {
	first, found := 0.0, false
	for _, r := range rows {
		key := float64(r.ID)
		if r.Seq != nil {
			key = *r.Seq
		}
		if !found || key < first {
			first, found = key, true
		}
	}
	return first, found
}

// hoistBefore stamps the opening rows into the gap below before, in order and
// evenly spread, so all of them sort ahead of every row already on the
// transcript and none of them collides with another.
//
// The whole unit lands inside (before-1, before), which renumbers nothing: ids
// are the keys of the rows already there, and an integer id can never fall
// strictly between two of these fractions. That is the seq column's stated
// purpose — a position between two existing rows, bought without an
// insert-time renumbering dance.
func hoistBefore(opening []domain.Message, before float64) {
	n := len(opening)
	for i := range opening {
		seq := before - 1 + float64(i+1)/float64(n+1)
		opening[i].Seq = &seq
	}
}

// selectInjectedMemories picks the memories that ride the opening turn: the
// newest that fit the budget, handed back oldest-first for minting.
//
// The walk is newest-first and stops at the first memory that does not fit,
// rather than skipping it for a smaller older one, so the injected set is
// always a contiguous newest-first window — which is what makes the envelope's
// "these are the most recent, the rest are in the folder" true. There is no
// floor: a single memory over the byte cap is not injected either, and that
// run falls back to the folder the memory block already points it at.
//
// memories arrive oldest-first, as every entity read hands them back. An empty
// memory — a conversation that ended with nothing to remember — carries no
// content and is skipped without spending a slot.
func selectInjectedMemories(memories []domain.TaskMemory) []domain.TaskMemory {
	picked := make([]domain.TaskMemory, 0, maxInjectedMemories)
	size := 0
	for i := len(memories) - 1; i >= 0; i-- {
		m := memories[i]
		if m.Content == "" {
			continue
		}
		if len(picked)+1 > maxInjectedMemories || size+len(m.Content) > maxInjectedMemoryBytes {
			break
		}
		size += len(m.Content)
		picked = append(picked, m)
	}
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return picked
}

// openingRowsOf returns the rows a conversation was opened with — the injected
// memories and the task context — picked out of a transcript that may hold
// anything else besides.
//
// It exists for the one launch that does not mint: a re-claim with no session
// to resume, whose transcript already carries the opening the engagement that
// died minted, and whose new SDK process has no memory of any of it. Selecting
// by subtype rather than by position is what makes that true of a transcript
// the control plane wrote to in between.
//
// Order is left to the assembler, which sorts by COALESCE(seq, id) — the same
// key the hoist above stamps into.
func openingRowsOf(rows []domain.Message) []domain.Message {
	out := make([]domain.Message, 0, maxInjectedMemories+1)
	for _, r := range rows {
		switch r.Subtype {
		case domain.MessageSubtypeInjectionMemory, domain.MessageSubtypeInjectionTaskContext:
			out = append(out, r)
		}
	}
	return out
}
