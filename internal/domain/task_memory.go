package domain

import "time"

// TaskMemory is a durable per-conversation narrative of what an agent tried on a task
// and why. The agent writes it to the one fixed path `./_tfac/memory.md` in
// its run root; the orchestrator reads it at termination and ingests it into
// the `conversation_memory` table before worktree teardown. Materialized back
// into future conversations' worktrees under `_tfac/entity-memory/` so iterations on
// the same entity can read what prior attempts tried — and so the sibling steps
// of one blueprint run can read each other's memory as their handoff.
//
// Stored in the `conversation_memory` table with a denormalized `entity_id` for fast
// entity-scoped queries (memory materialization walks the entity graph) and a
// denormalized `blueprint_run_id` that groups one blueprint run's files.
type TaskMemory struct {
	ID             string
	ConversationID string
	EntityID       string // denormalized from conversation→task→entity for fast entity-scoped queries
	BlueprintRunID string // denormalized from the conversation; empty for a standalone (non-blueprint) conversation
	Content        string
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
