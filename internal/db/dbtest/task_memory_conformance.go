package dbtest

import (
	"context"
	"errors"
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
// TaskMemory (they are what lets a materializer name a memory after the work it
// records), so the suite asserts them against these values — pinning them here
// rather than per-backend keeps the two SQL trees answering the same question.
// The step index is deliberately non-zero so a column that never made it into
// the SELECT can't pass as an unset one.
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

	// TeamID owns every conversation Conversation seeds — what the team-scoped
	// System entity reads must be handed to see them. SQLite ignores it (N=1,
	// one team); Postgres hand-rolls its team filter off it.
	TeamID string

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

// memoryForConversation point-reads the conversation's own memory row. It uses
// GetForConversationSystem deliberately: it is the one read that shows a
// source='none' row, so a test asserting what was written never has to know in
// advance whether the entity reads would hide it.
func memoryForConversation(t *testing.T, ctx context.Context, s db.TaskMemoryStore, orgID, conversationID string) *domain.TaskMemory {
	t.Helper()
	mem, err := s.GetForConversationSystem(ctx, orgID, conversationID)
	if err != nil {
		t.Fatalf("GetForConversationSystem: %v", err)
	}
	return mem
}

// RunTaskMemoryStoreConformance covers the TaskMemoryStore contract
// every backend impl must hold. The System variants are NOT covered
// by parallel cases — their behavior is documented as identical to
// the non-System counterparts and a cleanup pruned the
// per-method passthrough tests for variants that don't diverge.
//
// What's covered:
//
//   - UpsertAgentMemory writes agent_content + source and is idempotent on
//     (conversation_id); the conflict arm replaces both, so an agent's own
//     conclusion supersedes a generated stand-in.
//   - The source door: every value of domain.AllMemorySources() round-trips,
//     and a source that disagrees with its content is refused outright rather
//     than canonicalized.
//   - A source='none' row is invisible to every entity read and visible to
//     GetForConversationSystem — the difference between "nothing worth
//     materializing" and "this conversation settled on remembering nothing".
//   - GetMemoriesForEntity returns rows reachable through
//     conversation_memory_entities ordered by created_at ASC — including a row
//     whose conversation's primary entity is elsewhere, as long as a join row
//     ties the conversation to the queried entity.
//   - RecordEntityTouchSystem upserts a join row with role-precedence
//     upgrade (primary > produced > touched) and is idempotent.
//
// Every UpsertAgentMemory(System) call whose row must show up in an entity read
// is paired with a RecordEntityTouchSystem(..., domain.MemoryRolePrimary) call
// — the join row a real conversation's completion writes beside its memory.
// Without it, the join-based entity reads would see nothing.
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
		if _, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", "agent wrote this", domain.MemorySourceAgent); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)
		mem := memoryForConversation(t, ctx, s, orgID, conversationID)
		if mem == nil {
			t.Fatalf("memoryForConversation returned nil")
		}
		if mem.Content != "agent wrote this" {
			t.Errorf("Content = %q, want %q", mem.Content, "agent wrote this")
		}
		if mem.Source != domain.MemorySourceAgent {
			t.Errorf("Source = %q, want %q", mem.Source, domain.MemorySourceAgent)
		}
	})

	t.Run("every_memory_source_round_trips", func(t *testing.T) {
		// Derived from the Go vocabulary, not a literal list: a source added in
		// domain but never taught to a store fails here on both backends.
		for _, src := range domain.AllMemorySources() {
			t.Run(string(src), func(t *testing.T) {
				s, orgID, seed := mk(t)
				conversationID, _ := seed.Conversation(t, "source-"+string(src))
				content := "content written by " + string(src)
				if src == domain.MemorySourceNone {
					content = ""
				}
				written, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", content, src)
				if err != nil {
					t.Fatalf("UpsertAgentMemory(%s): %v", src, err)
				}
				if written.Source != src {
					t.Errorf("returned Source = %q, want %q", written.Source, src)
				}
				mem := memoryForConversation(t, ctx, s, orgID, conversationID)
				if mem == nil {
					t.Fatalf("memoryForConversation returned nil")
				}
				if mem.Source != src || mem.Content != content {
					t.Errorf("read back (Source=%q, Content=%q), want (%q, %q)", mem.Source, mem.Content, src, content)
				}
			})
		}
	})

	t.Run("UpsertAgentMemory_refuses_a_source_that_disagrees_with_its_content", func(t *testing.T) {
		// The invariant every entity read leans on — agent_content IS NULL
		// exactly when source = 'none' — is held here and nowhere else (no
		// CHECK, either dialect). A refusal writes nothing at all.
		cases := []struct {
			name    string
			content string
			source  domain.MemorySource
		}{
			{"empty_content_claiming_agent", "", domain.MemorySourceAgent},
			{"whitespace_content_claiming_generated", "  \n\t ", domain.MemorySourceGenerated},
			{"content_claiming_none", "the agent wrote this", domain.MemorySourceNone},
			{"unset_source", "the agent wrote this", ""},
			{"unknown_source", "the agent wrote this", domain.MemorySource("human")},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s, orgID, seed := mk(t)
				conversationID, _ := seed.Conversation(t, "refuse-"+tc.name)
				if _, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", tc.content, tc.source); !errors.Is(err, db.ErrInvalidMemorySource) {
					t.Fatalf("UpsertAgentMemory(%q, %q) error = %v, want ErrInvalidMemorySource", tc.content, tc.source, err)
				}
				if mem := memoryForConversation(t, ctx, s, orgID, conversationID); mem != nil {
					t.Errorf("a refused write left a row behind: %+v", mem)
				}
			})
		}
	})

	t.Run("none_rows_are_invisible_to_every_entity_read", func(t *testing.T) {
		// A 'none' row records that the conversation settled on remembering
		// nothing. Materializing it would hand the next agent an empty file and
		// counting it would claim history that isn't there — so every entity
		// read hides it, and the per-conversation read is the one place it
		// shows.
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "none-row")
		if _, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", "", domain.MemorySourceNone); err != nil {
			t.Fatalf("UpsertAgentMemory(none): %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)

		mems, err := s.GetMemoriesForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("GetMemoriesForEntity: %v", err)
		}
		if len(mems) != 0 {
			t.Errorf("GetMemoriesForEntity = %+v, want no rows", mems)
		}
		memsSystem, err := s.GetMemoriesForEntitySystem(ctx, orgID, entityID, seed.TeamID)
		if err != nil {
			t.Fatalf("GetMemoriesForEntitySystem: %v", err)
		}
		if len(memsSystem) != 0 {
			t.Errorf("GetMemoriesForEntitySystem = %+v, want no rows", memsSystem)
		}
		recent, err := s.GetRecentMemoriesForEntitySystem(ctx, orgID, entityID, seed.TeamID, 10)
		if err != nil {
			t.Fatalf("GetRecentMemoriesForEntitySystem: %v", err)
		}
		if len(recent) != 0 {
			t.Errorf("GetRecentMemoriesForEntitySystem = %+v, want no rows", recent)
		}
		n, err := s.CountMemoriesForEntitySystem(ctx, orgID, entityID, seed.TeamID)
		if err != nil {
			t.Fatalf("CountMemoriesForEntitySystem: %v", err)
		}
		if n != 0 {
			t.Errorf("CountMemoriesForEntitySystem = %d, want 0", n)
		}

		mem := memoryForConversation(t, ctx, s, orgID, conversationID)
		if mem == nil {
			t.Fatalf("GetForConversationSystem returned nil; a 'none' row must be visible there")
		}
		if mem.Source != domain.MemorySourceNone || mem.Content != "" {
			t.Errorf("GetForConversationSystem = (Source=%q, Content=%q), want (none, \"\")", mem.Source, mem.Content)
		}
	})

	t.Run("generated_is_overwritten_by_the_agents_own_conclusion", func(t *testing.T) {
		// The conflict arm replaces agent_content and source together. A
		// generated stand-in written while the agent was still working must not
		// survive the agent's own account of the same conversation.
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "generated-then-agent")
		first, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", "TF composed this from the transcript", domain.MemorySourceGenerated)
		if err != nil {
			t.Fatalf("UpsertAgentMemory(generated): %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)
		if _, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", "what I actually tried", domain.MemorySourceAgent); err != nil {
			t.Fatalf("UpsertAgentMemory(agent): %v", err)
		}
		mem := memoryForConversation(t, ctx, s, orgID, conversationID)
		if mem == nil {
			t.Fatalf("memoryForConversation returned nil")
		}
		if mem.Source != domain.MemorySourceAgent || mem.Content != "what I actually tried" {
			t.Errorf("after the agent write = (Source=%q, Content=%q), want (agent, %q)", mem.Source, mem.Content, "what I actually tried")
		}
		if mem.ID != first.ID || !mem.CreatedAt.Equal(first.CreatedAt) {
			t.Errorf("the overwrite minted a new row: %+v, want the id/created_at of %+v", mem, first)
		}
	})

	t.Run("GetForConversationSystem_is_nil_when_the_conversation_has_no_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		conversationID, _ := seed.Conversation(t, "no-memory")
		mem, err := s.GetForConversationSystem(ctx, orgID, conversationID)
		if err != nil || mem != nil {
			t.Errorf("GetForConversationSystem on a conversation with no row = (%v, %v), want (nil, nil)", mem, err)
		}
	})

	t.Run("GetMemoriesForEntity_orders_by_created_at_ASC", func(t *testing.T) {
		// Materializer reads in oldest-first order so the next agent
		// reading prior memories sees them chronologically. Insert two
		// conversations on the same entity with a sleep between them and pin
		// the slice order.
		s, orgID, seed := mk(t)
		conv1, entityID := seed.Conversation(t, "order-first")
		if _, err := s.UpsertAgentMemory(ctx, orgID, conv1, "", "first", domain.MemorySourceAgent); err != nil {
			t.Fatalf("upsert first: %v", err)
		}
		seedPrimary(t, s, orgID, conv1, entityID)
		// Sleep so SQLite's second-resolution column doesn't tie. The
		// Postgres impl binds ns-resolution createdAt from Go side
		// (matches the EventStore precedent) so the sleep is belt +
		// suspenders.
		time.Sleep(1100 * time.Millisecond)
		conv2, _ := seed.Conversation(t, "order-second")
		// The Conversation seeder returns a fresh entity per call; the test
		// wants the second memory reachable from the FIRST entity, which the
		// join row below is what decides.
		if _, err := s.UpsertAgentMemory(ctx, orgID, conv2, "", "second", domain.MemorySourceAgent); err != nil {
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

	t.Run("GetMemoriesForEntity_finds_row_via_touched_join_on_different_entity", func(t *testing.T) {
		// The read path walks conversation_memory_entities membership — a
		// conversation whose primary entity is A must still surface for entity B
		// once a join row ties (conversation, B) at any role, even 'touched'.
		s, orgID, seed := mk(t)
		conversationID, entityA := seed.Conversation(t, "touch-join-a")
		_, entityB := seed.Conversation(t, "touch-join-b")
		if _, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", "cross-entity narrative", domain.MemorySourceAgent); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityA)
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
		// canonicalizes to SQL NULL (the standalone-conversation case).
		s, orgID, seed := mk(t)
		conversationID, entityID := seed.Conversation(t, "bp-roundtrip")
		blueprintRunID := seed.BlueprintRun(t, "bp-roundtrip")
		if _, err := s.UpsertAgentMemory(ctx, orgID, conversationID, blueprintRunID, "step memory", domain.MemorySourceAgent); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		seedPrimary(t, s, orgID, conversationID, entityID)
		mem := memoryForConversation(t, ctx, s, orgID, conversationID)
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
		if _, err := s.UpsertAgentMemory(ctx, orgID, conv2, "", "standalone memory", domain.MemorySourceAgent); err != nil {
			t.Fatalf("UpsertAgentMemory standalone: %v", err)
		}
		seedPrimary(t, s, orgID, conv2, ent2)
		mem2 := memoryForConversation(t, ctx, s, orgID, conv2)
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
		if _, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", "agent wrote this", domain.MemorySourceAgent); err != nil {
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

// RunTaskMemoryReturnedRowConformance covers the returned-row standard for
// TaskMemoryStore's two single-row writes: UpsertAgentMemory and
// UpsertAgentMemorySystem. Each hands back the identical shape
// GetForConversationSystem would show for the same row — including the
// producing conversation's naming facts (StepIndex, PromptName).
//
// RecordEntityTouchSystem is not covered here: it carries its own stated
// exemption on the interface (an ON CONFLICT DO NOTHING append to a join
// table, with nothing to hand back on the repeat touch that is its steady
// state).
func RunTaskMemoryReturnedRowConformance(t *testing.T, mk TaskMemoryStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("UpsertAgentMemory_returns_the_stored_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		conversationID, _ := seed.Conversation(t, "rr-upsert")
		read := func() (*domain.TaskMemory, error) {
			return s.GetForConversationSystem(ctx, orgID, conversationID)
		}

		mem, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", "first draft", domain.MemorySourceAgent)
		if err != nil {
			t.Fatalf("UpsertAgentMemory (insert): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertAgentMemory (insert)", mem, read)

		// The conflict arm is the one designed to disagree with a naive
		// re-read of the caller's input — it overwrites agent_content and
		// source but preserves id/created_at — so it gets its own pin.
		mem2, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", "second draft", domain.MemorySourceAgent)
		if err != nil {
			t.Fatalf("UpsertAgentMemory (conflict): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertAgentMemory (conflict)", mem2, read)
		if mem2.ID != mem.ID || !mem2.CreatedAt.Equal(mem.CreatedAt) {
			t.Errorf("UpsertAgentMemory (conflict) = %+v, want the original id/created_at preserved from %+v", mem2, mem)
		}
	})

	t.Run("UpsertAgentMemorySystem_returns_the_stored_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		conversationID, _ := seed.Conversation(t, "rr-upsert-system")

		mem, err := s.UpsertAgentMemorySystem(ctx, orgID, conversationID, "", "agent narrative", domain.MemorySourceAgent)
		if err != nil {
			t.Fatalf("UpsertAgentMemorySystem: %v", err)
		}
		read := func() (*domain.TaskMemory, error) {
			return s.GetForConversationSystem(ctx, orgID, conversationID)
		}
		AssertWriteReturnedStoredRow(t, "UpsertAgentMemorySystem", mem, read)
	})

	t.Run("UpsertAgentMemory_returns_the_stored_none_row", func(t *testing.T) {
		// A 'none' write is still a row, and its returned shape must match the
		// stored one — the read the entity queries hide it from is not the one
		// the write answers with.
		s, orgID, seed := mk(t)
		conversationID, _ := seed.Conversation(t, "rr-upsert-none")

		mem, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", "", domain.MemorySourceNone)
		if err != nil {
			t.Fatalf("UpsertAgentMemory (none): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertAgentMemory (none)", mem, func() (*domain.TaskMemory, error) {
			return s.GetForConversationSystem(ctx, orgID, conversationID)
		})
	})
}

// RunTaskMemoryAppPoolReturnedRowConformance is
// RunTaskMemoryReturnedRowConformance's RLS-focused arm — the same wiring
// shape TestJiraAppsStore_Postgres_ReturnedRowConformance and
// RunConversationAppPoolReturnedRowConformance use and for the same reason:
// wiring both pools to AdminDB (what RunTaskMemoryReturnedRowConformance's
// plain conformance run does) is BYPASSRLS, and a RETURNING clause on a
// BYPASSRLS connection returns its row unconditionally. Under RLS it does
// not — the write's RETURNING has to satisfy the SELECT policy for the row
// it hands back, so a policy that admits the write but not the read-back
// yields zero rows from a statement that updated one.
//
// It covers only UpsertAgentMemory — its System twin always routes through the
// true admin pool in production (BYPASSRLS), never through a claims-carrying
// app-pool transaction, so testing it here would exercise a code path that
// never happens: RunTaskMemoryReturnedRowConformance's admin-pool wiring
// already covers it.
func RunTaskMemoryAppPoolReturnedRowConformance(t *testing.T, mk TaskMemoryStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("UpsertAgentMemory_returns_the_stored_row_under_RLS", func(t *testing.T) {
		s, orgID, seed := mk(t)
		conversationID, _ := seed.Conversation(t, "rr-app-upsert")
		read := func() (*domain.TaskMemory, error) {
			return s.GetForConversationSystem(ctx, orgID, conversationID)
		}

		mem, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", "first draft", domain.MemorySourceAgent)
		if err != nil {
			t.Fatalf("UpsertAgentMemory (insert): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertAgentMemory (insert)", mem, read)

		mem2, err := s.UpsertAgentMemory(ctx, orgID, conversationID, "", "second draft", domain.MemorySourceAgent)
		if err != nil {
			t.Fatalf("UpsertAgentMemory (conflict): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertAgentMemory (conflict)", mem2, read)
	})
}
