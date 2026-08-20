package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=TaskMemoryStore --output=./mocks --case=underscore --with-expecter

// TaskMemoryStore is the per-resource store for the conversation_memory table —
// the durable agent-side narrative + the human's post-run verdict for
// every conversation on every task. Lifted out of the pre-D2 package-level
// functions in internal/db/task_memory.go so multi-mode Postgres
// callers route through $N placeholders + explicit org_id + the
// dual-pool admin/app split.
//
// Method naming follows the dual-pool convention introduced with
// UsersStore / EntityStore / EventStore:
//
//   - Plain methods (UpsertAgentMemory, UpdateConversationMemoryHumanContent,
//     GetMemoriesForEntity) run on the app pool in Postgres
//     (RLS-active). Callers are request-handler equivalents (review
//     submit, PR submit, swipe-discard cleanup, factory read-back)
//     and must be inside WithTx in multi-mode so JWT claims (org_id,
//     sub) are set for RLS evaluation.
//   - `...System` methods (UpsertAgentMemorySystem,
//     GetMemoriesForEntitySystem, RecordEntityTouchSystem,
//     CountMemoriesForEntitySystem) run on the admin pool (BYPASSRLS).
//     The consumers are background goroutines without a JWT-claims
//     context — the delegate spawner's post-completion gate teardown
//     and the engagement-start materializer both fire from inside the
//     spawner's `runAgent` goroutine which has no request scope.
//     org_id stays bound in the INSERT/SELECT as defense in depth.
//
// No System variant exists for UpdateConversationMemoryHumanContent (only
// called from HTTP handlers under request claims). Adding a
// speculative System variant would just be dead code the admin-pool
// conformance suite would have to cover for no consumer; the
// precedent (e.g. EventStore's missing app-side GetMetadata) is to
// omit unused variants until a real caller arrives.
//
// RecordEntityTouchSystem and CountMemoriesForEntitySystem are the
// deliberate exception to that precedent: they're the conversation_memory_entities
// foundation TFAC-622 lands ahead of their production callers, which
// arrive with the sibling touch-capture (TFAC-623) and memory-load
// (TFAC-624) tickets. Until then they're exercised only by tests and the
// tf_system grant conformance suite.
//
// SQLite collapses both pools onto the single connection. The
// `...System` methods are thin wrappers around their non-System
// counterparts; assertLocalOrg gates every entry point.
// TODO(TFAC-867): these single-row writes still return a bare error rather
// than the row they persisted: UpsertAgentMemory, UpsertAgentMemorySystem,
// UpdateConversationMemoryHumanContent,
// UpdateConversationMemoryHumanContentSystem.
type TaskMemoryStore interface {
	// UpsertAgentMemory writes the agent-side memory row for a conversation.
	// Empty / whitespace-only content canonicalizes to SQL NULL on
	// the way in so downstream consumers (factory's memory_missing
	// derivation) get a single truth condition for "agent didn't
	// comply with the gate."
	//
	// blueprintRunID is the conversation's blueprint run (denormalized from
	// the conversation, like entityID); pass empty for a standalone
	// conversation, where it canonicalizes to SQL NULL. It groups one
	// blueprint run's memory so the materializer can fold each step's file
	// into a shared namespace folder.
	//
	// Idempotent on (conversation_id) via ON CONFLICT — re-running the gate
	// after a retry overwrites agent_content but preserves the row's
	// id, created_at, and any human_content the user has already
	// attached.
	UpsertAgentMemory(ctx context.Context, orgID, conversationID, entityID, blueprintRunID, content string) error

	// UpsertAgentMemorySystem is the admin-pool variant for the
	// delegate spawner's post-completion gate teardown. Fires inside
	// the runAgent goroutine, which has no JWT-claims context, so the
	// write routes around RLS via BYPASSRLS. Same idempotency +
	// NULL-on-empty contract as the non-System variant.
	UpsertAgentMemorySystem(ctx context.Context, orgID, conversationID, entityID, blueprintRunID, content string) error

	// UpdateConversationMemoryHumanContent records the human's verdict on a
	// conversation's agent draft into the conversation_memory row keyed by
	// conversationID. The gate-teardown upsert at termination guarantees
	// the row exists by the time the human writes a verdict, so this is a
	// plain UPDATE with no INSERT-or-UPDATE branching.
	//
	// Empty / whitespace-only content canonicalizes to NULL, matching
	// UpsertAgentMemory's agent_content handling.
	//
	// A missing row is logged-and-returned-nil rather than failing
	// the call: the only way a conversationID with no row reaches here is a
	// non-agent review path or a cleanup race, and failing the
	// response after GitHub already accepted the review would be
	// worse than the missed memory write.
	//
	// App pool only — every caller (reviews handler, artifact-PR approve
	// handler, swipe-discard cleanup) runs under request claims.
	UpdateConversationMemoryHumanContent(ctx context.Context, orgID, conversationID, content string) error

	// UpdateConversationMemoryHumanContentSystem is the admin-pool (BYPASSRLS) variant
	// of UpdateConversationMemoryHumanContent for the artifact reconciler (TFAC-464 β),
	// which has no JWT-claims context. It overwrites human_content with the
	// conversation's post-run outcome — how its artifacts resolved on GitHub
	// (merged/closed/deleted/submitted) vs. what the agent drafted — so the
	// next agent on the entity reads the final state.
	//
	// Overwrite, not append: human_content is the single "how reality diverged
	// from your draft" slot, and the terminal outcome is its most authoritative
	// version, superseding any approval-time account. The reconciler composes
	// the note over the conversation's WHOLE artifact set each time one
	// resolves, so a branch-then-PR conversation accumulates correctly without
	// an append and a repeated cycle is idempotent. Same empty→NULL +
	// missing-row-logged-not-fatal contract as the app-pool variant. org_id
	// stays bound as defense in depth; SQLite collapses onto the one
	// connection.
	UpdateConversationMemoryHumanContentSystem(ctx context.Context, orgID, conversationID, content string) error

	// GetMemoriesForEntity returns every conversation_memory row reachable for
	// this entity through conversation_memory_entities — the conversation
	// touched, produced for, or was primarily about this entity — oldest
	// first. The returned TaskMemory.Content is materialized from
	// agent_content + human_content via the stable separator format
	// the next agent's prompt context parses. Each row carries its
	// BlueprintRunID so the materializer can tell this blueprint run's
	// own steps from prior separate conversations, plus the producing
	// conversation's step index and prompt name so it can name the
	// materialized file after the work it records rather than after a
	// row id.
	GetMemoriesForEntity(ctx context.Context, orgID, entityID string) ([]domain.TaskMemory, error)

	// GetMemoriesForEntitySystem mirrors GetMemoriesForEntity but
	// routes through the admin pool. The consumer is the delegate
	// spawner's engagement-start materializer (materializePriorMemories),
	// which fires inside the runAgent goroutine with no JWT-claims
	// context. org_id stays in the WHERE clause as defense in depth.
	//
	// teamID is the materializing conversation's owning team
	// (conversations.team_id). The admin pool bypasses RLS, so the
	// app-pool variant's team scoping (conversation_memory_all →
	// conversations_select) is hand-rolled here: the result is restricted
	// to memory whose parent conversation that team can see — team-visible
	// conversations owned by teamID plus any org-visible conversation —
	// matching what a member of that team sees in the UI (TFAC-506).
	// Without this, the System path returned ALL of the org's memory for
	// the entity, leaking other teams' conversation narratives into a
	// conversation they don't own. Private-visibility conversations are
	// excluded: they're creator-scoped and the System path carries no user
	// to match. SQLite (N=1, single team) ignores teamID — no cross-team
	// bleed is possible locally.
	GetMemoriesForEntitySystem(ctx context.Context, orgID, entityID, teamID string) ([]domain.TaskMemory, error)

	// GetRecentMemoriesForEntitySystem is GetMemoriesForEntitySystem capped to
	// the most recent `limit` rows, with the cap pushed INTO the query (ORDER BY
	// created_at DESC LIMIT) rather than fetched-all-then-sliced — so an
	// on-demand read on an entity with a long conversation history doesn't
	// transfer and materialize its entire memory just to keep the tail. Returns
	// oldest-first (ASC), identical to the unbounded read's ordering, so
	// callers compose the same way. Same team-visibility scope as
	// GetMemoriesForEntitySystem. limit must be positive — a non-positive limit
	// returns no rows (the store never treats it as unbounded, and Postgres
	// rejects a negative LIMIT); callers resolve a non-positive request to a
	// default before calling.
	GetRecentMemoriesForEntitySystem(ctx context.Context, orgID, entityID, teamID string, limit int) ([]domain.TaskMemory, error)

	// RecordEntityTouchSystem upserts a (conversation_id, entity_id) row in
	// conversation_memory_entities with role-precedence upgrade: insert if
	// absent; on conflict, set role only if the new role outranks the
	// stored one (domain.MemoryRoleOutranks — primary > produced >
	// touched), so a later stronger classification upgrades the row
	// and a weaker one never downgrades it. Admin pool only
	// (tf_system on executors); every caller is a goroutine-internal
	// write with no JWT-claims context. Best-effort by contract:
	// callers must never fail the operation that produced the touch
	// on this method's error.
	//
	// Exempt from the returned-row rule: it appends to a join table with ON
	// CONFLICT DO NOTHING, so there is no single row it can hand back on the
	// repeat touch that is its steady state.
	RecordEntityTouchSystem(ctx context.Context, orgID, conversationID, entityID, role string) error

	// CountMemoriesForEntitySystem returns the number of conversation_memory
	// rows reachable for entityID through conversation_memory_entities, under
	// the same team-visibility filter as GetMemoriesForEntitySystem.
	CountMemoriesForEntitySystem(ctx context.Context, orgID, entityID, teamID string) (int, error)
}
