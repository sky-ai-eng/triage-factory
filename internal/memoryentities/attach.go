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

// Attach makes a conversation's memory reachable from every entity the work
// materially engaged: the primary (task) entity, plus every entity it produced
// (derived from the conversation's artifacts). Touched entities are recorded
// durably at verb time by the exec funnel; role precedence in
// RecordEntityTouchSystem (primary > produced > touched) converges all three
// sets, so this needs no ordering care against those mid-run writes — a
// touched row upgrades to produced here, and the primary entity ends primary
// even if it was also touched or produced.
//
// Best-effort and non-fatal throughout: it runs after the conversation_memory
// upsert has already landed, so a join-row failure is logged and skipped, never
// aborts the caller.
//
// The primary attach is unconditional even though the upsert is not — the join
// row is what a later memory write on this conversation becomes reachable
// through, and writing it costs nothing when there is no memory yet. On the
// produced side, FindOrCreate (not lookup-only) is deliberate: a PR the agent
// just opened may not have been polled yet, so the attach mints the
// create-minimal stub the poller/enrichment path later fills (the artifact URL
// links it out). A repo-level artifact target (a branch push, or owner/repo
// with no '#N') maps to no entity and is skipped.
//
// A nil store is an absent capability, not an error, and the two cases are not
// the same: a nil taskMemory is nowhere to write, so nothing is attached at all
// — not even the primary row. A nil artifacts or entities store costs only the
// produced pass, which cannot resolve anything without them; the primary row
// still lands.
func Attach(ctx context.Context, taskMemory db.TaskMemoryStore, artifacts db.ArtifactStore, entities db.EntityStore, orgID, conversationID, primaryEntityID string) {
	if taskMemory == nil {
		return
	}
	// primary — the task's entity always carries the conversation's memory.
	if err := taskMemory.RecordEntityTouchSystem(ctx, orgID, conversationID, primaryEntityID, domain.MemoryRolePrimary); err != nil {
		attachLog.Warn("attach primary entity to conversation memory failed", "conversation", conversationID, "entity", primaryEntityID, "error", err)
	}

	if artifacts == nil || entities == nil {
		return
	}
	// produced — every external object the agent created/mutated, resolved from
	// the conversation's artifacts. A listing failure leaves the upsert +
	// primary row intact.
	arts, err := artifacts.ListByConversationSystem(ctx, orgID, conversationID)
	if err != nil {
		attachLog.Warn("list artifacts for produced-entity attach failed", "conversation", conversationID, "error", err)
		return
	}
	for _, a := range arts {
		source, sourceID, kind, ok := domain.EntityRefForExternal(a.Provider, a.Target)
		if !ok {
			continue
		}
		ent, _, err := entities.FindOrCreateSystem(ctx, orgID, source, sourceID, kind, "", a.URL)
		if err != nil || ent == nil {
			attachLog.Warn("resolve produced entity for conversation memory failed",
				"conversation", conversationID, "provider", a.Provider, "target", a.Target, "error", err)
			continue
		}
		if err := taskMemory.RecordEntityTouchSystem(ctx, orgID, conversationID, ent.ID, domain.MemoryRoleProduced); err != nil {
			attachLog.Warn("attach produced entity to conversation memory failed", "conversation", conversationID, "entity", ent.ID, "error", err)
		}
	}
}
