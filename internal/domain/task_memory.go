package domain

import "time"

// MemorySource names who wrote a conversation_memory row's agent_content, and
// is the only thing that tells them apart once the text is in the column:
//
//   - agent — the conversation's own agent wrote its memory file and the
//     orchestrator ingested it. The authoritative account of what was tried.
//   - generated — TF composed the memory from the transcript because the agent
//     left none. Useful, but second-hand, and overwritten the moment an agent
//     conclusion arrives for the same conversation.
//   - none — nothing was remembered. agent_content is NULL and the row exists
//     only to record that the question was settled.
//
// Stored as text and validated at the store door rather than by a CHECK, the
// `type`/`origin` pattern. The invariant the door enforces: agent_content IS
// NULL exactly when source = 'none'.
type MemorySource string

const (
	MemorySourceAgent     MemorySource = "agent"
	MemorySourceGenerated MemorySource = "generated"
	MemorySourceNone      MemorySource = "none"
)

// AllMemorySources returns the source vocabulary. Same discipline as
// AllParkReasons: SQL cannot import a Go const, so the dual-dialect conformance
// suite derives its coverage from this set and a source added here but never
// taught to a store fails on both backends.
func AllMemorySources() []MemorySource {
	return []MemorySource{MemorySourceAgent, MemorySourceGenerated, MemorySourceNone}
}

// IsMemorySource reports whether source names a memory author. Closed-world:
// the empty string and anything unrecognized are not sources, which is what
// lets the store door refuse a write rather than canonicalize one.
func IsMemorySource(source string) bool {
	switch MemorySource(source) {
	case MemorySourceAgent, MemorySourceGenerated, MemorySourceNone:
		return true
	}
	return false
}

// TaskMemory is a durable per-conversation narrative of what an agent tried on a task
// and why. The agent writes it to the one fixed path `./_tfac/memory.md` in
// its run root; the orchestrator reads it at termination and ingests it into
// the `conversation_memory` table before worktree teardown. Materialized back
// into future conversations' worktrees under `_tfac/entity-memory/` so iterations on
// the same entity can read what prior attempts tried — and so the sibling steps
// of one blueprint run can read each other's memory as their handoff.
//
// Which entities a row is reachable from is the `conversation_memory_entities`
// join, not a column here. Stored in the `conversation_memory` table with a
// denormalized `blueprint_run_id` that groups one blueprint run's files.
type TaskMemory struct {
	ID             string
	ConversationID string
	BlueprintRunID string // denormalized from the conversation; empty for a standalone (non-blueprint) conversation
	Content        string
	Source         MemorySource
	CreatedAt      time.Time

	// StepIndex and PromptName come from the producing conversation, not the
	// memory row: they are what lets a materializer name the file after the
	// work it records — "02-implement.md" rather than an opaque id. Both are
	// zero-valued when the conversation carries no step index / prompt (or the
	// reader's visibility can't see it), so every consumer must tolerate their
	// absence.
	StepIndex  *int   // 0-based blueprint step index of the producing conversation
	PromptName string // display name of the prompt that conversation ran
}
