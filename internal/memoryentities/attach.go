// Package memoryentities owns the entity attach a conversation's memory needs
// to be reachable: the join rows in conversation_memory_entities that carry one
// conversation's narrative to every entity the work materially engaged.
//
// The join rows follow the conversation_memory row, not the process that
// produced it, so the rule lives here rather than on the spawner: every writer
// of a conversation's memory owes the same attach, and there is one spelling of
// which entity gets which role.
package memoryentities

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

var attachLog = logging.Component("memoryentities")

// Attach makes a conversation's memory reachable from its task's entity, the
// primary one. The other entities the work engaged are linked at verb time by
// the exec funnel: produced for the entity a landed change is about, touched
// for one the run read. Role precedence in RecordEntityTouchSystem (primary >
// produced > touched) converges the three, so this needs no ordering care
// against those mid-run writes — the primary entity ends primary even if it was
// also touched or produced.
//
// Best-effort and non-fatal: it runs after the conversation_memory upsert has
// already landed, so a join-row failure is logged and skipped, never aborts the
// caller.
//
// The attach is unconditional even though the upsert is not — the join row is
// what a later memory write on this conversation becomes reachable through, and
// writing it costs nothing when there is no memory yet. A nil taskMemory is an
// absent capability, nowhere to write, and attaches nothing.
func Attach(ctx context.Context, taskMemory db.TaskMemoryStore, orgID, conversationID, primaryEntityID string) {
	if taskMemory == nil {
		return
	}
	if err := taskMemory.RecordEntityTouchSystem(ctx, orgID, conversationID, primaryEntityID, domain.MemoryRolePrimary); err != nil {
		attachLog.Warn("attach primary entity to conversation memory failed", "conversation", conversationID, "entity", primaryEntityID, "error", err)
	}
}
