package dbtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskMemoryStoreFactory is what a per-backend test file hands to
// RunTaskMemoryStoreConformance. Returns:
//   - the wired TaskMemoryStore impl,
//   - the orgID to pass to every call,
//   - a TaskMemorySeeder the harness uses to drop the entity + conversation FK
//     chain (conversation_memory FKs to conversations which FKs to tasks
//     which FKs to events which FKs to entities — the backends seed those rows
//     differently and the conformance harness shouldn't bake one
//     shape's schema into the assertions).
type TaskMemoryStoreFactory func(t *testing.T) (store db.TaskMemoryStore, orgID string, seed TaskMemorySeeder)

// TaskMemorySeedPromptName and TaskMemorySeedStepIndex are the producing-
// conversation facts every backend's Conversation seeder must stamp on the
// conversation it creates. The entity reads project both back onto
// TaskMemory (they are what
// lets a materializer name a memory after the work it records), so the suite
// asserts them against these values — pinning them here rather than per-backend
// keeps the two SQL trees answering the same question. The step index is
// deliberately non-zero so a column that never made it into the SELECT can't
// pass as an unset one.
const (
	TaskMemorySeedPromptName = "Task Memory Test"
	TaskMemorySeedStepIndex  = 2
)

// AssertTaskMemoryNamingFacts checks one read's row against what the seeders
// stamp. Exported so a backend's own team-scoped tests — the arm the shared
// suite deliberately leaves to them — assert the same contract.
func AssertTaskMemoryNamingFacts(t *testing.T, mem domain.TaskMemory) {
	t.Helper()
	if mem.PromptName != TaskMemorySeedPromptName {
		t.Errorf("PromptName = %q, want %q", mem.PromptName, TaskMemorySeedPromptName)
	}
	switch {
	case mem.StepIndex == nil:
		t.Errorf("StepIndex = nil, want %d", TaskMemorySeedStepIndex)
	case *mem.StepIndex != TaskMemorySeedStepIndex:
		t.Errorf("StepIndex = %d, want %d", *mem.StepIndex, TaskMemorySeedStepIndex)
	}
}

// TaskMemorySeeder is a bag of callbacks the conformance suite uses
// to stage fixture rows the TaskMemoryStore doesn't own. Each backend
// implements them against its own SQL.
type TaskMemorySeeder struct {
	// Conversation inserts the entity + event + prompt + task + conversation FK chain
	// needed to attach a conversation_memory row, and returns (conversationID, entityID).
	// The conversation it inserts carries TaskMemorySeedPromptName as its
	// prompt's name and TaskMemorySeedStepIndex as its blueprint step index.
	// suffix discriminates per-subtest seeds so the unique indexes on
	// entities/conversations don't collide.
	Conversation func(t *testing.T, suffix string) (conversationID, entityID string)

	// BlueprintRun seeds a blueprint + blueprint_run row so a conversation_memory
	// row can carry a valid blueprint_run_id (conversation_memory FKs it with ON
	// DELETE SET NULL), and returns the blueprint_run id. Only the
	// round-trip subtest needs it.
	BlueprintRun func(t *testing.T, suffix string) (blueprintRunID string)

	// Role reads back the role column of a conversation_memory_entities row
	// directly (bypassing the store interface, which has no
	// role-returning read) — used only by the RecordEntityTouchSystem
	// precedence subtest. Returns "" if no row exists for (conversationID, entityID).
	Role func(t *testing.T, conversationID, entityID string) string
}

// memoryForConversation finds the memory row belonging to conversationID in the
// slice GetMemoriesForEntity(entityID) returns — reads are join-based
// (conversation_memory_entities), so there is no direct by-conversation
// point read to call instead. Returns nil if no row matches.
func memoryForConversation(t *testing.T, ctx context.Context, s db.TaskMemoryStore, orgID, entityID, conversationID string) *domain.TaskMemory {
	t.Helper()
	mems, err := s.GetMemoriesForEntity(ctx, orgID, entityID)
	if err != nil {
		t.Fatalf("GetMemoriesForEntity: %v", err)
	}
	for i := range mems {
		if mems[i].ConversationID == conversationID {
			return &mems[i]
		}
	}
	return nil
}

// RunTaskMemoryStoreConformance covers the TaskMemoryStore contract
// every backend impl must hold. The System variants are NOT covered
// by parallel cases — their behavior is documented as identical to
// the non-System counterparts and a cleanup pruned the
// per-method passthrough tests for variants that don't diverge.
//
// What's covered:
//
//   - UpsertAgentMemory writes agent_content and is idempotent on
//     (conversation_id); a retry overwrites agent_content but never tramples
//     human_content.
//   - Empty / whitespace-only content canonicalizes to SQL NULL
//     (factory's memory_missing derivation depends on the truth
//     condition "agent_content IS NULL").
//   - UpdateConversationMemoryHumanContent lands on the existing row, also
//     canonicalizes empty / whitespace to NULL, and is logged-not-
//     fatal on missing rows.
//   - GetMemoriesForEntity returns rows reachable through
//     conversation_memory_entities ordered by created_at ASC and materializes
//     Content via the agent + separator + human format when both
//     halves are populated — including a row whose denormalized
//     rm.entity_id points elsewhere, as long as a join row ties the
//     conversation to the queried entity.
//   - RecordEntityTouchSystem upserts a join row with role-precedence
//     upgrade (primary > produced > touched) and is idempotent.
//
// Every UpsertAgentMemory(System) call below is paired with a
// RecordEntityTouchSystem(..., domain.MemoryRolePrimary) call — the
// join row a real conversation's completion will write once the
// completion-attach ticket lands. Without it, GetMemoriesForEntity's
// join-based read
// would see nothing (this ticket does not touch UpsertAgentMemorySystem
// itself; see the migration's non-goals).
func RunTaskMemoryStoreConformance(t *testing.T, mk TaskMemoryStoreFactory) {
	t.Helper()
	ctx := context.Background()

	seedPrimary := func(t *testing.T, s db.TaskMemoryStore, orgID, conversationID, entityID string) {
		t.Helper()
		if err := s.RecordEntityTouchSystem(ctx, orgID, conversationID, entityID, domain.MemoryRolePrimary); err != nil {
			t.Fatalf("RecordEntityTouchSystem: %v", err)
		}
	}

	t.Run("UpsertAgentMemory_writes_agent_content", func(t *testing.T) {
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "upsert-agent")
		if err := s.UpsertAgentMemory(ctx, orgID, conversationID, entityID, "", "agent wrote this"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)
		mem := memoryForConversation(t, ctx, s, orgID, entityID, conversationID)
		if mem == nil {
			t.Fatalf("memoryForConversation returned nil")
		}
		if mem.Content != "agent wrote this" {
			t.Errorf("Content = %q, want %q", mem.Content, "agent wrote this")
		}
	})

	t.Run("UpsertAgentMemory_empty_canonicalizes_to_null", func(t *testing.T) {
		// Row-presence-as-signal contract: empty + whitespace-only
		// content (the signals that the agent didn't pass through the
		// gate) both land as SQL NULL so factory's memory_missing
		// derivation can rely on the single condition.
		cases := []struct {
			name    string
			content string
		}{
			{"empty", ""},
			{"whitespace_only", "   \n\t  "},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s, orgID, seed := mk(t)
				conversationID, entityID := seed.Conversation(t, "empty-"+tc.name)
				if err := s.UpsertAgentMemory(ctx, orgID, conversationID, entityID, "", tc.content); err != nil {
					t.Fatalf("UpsertAgentMemory: %v", err)
				}
				seedPrimary(t, s, orgID, conversationID, entityID)
				mem := memoryForConversation(t, ctx, s, orgID, entityID, conversationID)
				if mem == nil {
					t.Fatalf("memoryForConversation returned nil")
				}
				// Materialized Content is the empty agent_content
				// fallback (no human content). The store still wrote
				// a row (idempotency check below depends on it) but
				// the column is NULL so Content materializes empty.
				if mem.Content != "" {
					t.Errorf("Content = %q, want \"\" (NULL canonicalization)", mem.Content)
				}
			})
		}
	})

	t.Run("UpsertAgentMemory_idempotent_preserves_human_content", func(t *testing.T) {
		// Re-running the gate (a retry after the first attempt produced
		// a memory file the second time) overwrites agent_content but
		// MUST leave any already-attached human_content intact — the
		// human writer (review submit, swipe-discard) might land
		// between the first agent attempt and the second.
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "idempotent")
		if err := s.UpsertAgentMemory(ctx, orgID, conversationID, entityID, "", "first attempt"); err != nil {
			t.Fatalf("first upsert: %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)
		if err := s.UpdateConversationMemoryHumanContent(ctx, orgID, conversationID, "human kept it as-is"); err != nil {
			t.Fatalf("seed human_content: %v", err)
		}
		if err := s.UpsertAgentMemory(ctx, orgID, conversationID, entityID, "", "second attempt"); err != nil {
			t.Fatalf("second upsert: %v", err)
		}
		mem := memoryForConversation(t, ctx, s, orgID, entityID, conversationID)
		if mem == nil {
			t.Fatalf("memoryForConversation returned nil")
		}
		if !strings.HasPrefix(mem.Content, "second attempt") {
			t.Errorf("Content prefix = %q, want to start with %q", mem.Content, "second attempt")
		}
		if !strings.HasSuffix(mem.Content, "human kept it as-is") {
			t.Errorf("Content suffix = %q, want to end with %q (re-upsert must NOT trample human field)", mem.Content, "human kept it as-is")
		}
	})

	t.Run("UpdateConversationMemoryHumanContent_lands_on_existing_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "update-human")
		if err := s.UpsertAgentMemory(ctx, orgID, conversationID, entityID, "", "agent self-report"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)
		if err := s.UpdateConversationMemoryHumanContent(ctx, orgID, conversationID, "Looks good."); err != nil {
			t.Fatalf("UpdateConversationMemoryHumanContent: %v", err)
		}
		mem := memoryForConversation(t, ctx, s, orgID, entityID, conversationID)
		if mem == nil {
			t.Fatalf("memoryForConversation returned nil")
		}
		if !strings.HasPrefix(mem.Content, "agent self-report") {
			t.Errorf("Content prefix = %q, want to start with %q", mem.Content, "agent self-report")
		}
		if !strings.Contains(mem.Content, "## Human feedback (post-run)") {
			t.Errorf("Content missing canonical separator marker; got %q", mem.Content)
		}
		if !strings.HasSuffix(mem.Content, "Looks good.") {
			t.Errorf("Content suffix = %q, want to end with %q", mem.Content, "Looks good.")
		}
	})

	t.Run("UpdateConversationMemoryHumanContent_empty_canonicalizes_to_null", func(t *testing.T) {
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "update-blank")
		if err := s.UpsertAgentMemory(ctx, orgID, conversationID, entityID, "", "agent text"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)
		if err := s.UpdateConversationMemoryHumanContent(ctx, orgID, conversationID, "   \t  \n  "); err != nil {
			t.Fatalf("UpdateConversationMemoryHumanContent: %v", err)
		}
		mem := memoryForConversation(t, ctx, s, orgID, entityID, conversationID)
		if mem == nil {
			t.Fatalf("memoryForConversation returned nil")
		}
		// Whitespace-only canonicalizes to NULL → materialized Content
		// is just the agent half with no separator.
		if mem.Content != "agent text" {
			t.Errorf("Content = %q, want %q (whitespace human_content should canonicalize to NULL)", mem.Content, "agent text")
		}
	})

	t.Run("UpdateConversationMemoryHumanContent_missing_row_logged_not_fatal", func(t *testing.T) {
		// The handler skips this call when the conversation id is empty, but if
		// some other caller hits it with a conversationID that has no row,
		// returning an error would push a 5xx after the GitHub submit
		// already succeeded. Logged-and-nil is the right shape.
		s, orgID, _ := mk(t)
		if err := s.UpdateConversationMemoryHumanContent(ctx, orgID, "00000000-0000-0000-0000-0000000000ff", "anything"); err != nil {
			t.Errorf("expected nil error on missing row (logged warning); got %v", err)
		}
	})

	t.Run("UpdateConversationMemoryHumanContentSystem_overwrites_prior_verdict", func(t *testing.T) {
		// The reconciler's post-run outcome capture (TFAC-464 β). human_content
		// is the single "how reality diverged from your draft" slot; the
		// terminal outcome supersedes any approval-time verdict. The admin-pool
		// System variant overwrites it (the reconciler has no claims context),
		// leaves agent_content untouched, and materializes under the post-run
		// heading.
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "system-overwrite")
		if err := s.UpsertAgentMemory(ctx, orgID, conversationID, entityID, "", "agent narrative"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)
		if err := s.UpdateConversationMemoryHumanContent(ctx, orgID, conversationID, "approval-time verdict"); err != nil {
			t.Fatalf("seed verdict: %v", err)
		}
		if err := s.UpdateConversationMemoryHumanContentSystem(ctx, orgID, conversationID, "**Post-run outcome** — PR merged on GitHub."); err != nil {
			t.Fatalf("UpdateConversationMemoryHumanContentSystem: %v", err)
		}
		mem := memoryForConversation(t, ctx, s, orgID, entityID, conversationID)
		if mem == nil {
			t.Fatalf("memoryForConversation returned nil")
		}
		if strings.Contains(mem.Content, "approval-time verdict") {
			t.Errorf("System overwrite did not supersede the prior verdict: %q", mem.Content)
		}
		if !strings.HasPrefix(mem.Content, "agent narrative") {
			t.Errorf("agent_content was trampled: %q", mem.Content)
		}
		if !strings.Contains(mem.Content, "## Human feedback (post-run)") || !strings.HasSuffix(mem.Content, "PR merged on GitHub.") {
			t.Errorf("outcome did not materialize under the post-run heading: %q", mem.Content)
		}
	})

	t.Run("UpdateConversationMemoryHumanContentSystem_missing_row_logged_not_fatal", func(t *testing.T) {
		// The external transition already landed on GitHub; a missing
		// conversation_memory row (purged / detached conversation) must not error
		// the cycle.
		s, orgID, _ := mk(t)
		if err := s.UpdateConversationMemoryHumanContentSystem(ctx, orgID, "00000000-0000-0000-0000-0000000000fe", "**Post-run outcome** — anything"); err != nil {
			t.Errorf("expected nil error on missing row (logged warning); got %v", err)
		}
	})

	t.Run("GetMemoriesForEntity_orders_by_created_at_ASC", func(t *testing.T) {
		// Materializer reads in oldest-first order so the next agent
		// reading prior memories sees them chronologically. Insert two
		// conversations on the same entity with a sleep between them and pin
		// the slice order.
		s, orgID, seed := mk(t)
		conv1, entityID := seed.Conversation(t, "order-first")
		if err := s.UpsertAgentMemory(ctx, orgID, conv1, entityID, "", "first"); err != nil {
			t.Fatalf("upsert first: %v", err)
		}
		seedPrimary(t, s, orgID, conv1, entityID)
		// Sleep so SQLite's second-resolution column doesn't tie. The
		// Postgres impl binds ns-resolution createdAt from Go side
		// (matches the EventStore precedent) so the sleep is belt +
		// suspenders.
		time.Sleep(1100 * time.Millisecond)
		conv2, _ := seed.Conversation(t, "order-second")
		// Re-use the same entity by overriding the seeded conversation's entity_id.
		// The Conversation seeder returns a fresh entity per call; the test wants
		// the second memory on the same entity. Seeder shape can't be
		// changed mid-call, so write the second memory under conv2 +
		// entityID (the first entity) by re-pointing — backends accept
		// any entity_id that exists, and the first entity does.
		if err := s.UpsertAgentMemory(ctx, orgID, conv2, entityID, "", "second"); err != nil {
			t.Fatalf("upsert second: %v", err)
		}
		seedPrimary(t, s, orgID, conv2, entityID)
		mems, err := s.GetMemoriesForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("GetMemoriesForEntity: %v", err)
		}
		if len(mems) != 2 {
			t.Fatalf("len(mems) = %d, want 2", len(mems))
		}
		if mems[0].Content != "first" || mems[1].Content != "second" {
			t.Errorf("order = [%q, %q], want [%q, %q]", mems[0].Content, mems[1].Content, "first", "second")
		}
	})

	t.Run("GetMemoriesForEntity_materializes_separator_when_both_halves_set", func(t *testing.T) {
		// When both agent_content and human_content are populated, the
		// materialized Content carries agent text + stable separator +
		// human verdict in that order. The next agent's prompt context
		// relies on this shape to parse the boundary, so a regression
		// here would silently corrupt memory replay.
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "separator")
		if err := s.UpsertAgentMemory(ctx, orgID, conversationID, entityID, "", "agent reasoning"); err != nil {
			t.Fatalf("upsert agent: %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)
		if err := s.UpdateConversationMemoryHumanContent(ctx, orgID, conversationID, "human verdict"); err != nil {
			t.Fatalf("update human: %v", err)
		}
		mems, err := s.GetMemoriesForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("GetMemoriesForEntity: %v", err)
		}
		if len(mems) != 1 {
			t.Fatalf("len(mems) = %d, want 1", len(mems))
		}
		got := mems[0].Content
		if !strings.HasPrefix(got, "agent reasoning") {
			t.Errorf("Content prefix = %q, want to start with %q", got, "agent reasoning")
		}
		if !strings.Contains(got, "## Human feedback (post-run)") {
			t.Errorf("Content missing canonical separator marker; got %q", got)
		}
		if !strings.HasSuffix(got, "human verdict") {
			t.Errorf("Content suffix = %q, want to end with %q", got, "human verdict")
		}
	})

	t.Run("GetMemoriesForEntity_agent_only_has_no_separator", func(t *testing.T) {
		// Common case in this PR: a row with agent_content but
		// human_content NULL renders without the separator marker.
		// Otherwise every materialized memory would carry an empty
		// "## Human feedback (post-run)" section the next agent has
		// to skip past.
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "agent-only")
		if err := s.UpsertAgentMemory(ctx, orgID, conversationID, entityID, "", "agent reasoning only"); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)
		mems, err := s.GetMemoriesForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("GetMemoriesForEntity: %v", err)
		}
		if len(mems) != 1 {
			t.Fatalf("len(mems) = %d, want 1", len(mems))
		}
		if mems[0].Content != "agent reasoning only" {
			t.Errorf("Content = %q, want %q (no separator when human_content is NULL)", mems[0].Content, "agent reasoning only")
		}
	})

	t.Run("GetMemoriesForEntity_finds_row_via_touched_join_on_different_entity", func(t *testing.T) {
		// The read path walks conversation_memory_entities membership, not the
		// denormalized rm.entity_id column — a conversation whose primary entity
		// is A must still surface for entity B once a join row ties
		// (conversation, B) at any role, even 'touched'.
		s, orgID, seed := mk(t)
		conversationID, entityA := seed.Conversation(t, "touch-join-a")
		_, entityB := seed.Conversation(t, "touch-join-b")
		if err := s.UpsertAgentMemory(ctx, orgID, conversationID, entityA, "", "cross-entity narrative"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		if err := s.RecordEntityTouchSystem(ctx, orgID, conversationID, entityB, domain.MemoryRoleTouched); err != nil {
			t.Fatalf("RecordEntityTouchSystem: %v", err)
		}
		memsB, err := s.GetMemoriesForEntity(ctx, orgID, entityB)
		if err != nil {
			t.Fatalf("GetMemoriesForEntity(entityB): %v", err)
		}
		if len(memsB) != 1 || memsB[0].ConversationID != conversationID || memsB[0].Content != "cross-entity narrative" {
			t.Fatalf("GetMemoriesForEntity(entityB) = %+v, want the conversation's memory via the touched join", memsB)
		}
	})

	t.Run("blueprint_run_id_round_trips", func(t *testing.T) {
		// The denormalized blueprint_run_id is what groups one blueprint
		// conversation's memory under a shared namespace folder. Pin that it
		// survives the write and both read paths, and that an empty value
		// canonicalizes to SQL NULL (the N=1 / standalone-conversation case).
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "bp-roundtrip")
		blueprintRunID := seed.BlueprintRun(t, "bp-roundtrip")
		if err := s.UpsertAgentMemory(ctx, orgID, conversationID, entityID, blueprintRunID, "step memory"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)
		mem := memoryForConversation(t, ctx, s, orgID, entityID, conversationID)
		if mem == nil {
			t.Fatalf("memoryForConversation returned nil")
		}
		if mem.BlueprintRunID != blueprintRunID {
			t.Errorf("memoryForConversation BlueprintRunID = %q, want %q", mem.BlueprintRunID, blueprintRunID)
		}
		mems, err := s.GetMemoriesForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("GetMemoriesForEntity: %v", err)
		}
		if len(mems) != 1 {
			t.Fatalf("len(mems) = %d, want 1", len(mems))
		}
		if mems[0].BlueprintRunID != blueprintRunID {
			t.Errorf("GetMemoriesForEntity BlueprintRunID = %q, want %q", mems[0].BlueprintRunID, blueprintRunID)
		}

		// Standalone conversation: empty blueprintRunID canonicalizes to SQL NULL
		// and reads back empty.
		conv2, ent2 := seed.Conversation(t, "bp-null")
		if err := s.UpsertAgentMemory(ctx, orgID, conv2, ent2, "", "standalone memory"); err != nil {
			t.Fatalf("UpsertAgentMemory standalone: %v", err)
		}
		seedPrimary(t, s, orgID, conv2, ent2)
		mem2 := memoryForConversation(t, ctx, s, orgID, ent2, conv2)
		if mem2 == nil {
			t.Fatalf("memoryForConversation standalone returned nil")
		}
		if mem2.BlueprintRunID != "" {
			t.Errorf("standalone BlueprintRunID = %q, want empty (NULL)", mem2.BlueprintRunID)
		}
	})

	t.Run("GetMemoriesForEntity_carries_producing_conversation_naming_facts", func(t *testing.T) {
		// The reader names materialized memory after the work it records —
		// step order plus the prompt that ran — so both entity reads must
		// project those two facts off the producing conversation, not just
		// the memory row's own columns.
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "naming-facts")
		if err := s.UpsertAgentMemory(ctx, orgID, conversationID, entityID, "", "agent wrote this"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)

		mems, err := s.GetMemoriesForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("GetMemoriesForEntity: %v", err)
		}
		if len(mems) != 1 {
			t.Fatalf("len(mems) = %d, want 1", len(mems))
		}
		AssertTaskMemoryNamingFacts(t, mems[0])
	})

	t.Run("RecordEntityTouchSystem_role_precedence", func(t *testing.T) {
		// primary > produced > touched: a later stronger classification
		// upgrades the row; a weaker one never downgrades it; re-recording
		// the same (winning) role is a no-op.
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "touch-precedence")

		assertRole := func(want string) {
			t.Helper()
			if got := seed.Role(t, conversationID, entityID); got != want {
				t.Errorf("role = %q, want %q", got, want)
			}
		}

		if err := s.RecordEntityTouchSystem(ctx, orgID, conversationID, entityID, domain.MemoryRoleTouched); err != nil {
			t.Fatalf("record touched: %v", err)
		}
		assertRole(domain.MemoryRoleTouched)

		if err := s.RecordEntityTouchSystem(ctx, orgID, conversationID, entityID, domain.MemoryRoleProduced); err != nil {
			t.Fatalf("record produced: %v", err)
		}
		assertRole(domain.MemoryRoleProduced)

		// A weaker role never downgrades an already-stronger row.
		if err := s.RecordEntityTouchSystem(ctx, orgID, conversationID, entityID, domain.MemoryRoleTouched); err != nil {
			t.Fatalf("re-record touched: %v", err)
		}
		assertRole(domain.MemoryRoleProduced)

		if err := s.RecordEntityTouchSystem(ctx, orgID, conversationID, entityID, domain.MemoryRolePrimary); err != nil {
			t.Fatalf("record primary: %v", err)
		}
		assertRole(domain.MemoryRolePrimary)

		// Idempotent re-record of the current (winning) role.
		if err := s.RecordEntityTouchSystem(ctx, orgID, conversationID, entityID, domain.MemoryRolePrimary); err != nil {
			t.Fatalf("re-record primary: %v", err)
		}
		assertRole(domain.MemoryRolePrimary)
	})
}
