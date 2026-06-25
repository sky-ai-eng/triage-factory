package dbtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// TaskMemoryStoreFactory is what a per-backend test file hands to
// RunTaskMemoryStoreConformance. Returns:
//   - the wired TaskMemoryStore impl,
//   - the orgID to pass to every call,
//   - a TaskMemorySeeder the harness uses to drop the entity + run FK
//     chain (run_memory FKs to runs which FKs to tasks which FKs to
//     events which FKs to entities — the backends seed those rows
//     differently and the conformance harness shouldn't bake one
//     shape's schema into the assertions).
type TaskMemoryStoreFactory func(t *testing.T) (store db.TaskMemoryStore, orgID string, seed TaskMemorySeeder)

// TaskMemorySeeder is a bag of callbacks the conformance suite uses
// to stage fixture rows the TaskMemoryStore doesn't own. Each backend
// implements them against its own SQL.
type TaskMemorySeeder struct {
	// Run inserts the entity + event + prompt + task + run FK chain
	// needed to attach a run_memory row, and returns (runID, entityID).
	// suffix discriminates per-subtest seeds so the unique indexes on
	// entities/runs don't collide.
	Run func(t *testing.T, suffix string) (runID, entityID string)

	// BlueprintRun seeds a blueprint + blueprint_run row so a run_memory
	// row can carry a valid blueprint_run_id (run_memory FKs it with ON
	// DELETE SET NULL), and returns the blueprint_run id. Only the
	// round-trip subtest needs it.
	BlueprintRun func(t *testing.T, suffix string) (blueprintRunID string)
}

// RunTaskMemoryStoreConformance covers the TaskMemoryStore contract
// every backend impl must hold. The System variants are NOT covered
// by parallel cases — their behavior is documented as identical to
// the non-System counterparts and the SKY-306 cleanup pruned the
// per-method passthrough tests for variants that don't diverge.
//
// What's covered:
//
//   - UpsertAgentMemory writes agent_content and is idempotent on
//     (run_id); a retry overwrites agent_content but never tramples
//     human_content.
//   - Empty / whitespace-only content canonicalizes to SQL NULL
//     (factory's memory_missing derivation depends on the truth
//     condition "agent_content IS NULL").
//   - UpdateRunMemoryHumanContent lands on the existing row, also
//     canonicalizes empty / whitespace to NULL, and is logged-not-
//     fatal on missing rows.
//   - GetMemoriesForEntity returns rows ordered by created_at ASC
//     and materializes Content via the agent + separator + human
//     format when both halves are populated.
//   - GetRunMemory returns nil on miss and the materialized row
//     otherwise.
func RunTaskMemoryStoreConformance(t *testing.T, mk TaskMemoryStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("UpsertAgentMemory_writes_agent_content", func(t *testing.T) {
		s, orgID, seed := mk(t)
		runID, entityID := seed.Run(t, "upsert-agent")
		if err := s.UpsertAgentMemory(ctx, orgID, runID, entityID, "", "agent wrote this"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		mem, err := s.GetRunMemory(ctx, orgID, runID)
		if err != nil || mem == nil {
			t.Fatalf("GetRunMemory: mem=%v err=%v", mem, err)
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
				runID, entityID := seed.Run(t, "empty-"+tc.name)
				if err := s.UpsertAgentMemory(ctx, orgID, runID, entityID, "", tc.content); err != nil {
					t.Fatalf("UpsertAgentMemory: %v", err)
				}
				mem, err := s.GetRunMemory(ctx, orgID, runID)
				if err != nil || mem == nil {
					t.Fatalf("GetRunMemory: mem=%v err=%v", mem, err)
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
		runID, entityID := seed.Run(t, "idempotent")
		if err := s.UpsertAgentMemory(ctx, orgID, runID, entityID, "", "first attempt"); err != nil {
			t.Fatalf("first upsert: %v", err)
		}
		if err := s.UpdateRunMemoryHumanContent(ctx, orgID, runID, "human kept it as-is"); err != nil {
			t.Fatalf("seed human_content: %v", err)
		}
		if err := s.UpsertAgentMemory(ctx, orgID, runID, entityID, "", "second attempt"); err != nil {
			t.Fatalf("second upsert: %v", err)
		}
		mem, err := s.GetRunMemory(ctx, orgID, runID)
		if err != nil || mem == nil {
			t.Fatalf("GetRunMemory: mem=%v err=%v", mem, err)
		}
		if !strings.HasPrefix(mem.Content, "second attempt") {
			t.Errorf("Content prefix = %q, want to start with %q", mem.Content, "second attempt")
		}
		if !strings.HasSuffix(mem.Content, "human kept it as-is") {
			t.Errorf("Content suffix = %q, want to end with %q (re-upsert must NOT trample human field)", mem.Content, "human kept it as-is")
		}
	})

	t.Run("UpdateRunMemoryHumanContent_lands_on_existing_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		runID, entityID := seed.Run(t, "update-human")
		if err := s.UpsertAgentMemory(ctx, orgID, runID, entityID, "", "agent self-report"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		if err := s.UpdateRunMemoryHumanContent(ctx, orgID, runID, "Looks good."); err != nil {
			t.Fatalf("UpdateRunMemoryHumanContent: %v", err)
		}
		mem, err := s.GetRunMemory(ctx, orgID, runID)
		if err != nil || mem == nil {
			t.Fatalf("GetRunMemory: mem=%v err=%v", mem, err)
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

	t.Run("UpdateRunMemoryHumanContent_empty_canonicalizes_to_null", func(t *testing.T) {
		s, orgID, seed := mk(t)
		runID, entityID := seed.Run(t, "update-blank")
		if err := s.UpsertAgentMemory(ctx, orgID, runID, entityID, "", "agent text"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		if err := s.UpdateRunMemoryHumanContent(ctx, orgID, runID, "   \t  \n  "); err != nil {
			t.Fatalf("UpdateRunMemoryHumanContent: %v", err)
		}
		mem, err := s.GetRunMemory(ctx, orgID, runID)
		if err != nil || mem == nil {
			t.Fatalf("GetRunMemory: mem=%v err=%v", mem, err)
		}
		// Whitespace-only canonicalizes to NULL → materialized Content
		// is just the agent half with no separator.
		if mem.Content != "agent text" {
			t.Errorf("Content = %q, want %q (whitespace human_content should canonicalize to NULL)", mem.Content, "agent text")
		}
	})

	t.Run("UpdateRunMemoryHumanContent_missing_row_logged_not_fatal", func(t *testing.T) {
		// The handler skips this call when run_id is empty, but if
		// some other caller hits it with a runID that has no row,
		// returning an error would push a 5xx after the GitHub submit
		// already succeeded. Logged-and-nil is the right shape.
		s, orgID, _ := mk(t)
		if err := s.UpdateRunMemoryHumanContent(ctx, orgID, "00000000-0000-0000-0000-0000000000ff", "anything"); err != nil {
			t.Errorf("expected nil error on missing row (logged warning); got %v", err)
		}
	})

	t.Run("AppendRunMemoryOutcomeSystem_sets_then_appends_preserving_verdict", func(t *testing.T) {
		// The reconciler's final-memory capture (TFAC-464 β). On a row whose
		// human_content is empty it sets the note; on a row that already
		// carries a human verdict (the run was approved through TF, then the
		// PR later merged on GitHub) it APPENDS — the verdict must survive,
		// separated by a blank line.
		s, orgID, seed := mk(t)
		runID, entityID := seed.Run(t, "outcome-append")
		if err := s.UpsertAgentMemory(ctx, orgID, runID, entityID, "", "agent narrative"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}

		// Empty human_content → the note becomes the whole post-run section.
		if err := s.AppendRunMemoryOutcomeSystem(ctx, orgID, runID, "**Outcome:** PR merged on GitHub."); err != nil {
			t.Fatalf("AppendRunMemoryOutcomeSystem (set): %v", err)
		}
		mem, err := s.GetRunMemory(ctx, orgID, runID)
		if err != nil || mem == nil {
			t.Fatalf("GetRunMemory: mem=%v err=%v", mem, err)
		}
		if !strings.Contains(mem.Content, "## Human feedback (post-run)") || !strings.HasSuffix(mem.Content, "PR merged on GitHub.") {
			t.Fatalf("set did not materialize under the post-run heading: %q", mem.Content)
		}

		// Now simulate a prior human verdict and append a second note: both
		// survive, in order, blank-line separated.
		if err := s.UpdateRunMemoryHumanContent(ctx, orgID, runID, "Human approved as drafted."); err != nil {
			t.Fatalf("seed verdict: %v", err)
		}
		if err := s.AppendRunMemoryOutcomeSystem(ctx, orgID, runID, "**Outcome:** PR merged on GitHub."); err != nil {
			t.Fatalf("AppendRunMemoryOutcomeSystem (append): %v", err)
		}
		mem, err = s.GetRunMemory(ctx, orgID, runID)
		if err != nil || mem == nil {
			t.Fatalf("GetRunMemory after append: mem=%v err=%v", mem, err)
		}
		if !strings.Contains(mem.Content, "Human approved as drafted.") {
			t.Errorf("append trampled the human verdict: %q", mem.Content)
		}
		if !strings.Contains(mem.Content, "Human approved as drafted.\n\n**Outcome:** PR merged on GitHub.") {
			t.Errorf("append did not blank-line-separate the note after the verdict: %q", mem.Content)
		}
	})

	t.Run("AppendRunMemoryOutcomeSystem_empty_is_noop", func(t *testing.T) {
		s, orgID, seed := mk(t)
		runID, entityID := seed.Run(t, "outcome-empty")
		if err := s.UpsertAgentMemory(ctx, orgID, runID, entityID, "", "agent narrative"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		if err := s.AppendRunMemoryOutcomeSystem(ctx, orgID, runID, "   \n  "); err != nil {
			t.Fatalf("AppendRunMemoryOutcomeSystem: %v", err)
		}
		mem, err := s.GetRunMemory(ctx, orgID, runID)
		if err != nil || mem == nil {
			t.Fatalf("GetRunMemory: mem=%v err=%v", mem, err)
		}
		if mem.Content != "agent narrative" {
			t.Errorf("whitespace-only outcome should be a no-op; Content = %q", mem.Content)
		}
	})

	t.Run("AppendRunMemoryOutcomeSystem_missing_row_logged_not_fatal", func(t *testing.T) {
		// The external transition already landed on GitHub; a missing
		// run_memory row (purged / detached run) must not error the cycle.
		s, orgID, _ := mk(t)
		if err := s.AppendRunMemoryOutcomeSystem(ctx, orgID, "00000000-0000-0000-0000-0000000000fe", "**Outcome:** anything"); err != nil {
			t.Errorf("expected nil error on missing row (logged warning); got %v", err)
		}
	})

	t.Run("GetMemoriesForEntity_orders_by_created_at_ASC", func(t *testing.T) {
		// Materializer reads in oldest-first order so the next agent
		// reading prior memories sees them chronologically. Insert two
		// runs on the same entity with a sleep between them and pin
		// the slice order.
		s, orgID, seed := mk(t)
		run1, entityID := seed.Run(t, "order-first")
		if err := s.UpsertAgentMemory(ctx, orgID, run1, entityID, "", "first"); err != nil {
			t.Fatalf("upsert first: %v", err)
		}
		// Sleep so SQLite's second-resolution column doesn't tie. The
		// Postgres impl binds ns-resolution createdAt from Go side
		// (matches the EventStore precedent) so the sleep is belt +
		// suspenders.
		time.Sleep(1100 * time.Millisecond)
		run2, _ := seed.Run(t, "order-second")
		// Re-use the same entity by overriding the seeded run's entity_id.
		// The Run seeder returns a fresh entity per call; the test wants
		// the second memory on the same entity. Seeder shape can't be
		// changed mid-call, so write the second memory under run2 +
		// entityID (the first entity) by re-pointing — backends accept
		// any entity_id that exists, and the first entity does.
		if err := s.UpsertAgentMemory(ctx, orgID, run2, entityID, "", "second"); err != nil {
			t.Fatalf("upsert second: %v", err)
		}
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
		runID, entityID := seed.Run(t, "separator")
		if err := s.UpsertAgentMemory(ctx, orgID, runID, entityID, "", "agent reasoning"); err != nil {
			t.Fatalf("upsert agent: %v", err)
		}
		if err := s.UpdateRunMemoryHumanContent(ctx, orgID, runID, "human verdict"); err != nil {
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
		runID, entityID := seed.Run(t, "agent-only")
		if err := s.UpsertAgentMemory(ctx, orgID, runID, entityID, "", "agent reasoning only"); err != nil {
			t.Fatalf("upsert: %v", err)
		}
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

	t.Run("blueprint_run_id_round_trips", func(t *testing.T) {
		// The denormalized blueprint_run_id is what groups one blueprint
		// run's memory under a shared namespace folder. Pin that it survives
		// the write and both read paths, and that an empty value
		// canonicalizes to SQL NULL (the N=1 / standalone-run case).
		s, orgID, seed := mk(t)
		runID, entityID := seed.Run(t, "bp-roundtrip")
		blueprintRunID := seed.BlueprintRun(t, "bp-roundtrip")
		if err := s.UpsertAgentMemory(ctx, orgID, runID, entityID, blueprintRunID, "step memory"); err != nil {
			t.Fatalf("UpsertAgentMemory: %v", err)
		}
		mem, err := s.GetRunMemory(ctx, orgID, runID)
		if err != nil || mem == nil {
			t.Fatalf("GetRunMemory: mem=%v err=%v", mem, err)
		}
		if mem.BlueprintRunID != blueprintRunID {
			t.Errorf("GetRunMemory BlueprintRunID = %q, want %q", mem.BlueprintRunID, blueprintRunID)
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

		// Standalone run: empty blueprintRunID canonicalizes to SQL NULL and
		// reads back empty.
		run2, ent2 := seed.Run(t, "bp-null")
		if err := s.UpsertAgentMemory(ctx, orgID, run2, ent2, "", "standalone memory"); err != nil {
			t.Fatalf("UpsertAgentMemory standalone: %v", err)
		}
		mem2, err := s.GetRunMemory(ctx, orgID, run2)
		if err != nil || mem2 == nil {
			t.Fatalf("GetRunMemory standalone: mem=%v err=%v", mem2, err)
		}
		if mem2.BlueprintRunID != "" {
			t.Errorf("standalone BlueprintRunID = %q, want empty (NULL)", mem2.BlueprintRunID)
		}
	})

	t.Run("GetRunMemory_nil_on_missing_row", func(t *testing.T) {
		// Callers (factory's run summary, the resume picker)
		// interpret (nil, nil) as "no memory recorded yet," distinct
		// from a returned struct with empty Content. Drift here would
		// push them into branching on len(Content) instead of m==nil.
		s, orgID, _ := mk(t)
		got, err := s.GetRunMemory(ctx, orgID, "00000000-0000-0000-0000-00000000abcd")
		if err != nil {
			t.Fatalf("GetRunMemory: %v", err)
		}
		if got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
}
