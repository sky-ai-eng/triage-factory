package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=TaskMemoryStore --output=./mocks --case=underscore --with-expecter

// ErrInvalidMemorySource means an UpsertAgentMemory(System) call named a source
// outside domain.AllMemorySources(), or one that disagrees with its content:
// non-empty content requires 'agent' or 'generated', empty content requires
// 'none'. Refused rather than canonicalized — a caller that cannot say which of
// the two it is holding has a bug the store must not paper over, and the
// invariant "agent_content IS NULL exactly when source = 'none'" is the whole
// reason every entity read can filter on the content column alone.
var ErrInvalidMemorySource = errors.New("db: memory source does not match its content")

// ValidateMemorySource is the store door both dialects call before writing a
// conversation_memory row. Whitespace-only content counts as empty: it is what
// an agent that opened the file and wrote nothing leaves behind.
func ValidateMemorySource(content string, source domain.MemorySource) error {
	if !domain.IsMemorySource(string(source)) {
		return fmt.Errorf("%w: %q is not a memory source", ErrInvalidMemorySource, source)
	}
	if strings.TrimSpace(content) == "" {
		if source != domain.MemorySourceNone {
			return fmt.Errorf("%w: empty content requires source %q, got %q", ErrInvalidMemorySource, domain.MemorySourceNone, source)
		}
		return nil
	}
	if source == domain.MemorySourceNone {
		return fmt.Errorf("%w: source %q requires empty content", ErrInvalidMemorySource, domain.MemorySourceNone)
	}
	return nil
}

// TaskMemoryStore is the per-resource store for the conversation_memory table —
// the durable agent-side narrative for every conversation on every task.
// Lifted out of the pre-D2 package-level functions in
// internal/db/task_memory.go so multi-mode Postgres callers route through $N
// placeholders + explicit org_id + the dual-pool admin/app split.
//
// Method naming follows the dual-pool convention introduced with
// UsersStore / EntityStore / EventStore:
//
//   - Plain methods (UpsertAgentMemory, GetMemoriesForEntity) run on the app
//     pool in Postgres (RLS-active). Callers are request-handler equivalents
//     and must be inside WithTx in multi-mode so JWT claims (org_id, sub) are
//     set for RLS evaluation.
//   - `...System` methods (UpsertAgentMemorySystem, GetForConversationSystem,
//     GetMemoriesForEntitySystem, RecordEntityTouchSystem,
//     CountMemoriesForEntitySystem) run on the admin pool (BYPASSRLS). The
//     consumers are background goroutines without a JWT-claims context — the
//     delegate spawner's post-completion gate teardown and the
//     engagement-start materializer both fire from a goroutine with no request
//     scope. org_id stays bound in the INSERT/SELECT/UPDATE as defense in
//     depth.
//
// The precedent (e.g. EventStore's missing app-side GetMetadata) is to omit
// a System (or plain) variant until a real caller arrives rather than add
// one speculatively.
//
// SQLite collapses both pools onto the single connection. The
// `...System` methods are thin wrappers around their non-System
// counterparts; assertLocalOrg gates every entry point.
type TaskMemoryStore interface {
	// UpsertAgentMemory writes the memory row for a conversation.
	//
	// source says who wrote content and is validated at the door against it
	// (ValidateMemorySource): non-empty content requires
	// domain.MemorySourceAgent or MemorySourceGenerated, empty / whitespace-only
	// content requires MemorySourceNone. Never canonicalized — a mismatch is
	// ErrInvalidMemorySource and nothing is written. Empty content still lands
	// as SQL NULL, which is what lets every entity read filter a 'none' row out
	// on the content column alone.
	//
	// blueprintRunID is the conversation's blueprint run (denormalized from
	// the conversation); pass empty for a standalone conversation, where it
	// canonicalizes to SQL NULL. It groups one blueprint run's memory so the
	// materializer can fold each step's file into a shared namespace folder.
	//
	// Idempotent on (conversation_id) via ON CONFLICT — re-running the gate
	// after a retry overwrites agent_content, source and blueprint_run_id but
	// preserves the row's id and created_at. The conflict arm replacing source
	// is what lets an agent's own conclusion overwrite a generated stand-in.
	//
	// Returns the stored row, sourced from RETURNING on the write statement
	// itself — including the producing conversation's naming facts
	// (StepIndex, PromptName) a caller would otherwise have to re-read
	// GetMemoriesForEntity(System) to see, projected through the same join.
	UpsertAgentMemory(ctx context.Context, orgID, conversationID, blueprintRunID, content string, source domain.MemorySource) (domain.TaskMemory, error)

	// UpsertAgentMemorySystem is the admin-pool variant for the
	// delegate spawner's post-completion gate teardown. Fires inside
	// the runAgent goroutine, which has no JWT-claims context, so the
	// write routes around RLS via BYPASSRLS. Same validation, idempotency and
	// returned row as the non-System variant.
	UpsertAgentMemorySystem(ctx context.Context, orgID, conversationID, blueprintRunID, content string, source domain.MemorySource) (domain.TaskMemory, error)

	// GetForConversationSystem returns the conversation's own memory row,
	// including a source='none' row — the one read that sees what the entity
	// reads deliberately hide. It answers "has this conversation settled what
	// it remembered", which is a different question from "is there anything
	// here worth materializing", and a caller asking it must be told about the
	// row that says "nothing". nil when no row exists.
	//
	// Admin pool: the consumers are background goroutines with no JWT-claims
	// context. org_id stays bound as defense in depth.
	GetForConversationSystem(ctx context.Context, orgID, conversationID string) (*domain.TaskMemory, error)

	// GetMemoriesForEntity returns every conversation_memory row reachable for
	// this entity through conversation_memory_entities — the conversation
	// touched, produced for, or was primarily about this entity — oldest
	// first. Rows with no agent_content (source='none') are excluded: they
	// record that nothing was remembered, and materializing one would hand the
	// next agent an empty file to read. Each row carries its BlueprintRunID so
	// the materializer can tell this blueprint run's own steps from prior
	// separate conversations, plus the producing conversation's step index and
	// prompt name so it can name the materialized file after the work it
	// records rather than after a row id.
	GetMemoriesForEntity(ctx context.Context, orgID, entityID string) ([]domain.TaskMemory, error)

	// GetMemoriesForEntitySystem mirrors GetMemoriesForEntity but
	// routes through the admin pool. The consumer is the delegate
	// spawner's engagement-start materializer (materializeEntityMemories),
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
	// the same team-visibility filter and the same "has content" filter as
	// GetMemoriesForEntitySystem — it counts what a reader would get, not what
	// the table holds.
	CountMemoriesForEntitySystem(ctx context.Context, orgID, entityID, teamID string) (int, error)
}
