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
// (the entities the conversation's artifacts are about). Touched entities are
// recorded durably at verb time by the exec funnel; role precedence in
// RecordEntityTouchSystem (primary > produced > touched) converges all three
// sets, so this needs no ordering care against those mid-run writes — a
// touched row upgrades to produced here, and the primary entity ends primary
// even if it was also touched or produced.
//
// Best-effort and non-fatal throughout: it runs after the conversation_memory
// upsert has already landed, so a join-row failure is logged and skipped, never
// aborts the caller. A produced link skipped that way, or because the artifact
// carries no entity, is also counted (CountSkippedLink).
//
// The primary attach is unconditional even though the upsert is not — the join
// row is what a later memory write on this conversation becomes reachable
// through, and writing it costs nothing when there is no memory yet. A
// produced entity is the one its artifact was recorded against
// (domain.Artifact.EntityID): the exec funnel resolves, and creates where none
// exists yet, the entity each artifact is about before it writes the row, so a
// pull request the agent just opened has its stub already. A branch is about
// no entity and is skipped; any other artifact without one is one whose
// entity did not resolve when it was written, which was logged then.
//
// A nil store is an absent capability, not an error, and the two cases are not
// the same: a nil TaskMemory is nowhere to write, so nothing is attached at all
// — not even the primary row. A nil Artifacts store costs only the produced
// pass; the primary row still lands.
func Attach(ctx context.Context, stores db.Stores, orgID, conversationID, primaryEntityID string) {
	taskMemory, artifacts := stores.TaskMemory, stores.Artifacts
	if taskMemory == nil {
		return
	}
	// primary — the task's entity always carries the conversation's memory.
	if err := taskMemory.RecordEntityTouchSystem(ctx, orgID, conversationID, primaryEntityID, domain.MemoryRolePrimary); err != nil {
		attachLog.Warn("attach primary entity to conversation memory failed", "conversation", conversationID, "entity", primaryEntityID, "error", err)
	}

	if artifacts == nil {
		return
	}
	// produced — every external object the agent created/mutated, through
	// the entities the conversation's artifacts are about. A listing failure
	// leaves the upsert + primary row intact.
	arts, err := artifacts.ListByConversationSystem(ctx, orgID, conversationID)
	if err != nil {
		attachLog.Warn("list artifacts for produced-entity attach failed", "conversation", conversationID, "error", err)
		return
	}
	for _, a := range arts {
		if a.Provider == domain.ArtifactProviderGit {
			continue
		}
		if a.EntityID == "" {
			CountSkippedLink(ctx, orgID, domain.MemoryRoleProduced, a.Provider, nil)
			continue
		}
		if err := taskMemory.RecordEntityTouchSystem(ctx, orgID, conversationID, a.EntityID, domain.MemoryRoleProduced); err != nil {
			attachLog.Warn("attach produced entity to conversation memory failed", "conversation", conversationID, "entity", a.EntityID, "error", err)
			CountSkippedLink(ctx, orgID, domain.MemoryRoleProduced, a.Provider, err)
		}
	}
}
