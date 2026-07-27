package dbtest

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ConversationStoreFactory is what a per-backend test file hands to
// RunConversationStoreConformance. Returns:
//   - the wired ConversationStore impl,
//   - the orgID to pass to every call,
//   - a userID for user-attributed write paths,
//   - an ConversationSeeder for entity/task/conversation/conversation_memory
//     fixtures the harness needs but doesn't go through the store
//     to provide.
type ConversationStoreFactory func(t *testing.T) (
	store db.ConversationStore,
	orgID, userID string,
	seed ConversationSeeder,
)

// ClaimRow is the seeder's projection of one claims row, oldest-first —
// enough for the conformance suite to assert mint/release/phase bookkeeping
// without the suite owning claim SQL.
type ClaimRow struct {
	ID         string
	ExecutorID string
	BootEpoch  int64
	Phase      string
	Released   bool
	Outcome    string
}

// ConversationSeeder is a bag of callbacks the conformance suite uses
// to stage fixture rows that aren't store-writable. Each backend
// test file implements them against its own SQL.
type ConversationSeeder struct {
	// Entity inserts an active GitHub PR entity and returns its ID.
	Entity func(t *testing.T, suffix string) string

	// Event inserts an entity-attached event with the given event
	// type. Returns the event ID.
	Event func(t *testing.T, entityID, eventType string) string

	// Task inserts a task row tied to the given entity and event.
	// status defaults to "queued" — the factory's behavior tests
	// don't care about the parent task status, only about the runs
	// hanging off it.
	Task func(t *testing.T, entityID, eventType, primaryEventID string) string

	// Run inserts a conversations row directly and returns its id (run.ID
	// when set, a fresh uuid otherwise). RunQueueStore.EnqueueRun is the
	// only production mint; the conformance suite stages rows in arbitrary
	// status without driving the queue. Fields honored: ID, TaskID,
	// PromptID, Status, Model, TriggerType (default manual), TriggerID,
	// BlueprintRunID.
	Run func(t *testing.T, run domain.Conversation) string

	// ClaimRows returns the conversation's claims rows oldest-first, so
	// the suite can assert mint/release bookkeeping.
	ClaimRows func(t *testing.T, conversationID string) []ClaimRow

	// BlueprintRun mints a blueprint + blueprint_run pair against the
	// given taskID and returns the blueprint_run id. Every conversation
	// row carries a NOT NULL blueprint_run_id FK (a single prompt is a
	// 1-step blueprint), so the conformance suite stages one of these
	// per run it creates and sets domain.Conversation.BlueprintRunID. Each
	// call mints a fresh independent blueprint_run — that matches the
	// real firing model (one delegation = one blueprint_run) and keeps
	// multi-run-per-task subtests realistic.
	BlueprintRun func(t *testing.T, taskID string) string

	// SetBlueprintRunStatus raw-updates a blueprint_run's status, WITHOUT
	// touching its child runs (a plain UPDATE, not BlueprintStore.MarkRunStatus,
	// which now cascades a terminal flip onto children). Used to stage the
	// "parked run under an already-terminal parent" precondition the
	// worktree-preserve filter must exclude.
	SetBlueprintRunStatus func(t *testing.T, blueprintRunID, status string)

	// StampAgentClaim sets the task's claimed_by_agent_id directly.
	// Used to set up task-claim preconditions for claim-flip
	// assertions.
	StampAgentClaim func(t *testing.T, taskID, agentID string)

	// SetRunMemory upserts a conversation_memory row with the given
	// agent_content. content="" inserts an empty string;
	// NullMemorySentinel inserts SQL NULL.
	SetRunMemory func(t *testing.T, runID, entityID, content string)

	// SeedRawMessage inserts a messages row with rawJSON written
	// directly into the given column ("reasoning" or "content_blocks"),
	// bypassing InsertMessage's json.Marshal. Used to stage
	// well-formed-but-wrong-shaped JSON (a valid jsonb value in Postgres,
	// which enforces syntax at the storage layer, that nonetheless fails to
	// unmarshal into the target Go slice) so a test can assert the read
	// path surfaces the decode error instead of silently discarding it.
	// Returns the row's id.
	SeedRawMessage func(t *testing.T, runID, column, rawJSON string) int64

	// AgentID returns an identifier suitable for the
	// StampAgentClaim agentID and the run row's actor_agent_id.
	// Backends use this to thread their own seeded agent row (the
	// Postgres path needs a real FK; SQLite is more relaxed).
	AgentID string
}

// RunConversationStoreConformance covers the agent-run contract every
// backend impl must hold:
//
//   - Lifecycle methods (Complete / SetSession / MarkOpen /
//     MarkQueuedForResume / MarkCancelledIfActive / MarkFailedIfActive)
//     refuse terminal statuses, produce correct side effects when they
//     accept, and release the conversation's active claim with the right
//     outcome.
//   - SetExecutorSystem mints/updates/releases the active claim, and the
//     claim-derived read fields (ClaimedAt / Attempts / ExecutorID) track
//     the claims rows.
//   - Queries return what they advertise (status filters, sort
//     orders, JOIN-derived projections).
//   - Transcript layer round-trips messages including JSONB
//     metadata + tool_calls + user/claim attribution, and
//     TokenTotalsSystem sums correctly.
//   - Memory_missing derivation matches the four noncompliance
//     forms (no row / NULL / "" / whitespace) + the populated
//     baseline.
//
// Cross-org leakage is Postgres-only and lives in the backend test
// file directly. The SQLite assertLocalOrg guard is also pinned in
// the backend test file.
func RunConversationStoreConformance(t *testing.T, mk ConversationStoreFactory) {
	t.Helper()

	t.Run("SeededRun_GetReturnsIt", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		got, err := store.Get(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got == nil || got.ID != runID {
			t.Fatalf("Get returned %v, want id=%s", got, runID)
		}
		if got.Status != "running" || got.Model != "m" {
			t.Errorf("Get fields drift: %+v", got)
		}
		if got.Attempts != 0 || got.ClaimedAt != nil || got.ExecutorID != "" {
			t.Errorf("never-claimed run carries claim state: attempts=%d claimedAt=%v executor=%q",
				got.Attempts, got.ClaimedAt, got.ExecutorID)
		}
	})

	t.Run("Get_ReturnsNilForMissingID", func(t *testing.T) {
		store, orgID, _, _ := mk(t)
		got, err := store.Get(context.Background(), orgID, uuid.New().String())
		if err != nil {
			t.Fatalf("Get missing: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("Complete_SettlesCostLumpAndClaimTelemetry", func(t *testing.T) {
		// Two invocation cycles (initial + resume). Each cycle's rows insert
		// while its claim is active, so they carry its id; each Complete then
		// settles its invocation's reported cost as ONE lump on its own
		// engagement's newest row and stamps its duration/turns onto the
		// claim it releases; the read projection then derives cost as the
		// ledger SUM and duration/turns as the claims SUM — so two cycles
		// ADD without any stored accumulator.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-cost", 1); err != nil {
			t.Fatalf("SetExecutorSystem 1: %v", err)
		}
		msg1, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "turn 1"})
		if err != nil {
			t.Fatalf("InsertMessage 1: %v", err)
		}
		if err := store.Complete(ctx, orgID, runID, "completed", 1.25, 4000, 3, "partial", "", "", "", ""); err != nil {
			t.Fatalf("first Complete: %v", err)
		}
		got, err := store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get after first: err=%v, got=%v", err, got)
		}
		if got.TotalCostUSD == nil || *got.TotalCostUSD != 1.25 {
			t.Errorf("total_cost_usd after first = %v, want 1.25", got.TotalCostUSD)
		}
		if got.DurationMs == nil || *got.DurationMs != 4000 {
			t.Errorf("duration_ms after first = %v, want 4000", got.DurationMs)
		}
		if got.NumTurns == nil || *got.NumTurns != 3 {
			t.Errorf("num_turns after first = %v, want 3", got.NumTurns)
		}

		// The resume invocation mints its own claim, records its own row,
		// then settles.
		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-cost", 2); err != nil {
			t.Fatalf("SetExecutorSystem 2: %v", err)
		}
		msg2, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "turn 2"})
		if err != nil {
			t.Fatalf("InsertMessage 2: %v", err)
		}
		if err := store.Complete(ctx, orgID, runID, "completed", 0.75, 2000, 5, "ok", "all done", "abort", "needs human", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		got, err = store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v, got=%v", err, got)
		}
		if got.Status != "completed" {
			t.Errorf("status = %q, want completed", got.Status)
		}
		if got.CompletedAt == nil {
			t.Errorf("completed_at not stamped")
		}
		if got.TotalCostUSD == nil || *got.TotalCostUSD != 2.0 {
			t.Errorf("total_cost_usd = %v, want 2.0 (lump 1.25 + lump 0.75 over the ledger)", got.TotalCostUSD)
		}
		if got.DurationMs == nil || *got.DurationMs != 6000 {
			t.Errorf("duration_ms = %v, want 6000 (claims telemetry SUM)", got.DurationMs)
		}
		if got.NumTurns == nil || *got.NumTurns != 8 {
			t.Errorf("num_turns = %v, want 8 (claims telemetry SUM)", got.NumTurns)
		}
		if got.StopReason != "ok" {
			t.Errorf("stop_reason = %q, want ok", got.StopReason)
		}
		if got.Outcome != "abort" {
			t.Errorf("outcome = %q, want abort", got.Outcome)
		}
		if got.OutcomeReason != "needs human" {
			t.Errorf("outcome_reason = %q, want \"needs human\"", got.OutcomeReason)
		}

		// The stamps land exactly where reported: one lump per invocation's
		// last row, no proration.
		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		costByID := map[int]*float64{}
		for i := range msgs {
			costByID[msgs[i].ID] = msgs[i].CostUSD
		}
		if c := costByID[int(msg1)]; c == nil || *c != 1.25 {
			t.Errorf("msg1 cost_usd = %v, want 1.25", c)
		}
		if c := costByID[int(msg2)]; c == nil || *c != 0.75 {
			t.Errorf("msg2 cost_usd = %v, want 0.75", c)
		}
	})

	t.Run("Complete_SettlesOnEngagementRow_SkipsForeignNewerRow", func(t *testing.T) {
		// The lump must land on the settling engagement's own newest row —
		// NOT on the conversation's newest row when that row belongs to a
		// different (already-released) claim.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		// Engagement 1 records a row and settles.
		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-a", 1); err != nil {
			t.Fatalf("SetExecutorSystem 1: %v", err)
		}
		claims := seed.ClaimRows(t, runID)
		if len(claims) != 1 {
			t.Fatalf("claims = %+v, want 1", claims)
		}
		claim1 := claims[0].ID
		msgA, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "a"})
		if err != nil {
			t.Fatalf("InsertMessage a: %v", err)
		}
		if err := store.Complete(ctx, orgID, runID, "completed", 1.25, 0, 0, "", "", "abort", "wait", ""); err != nil {
			t.Fatalf("first Complete: %v", err)
		}

		// Engagement 2 goes live and records its row; then a NEWER row
		// attributed (explicitly) to the released first claim lands.
		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-b", 2); err != nil {
			t.Fatalf("SetExecutorSystem 2: %v", err)
		}
		msgB, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "b"})
		if err != nil {
			t.Fatalf("InsertMessage b: %v", err)
		}
		msgC, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "c", ClaimID: claim1})
		if err != nil {
			t.Fatalf("InsertMessage c: %v", err)
		}
		if err := store.Complete(ctx, orgID, runID, "completed", 0.75, 0, 0, "", "", "finish", "", ""); err != nil {
			t.Fatalf("second Complete: %v", err)
		}

		got, err := store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.TotalCostUSD == nil || *got.TotalCostUSD != 2.0 {
			t.Errorf("total_cost_usd = %v, want 2.0 (one lump per engagement)", got.TotalCostUSD)
		}
		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		costByID := map[int]*float64{}
		for i := range msgs {
			costByID[msgs[i].ID] = msgs[i].CostUSD
		}
		if c := costByID[int(msgA)]; c == nil || *c != 1.25 {
			t.Errorf("engagement-1 row cost_usd = %v, want 1.25", c)
		}
		if c := costByID[int(msgB)]; c == nil || *c != 0.75 {
			t.Errorf("engagement-2 row cost_usd = %v, want 0.75 (the engagement's own MAX row)", c)
		}
		if c := costByID[int(msgC)]; c != nil {
			t.Errorf("foreign newer row cost_usd = %v, want nil (never the settle target of another engagement)", *c)
		}
	})

	t.Run("Complete_NoClaimAttributedRows_SettlesAdditivelyOnNewestRow", func(t *testing.T) {
		// An engagement can bill while recording no rows of its own (an
		// invocation bills for system-prompt / cache overhead even when
		// nothing parsed) — its lump settles ADDITIVELY onto the
		// conversation's newest existing message row, on top of any lump
		// already there.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		msg1, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "older"})
		if err != nil {
			t.Fatalf("InsertMessage 1: %v", err)
		}
		msg2, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "newest"})
		if err != nil {
			t.Fatalf("InsertMessage 2: %v", err)
		}
		// No active claim at all: the fallback owns the settle.
		if err := store.Complete(ctx, orgID, runID, "completed", 1.25, 0, 0, "", "", "finish", "", ""); err != nil {
			t.Fatalf("first Complete: %v", err)
		}
		// A live claim whose engagement recorded nothing (both rows predate
		// it) falls back the same way, added.
		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-rowless", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		if err := store.Complete(ctx, orgID, runID, "completed", 0.75, 0, 0, "", "", "finish", "", ""); err != nil {
			t.Fatalf("rowless-engagement Complete: %v", err)
		}
		got, err := store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.TotalCostUSD == nil || *got.TotalCostUSD != 2.0 {
			t.Errorf("total_cost_usd = %v, want 2.0 (1.25 fallback-added + 0.75 fallback-added)", got.TotalCostUSD)
		}
		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		costByID := map[int]*float64{}
		for i := range msgs {
			costByID[msgs[i].ID] = msgs[i].CostUSD
		}
		if c := costByID[int(msg1)]; c != nil {
			t.Errorf("older row cost_usd = %v, want nil (fallback must target only the newest row)", *c)
		}
		if c := costByID[int(msg2)]; c == nil || *c != 2.0 {
			t.Errorf("newest row cost_usd = %v, want 2.0 (additive settle)", c)
		}
	})

	t.Run("Complete_NoRows_DropsLumpWithoutError", func(t *testing.T) {
		// A conversation with no message rows at all is the one truly
		// unattributable case: Complete succeeds (even with a live claim —
		// both settle paths find nothing), writes nothing to the ledger,
		// and must not invent a row.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-norows", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		if err := store.Complete(ctx, orgID, runID, "completed", 9.99, 0, 0, "", "", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		got, err := store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.Status != "completed" {
			t.Errorf("status = %q, want completed", got.Status)
		}
		if got.TotalCostUSD != nil {
			t.Errorf("total_cost_usd = %v, want nil (no ledger row to settle on)", *got.TotalCostUSD)
		}
		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("messages = %d rows, want 0 (fallback must not mint rows)", len(msgs))
		}
	})

	t.Run("TokensDeriveFromMessagesLedger", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		// messages is the source of truth (written by the streaming sink as
		// messages arrive); the read projection SUMs it live — no terminal
		// write is needed for the totals to appear, and re-Completing can't
		// double them. Two assistant rows so the SUM is non-trivial.
		ptr := func(n int) *int { return &n }
		for _, m := range []*domain.Message{
			{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "a",
				InputTokens: ptr(100), OutputTokens: ptr(20), CacheReadTokens: ptr(1000), CacheCreationTokens: ptr(7)},
			{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "b",
				InputTokens: ptr(50), OutputTokens: ptr(5), CacheReadTokens: ptr(500), CacheCreationTokens: ptr(3)},
		} {
			if _, err := store.InsertMessage(ctx, orgID, m); err != nil {
				t.Fatalf("InsertMessage: %v", err)
			}
		}

		got, err := store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.InputTokens != 150 || got.OutputTokens != 25 || got.CacheReadTokens != 1500 || got.CacheCreationTokens != 10 {
			t.Errorf("token cols = (%d,%d,%d,%d), want (150,25,1500,10) — SUM over messages",
				got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheCreationTokens)
		}

		if err := store.Complete(ctx, orgID, runID, "completed", 0, 0, 0, "ok", "done", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		got2, err := store.Get(ctx, orgID, runID)
		if err != nil || got2 == nil {
			t.Fatalf("Get after Complete: err=%v got=%v", err, got2)
		}
		if got2.InputTokens != 150 || got2.OutputTokens != 25 || got2.CacheReadTokens != 1500 || got2.CacheCreationTokens != 10 {
			t.Errorf("after Complete token cols = (%d,%d,%d,%d), want unchanged (150,25,1500,10) — the ledger is the only source",
				got2.InputTokens, got2.OutputTokens, got2.CacheReadTokens, got2.CacheCreationTokens)
		}
	})

	// Tokens derive from the messages ledger at read time, so a run that
	// ends by cancel or infra-failure still reflects the tokens it streamed
	// with no terminal roll-up at all.
	t.Run("CancelAndFail_TokensStillDeriveFromLedger", func(t *testing.T) {
		ptr := func(n int) *int { return &n }
		seedRunningWithTokens := func(t *testing.T, store db.ConversationStore, orgID string, seed ConversationSeeder) string {
			t.Helper()
			ctx := context.Background()
			runID := seedConversationForTest(t, orgID, seed, "running")
			for _, m := range []*domain.Message{
				{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "a",
					InputTokens: ptr(100), OutputTokens: ptr(20), CacheReadTokens: ptr(1000), CacheCreationTokens: ptr(7)},
				{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "b",
					InputTokens: ptr(50), OutputTokens: ptr(5), CacheReadTokens: ptr(500), CacheCreationTokens: ptr(3)},
			} {
				if _, err := store.InsertMessage(ctx, orgID, m); err != nil {
					t.Fatalf("InsertMessage: %v", err)
				}
			}
			return runID
		}
		assertRolledUp := func(t *testing.T, store db.ConversationStore, orgID, runID string) {
			t.Helper()
			got, err := store.Get(context.Background(), orgID, runID)
			if err != nil || got == nil {
				t.Fatalf("Get: err=%v got=%v", err, got)
			}
			if got.InputTokens != 150 || got.OutputTokens != 25 || got.CacheReadTokens != 1500 || got.CacheCreationTokens != 10 {
				t.Errorf("token cols = (%d,%d,%d,%d), want (150,25,1500,10) — ledger SUM must survive the terminal write",
					got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheCreationTokens)
			}
		}

		t.Run("MarkCancelledIfActive", func(t *testing.T) {
			store, orgID, _, seed := mk(t)
			runID := seedRunningWithTokens(t, store, orgID, seed)
			ok, err := store.MarkCancelledIfActive(context.Background(), orgID, runID, "user_cancelled", "cancelled")
			if err != nil || !ok {
				t.Fatalf("MarkCancelledIfActive: ok=%v err=%v", ok, err)
			}
			assertRolledUp(t, store, orgID, runID)
		})

		t.Run("MarkFailedIfActive", func(t *testing.T) {
			store, orgID, _, seed := mk(t)
			runID := seedRunningWithTokens(t, store, orgID, seed)
			ok, err := store.MarkFailedIfActive(context.Background(), orgID, runID, "")
			if err != nil || !ok {
				t.Fatalf("MarkFailedIfActive: ok=%v err=%v", ok, err)
			}
			assertRolledUp(t, store, orgID, runID)
		})
	})

	t.Run("MarkFailedIfActive_PersistsFailureKind", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		// Kind supplied → hydrated back typed on Get.
		runID := seedConversationForTest(t, orgID, seed, "running")
		ok, err := store.MarkFailedIfActive(ctx, orgID, runID, string(domain.RunFailureMemoryLimit))
		if err != nil || !ok {
			t.Fatalf("MarkFailedIfActive with kind: ok=%v err=%v", ok, err)
		}
		got, err := store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get: run=%v err=%v", got, err)
		}
		if got.FailureKind != domain.RunFailureMemoryLimit {
			t.Errorf("FailureKind = %q, want %q", got.FailureKind, domain.RunFailureMemoryLimit)
		}

		// Empty kind → NULL → hydrates as the unclassified zero value.
		plainID := seedConversationForTest(t, orgID, seed, "running")
		if ok, err := store.MarkFailedIfActive(ctx, orgID, plainID, ""); err != nil || !ok {
			t.Fatalf("MarkFailedIfActive without kind: ok=%v err=%v", ok, err)
		}
		if got, _ := store.Get(ctx, orgID, plainID); got == nil || got.FailureKind != domain.RunFailureUnclassified {
			t.Errorf("FailureKind on unclassified failure = %q, want empty", got.FailureKind)
		}
	})

	t.Run("Complete_PersistsFailureKind", func(t *testing.T) {
		// processCompletion writes failed-status kinds (agent_error,
		// no_result) via Complete rather than MarkFailedIfActive — this
		// pins that write path independently so a placeholder/order
		// regression in either dialect's UPDATE can't silently drop the
		// column on the more common failure route.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		runID := seedConversationForTest(t, orgID, seed, "running")
		if err := store.Complete(ctx, orgID, runID, "failed", 0, 0, 0, "", "", "", "", string(domain.RunFailureAgentError)); err != nil {
			t.Fatalf("Complete with kind: %v", err)
		}
		got, err := store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get: run=%v err=%v", got, err)
		}
		if got.Status != "failed" {
			t.Errorf("status = %q, want failed", got.Status)
		}
		if got.FailureKind != domain.RunFailureAgentError {
			t.Errorf("FailureKind = %q, want %q", got.FailureKind, domain.RunFailureAgentError)
		}

		// Empty kind on a failed Complete → NULL → unclassified zero value.
		plainID := seedConversationForTest(t, orgID, seed, "running")
		if err := store.Complete(ctx, orgID, plainID, "failed", 0, 0, 0, "", "", "", "", ""); err != nil {
			t.Fatalf("Complete without kind: %v", err)
		}
		if got, _ := store.Get(ctx, orgID, plainID); got == nil || got.FailureKind != domain.RunFailureUnclassified {
			t.Errorf("FailureKind on unclassified Complete failure = %q, want empty", got.FailureKind)
		}
	})

	t.Run("SetSession_PersistsSessionID", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		if err := store.SetSession(ctx, orgID, runID, "sess-abc"); err != nil {
			t.Fatalf("SetSession: %v", err)
		}
		got, _ := store.Get(ctx, orgID, runID)
		if got == nil || got.SessionID != "sess-abc" {
			t.Errorf("sdk_session_id = %q, want sess-abc", got.SessionID)
		}
	})

	t.Run("MarkOpen_FlipsRunning_RefusesTerminal", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		// Running → ok=true.
		runID := seedConversationForTest(t, orgID, seed, "running")
		ok, err := store.MarkOpen(ctx, orgID, runID)
		if err != nil || !ok {
			t.Fatalf("MarkOpen on running: ok=%v err=%v", ok, err)
		}
		got, _ := store.Get(ctx, orgID, runID)
		if got.Status != "open" {
			t.Errorf("status = %q, want open", got.Status)
		}
		// Already open → ok=false (terminal-list excludes open, so an
		// idempotent re-call refuses).
		ok, err = store.MarkOpen(ctx, orgID, runID)
		if err != nil || ok {
			t.Errorf("re-call on open: ok=%v err=%v, want false/nil", ok, err)
		}
		// Terminal → ok=false.
		runID2 := seedConversationForTest(t, orgID, seed, "completed")
		ok, err = store.MarkOpen(ctx, orgID, runID2)
		if err != nil || ok {
			t.Errorf("on completed: ok=%v err=%v, want false/nil", ok, err)
		}
	})

	t.Run("MarkQueuedForResume_CASGuard", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		// running → refused (only parked/abort states are resumable).
		runID := seedConversationForTest(t, orgID, seed, "running")
		if ok, err := store.MarkQueuedForResume(ctx, orgID, runID); err != nil || ok {
			t.Errorf("on running: ok=%v err=%v, want false/nil", ok, err)
		}
		// open → ok, flips to queued. This flip must NOT stamp resume-time
		// ownership — the row goes back through ClaimNextRun, which mints
		// the claim for whichever executor actually claims it.
		if _, err := store.MarkOpen(ctx, orgID, runID); err != nil {
			t.Fatalf("open: %v", err)
		}
		if ok, err := store.MarkQueuedForResume(ctx, orgID, runID); err != nil || !ok {
			t.Fatalf("from open: ok=%v err=%v, want true", ok, err)
		}
		run, err := store.GetSystem(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("get after requeue: %v", err)
		}
		if run.Status != "queued" {
			t.Errorf("status = %q, want queued", run.Status)
		}
		if run.ExecutorID != "" {
			t.Errorf("queued row carries an owner: executor_id=%q, want empty", run.ExecutorID)
		}
		// The CAS loser: a second flip on the now-queued row finds no
		// resumable state and refuses — this is the guard that makes two
		// concurrent ResumeOpenRun calls resolve to exactly one requeue
		// (the loser surfaces ErrRunNotResumable at the delegate layer).
		if ok, _ := store.MarkQueuedForResume(ctx, orgID, runID); ok {
			t.Errorf("second flip on a queued row succeeded; want refused (CAS loser)")
		}

		// completed + outcome=abort → ok (message-resumable).
		abortRun := seedConversationForTest(t, orgID, seed, "running")
		if err := store.Complete(ctx, orgID, abortRun, "completed", 0, 0, 0, "", "stopped", "abort", "needs a human", ""); err != nil {
			t.Fatalf("complete+abort: %v", err)
		}
		if ok, err := store.MarkQueuedForResume(ctx, orgID, abortRun); err != nil || !ok {
			t.Errorf("from completed+abort: ok=%v err=%v, want true", ok, err)
		}
		// completed + outcome=finish → refused (finish excluded).
		finishRun := seedConversationForTest(t, orgID, seed, "running")
		if err := store.Complete(ctx, orgID, finishRun, "completed", 0, 0, 0, "", "shipped", "finish", "", ""); err != nil {
			t.Fatalf("complete+finish: %v", err)
		}
		if ok, _ := store.MarkQueuedForResume(ctx, orgID, finishRun); ok {
			t.Errorf("from completed+finish succeeded; want refused")
		}
	})

	t.Run("MarkFailedIfActive_FailsOpen_RefusesTerminalAndPendingApproval", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		// `open` is intentionally failable (a warm open run has no durable
		// snapshot, so an infra error reaching failRun must terminate it).
		openRun := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.MarkOpen(ctx, orgID, openRun); err != nil {
			t.Fatalf("open: %v", err)
		}
		ok, err := store.MarkFailedIfActive(ctx, orgID, openRun, "")
		if err != nil || !ok {
			t.Fatalf("MarkFailedIfActive on open: ok=%v err=%v, want true (open is failable)", ok, err)
		}
		if got, _ := store.Get(ctx, orgID, openRun); got.Status != "failed" {
			t.Errorf("status = %q, want failed", got.Status)
		}
		// pending_approval is protected (it has a durable snapshot) → refused.
		paRun := seedConversationForTest(t, orgID, seed, "pending_approval")
		if ok, _ := store.MarkFailedIfActive(ctx, orgID, paRun, ""); ok {
			t.Errorf("MarkFailedIfActive flipped a pending_approval run; want refused")
		}
		// Already terminal → refused.
		doneRun := seedConversationForTest(t, orgID, seed, "completed")
		if ok, _ := store.MarkFailedIfActive(ctx, orgID, doneRun, ""); ok {
			t.Errorf("MarkFailedIfActive flipped a completed run; want refused")
		}
	})

	t.Run("MarkCancelledIfActive_FlipsActive_RefusesTerminal", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		ok, err := store.MarkCancelledIfActive(ctx, orgID, runID, "manual", "user cancelled")
		if err != nil || !ok {
			t.Fatalf("cancel active: ok=%v err=%v", ok, err)
		}
		got, _ := store.Get(ctx, orgID, runID)
		if got.Status != "cancelled" || got.StopReason != "manual" {
			t.Errorf("after cancel: status=%q stop_reason=%q", got.Status, got.StopReason)
		}
		// Already terminal → refused.
		ok, err = store.MarkCancelledIfActive(ctx, orgID, runID, "manual", "")
		if err != nil || ok {
			t.Errorf("re-cancel: ok=%v err=%v, want false/nil", ok, err)
		}
	})

	t.Run("SetExecutorSystem_MintsUpdatesReleasesActiveClaim", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		// No claim yet → the go-live confirmation mints one.
		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-a", 3); err != nil {
			t.Fatalf("SetExecutorSystem mint: %v", err)
		}
		claims := seed.ClaimRows(t, runID)
		if len(claims) != 1 || claims[0].ExecutorID != "exec-a" || claims[0].BootEpoch != 3 || claims[0].Released {
			t.Fatalf("after mint: claims = %+v, want one active (exec-a, 3)", claims)
		}

		// A live claim → idempotent identity update, no second row.
		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-a", 4); err != nil {
			t.Fatalf("SetExecutorSystem update: %v", err)
		}
		claims = seed.ClaimRows(t, runID)
		if len(claims) != 1 || claims[0].BootEpoch != 4 || claims[0].Released {
			t.Fatalf("after update: claims = %+v, want the same single active claim at epoch 4", claims)
		}
		if got, _ := store.Get(ctx, orgID, runID); got.ExecutorID != "exec-a" || got.Attempts != 1 {
			t.Errorf("Get after update: executor=%q attempts=%d, want exec-a/1", got.ExecutorID, got.Attempts)
		}

		// Empty executorID → the legacy clear: the active claim releases
		// as requeued and the read-side owner goes empty.
		if err := store.SetExecutorSystem(ctx, orgID, runID, "", 0); err != nil {
			t.Fatalf("SetExecutorSystem clear: %v", err)
		}
		claims = seed.ClaimRows(t, runID)
		if len(claims) != 1 || !claims[0].Released || claims[0].Outcome != "requeued" {
			t.Fatalf("after clear: claims = %+v, want one released claim with outcome requeued", claims)
		}
		got, _ := store.Get(ctx, orgID, runID)
		if got.ExecutorID != "" {
			t.Errorf("ExecutorID after release = %q, want empty", got.ExecutorID)
		}
		if got.Attempts != 1 {
			t.Errorf("Attempts after release = %d, want 1 (released claims still count)", got.Attempts)
		}
		if got.ClaimedAt == nil {
			t.Errorf("ClaimedAt after release = nil, want the released claim's claimed_at")
		}

		// A re-mint after release is a NEW claim — attempts becomes the
		// claim count, and one-active-claim holds (the released row stays).
		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-b", 5); err != nil {
			t.Fatalf("SetExecutorSystem re-mint: %v", err)
		}
		claims = seed.ClaimRows(t, runID)
		if len(claims) != 2 {
			t.Fatalf("after re-mint: %d claims, want 2", len(claims))
		}
		active := 0
		for _, c := range claims {
			if !c.Released {
				active++
			}
		}
		if active != 1 {
			t.Errorf("active claims = %d, want exactly 1", active)
		}
		if got, _ := store.Get(ctx, orgID, runID); got.ExecutorID != "exec-b" || got.Attempts != 2 {
			t.Errorf("Get after re-mint: executor=%q attempts=%d, want exec-b/2", got.ExecutorID, got.Attempts)
		}
	})

	t.Run("TerminalWrites_ReleaseActiveClaimWithOutcome", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		assertReleased := func(t *testing.T, runID, wantOutcome string) {
			t.Helper()
			claims := seed.ClaimRows(t, runID)
			if len(claims) != 1 {
				t.Fatalf("claims = %+v, want exactly 1", claims)
			}
			if !claims[0].Released || claims[0].Outcome != wantOutcome {
				t.Errorf("claim = %+v, want released with outcome %q", claims[0], wantOutcome)
			}
			if got, _ := store.Get(ctx, orgID, runID); got.ExecutorID != "" {
				t.Errorf("ExecutorID after release = %q, want empty", got.ExecutorID)
			}
		}
		stage := func(t *testing.T) string {
			t.Helper()
			runID := seedConversationForTest(t, orgID, seed, "running")
			if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-term", 1); err != nil {
				t.Fatalf("SetExecutorSystem: %v", err)
			}
			return runID
		}

		t.Run("Complete_completed", func(t *testing.T) {
			runID := stage(t)
			if err := store.Complete(ctx, orgID, runID, "completed", 0, 0, 0, "", "", "finish", "", ""); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			assertReleased(t, runID, "completed")
		})
		t.Run("Complete_failed", func(t *testing.T) {
			runID := stage(t)
			if err := store.Complete(ctx, orgID, runID, "failed", 0, 0, 0, "", "", "", "", ""); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			assertReleased(t, runID, "failed")
		})
		t.Run("MarkCancelledIfActive", func(t *testing.T) {
			runID := stage(t)
			if ok, err := store.MarkCancelledIfActive(ctx, orgID, runID, "manual", ""); err != nil || !ok {
				t.Fatalf("MarkCancelledIfActive: ok=%v err=%v", ok, err)
			}
			assertReleased(t, runID, "cancelled")
		})
		t.Run("MarkFailedIfActive", func(t *testing.T) {
			runID := stage(t)
			if ok, err := store.MarkFailedIfActive(ctx, orgID, runID, ""); err != nil || !ok {
				t.Fatalf("MarkFailedIfActive: ok=%v err=%v", ok, err)
			}
			assertReleased(t, runID, "failed")
		})
		t.Run("MarkOpen_parks", func(t *testing.T) {
			runID := stage(t)
			if ok, err := store.MarkOpen(ctx, orgID, runID); err != nil || !ok {
				t.Fatalf("MarkOpen: ok=%v err=%v", ok, err)
			}
			assertReleased(t, runID, "parked")
		})
		t.Run("MarkQueuedForResume_requeues", func(t *testing.T) {
			runID := stage(t)
			if ok, err := store.MarkOpen(ctx, orgID, runID); err != nil || !ok {
				t.Fatalf("MarkOpen: ok=%v err=%v", ok, err)
			}
			// MarkOpen already released as 'parked'; re-mint so the requeue
			// has an active claim to release.
			if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-park", 2); err != nil {
				t.Fatalf("SetExecutorSystem re-mint: %v", err)
			}
			if ok, err := store.MarkQueuedForResume(ctx, orgID, runID); err != nil || !ok {
				t.Fatalf("MarkQueuedForResume: ok=%v err=%v", ok, err)
			}
			claims := seed.ClaimRows(t, runID)
			if len(claims) != 2 {
				t.Fatalf("claims = %+v, want 2 (parked, then requeued)", claims)
			}
			last := claims[len(claims)-1]
			if !last.Released || last.Outcome != "requeued" {
				t.Errorf("latest claim = %+v, want released with outcome requeued", last)
			}
		})
		t.Run("RefusedWrite_LeavesClaimActive", func(t *testing.T) {
			// A guarded no-op must not release: complete the run, re-mint a
			// claim (simulating a racing engagement), then fail — the guard
			// refuses and the claim stays live.
			runID := stage(t)
			if err := store.Complete(ctx, orgID, runID, "completed", 0, 0, 0, "", "", "finish", "", ""); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-race", 9); err != nil {
				t.Fatalf("SetExecutorSystem: %v", err)
			}
			if ok, _ := store.MarkFailedIfActive(ctx, orgID, runID, ""); ok {
				t.Fatalf("MarkFailedIfActive on completed run succeeded; want refused")
			}
			claims := seed.ClaimRows(t, runID)
			last := claims[len(claims)-1]
			if last.Released {
				t.Errorf("refused terminal write released the claim: %+v", last)
			}
		})
	})

	t.Run("SetActiveClaimPhaseSystem_SetClearAndCoalesceIntoStatus", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-phase", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		// Set: the phase lands on the active claim and the display read
		// coalesces it over the stored 'running'.
		if err := store.SetActiveClaimPhaseSystem(ctx, orgID, runID, "cloning"); err != nil {
			t.Fatalf("SetActiveClaimPhaseSystem set: %v", err)
		}
		claims := seed.ClaimRows(t, runID)
		if len(claims) != 1 || claims[0].Phase != "cloning" {
			t.Fatalf("claims after set = %+v, want one active claim in phase cloning", claims)
		}
		got, err := store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.Status != "cloning" {
			t.Errorf("Status = %q, want cloning (active claim's phase coalesced over stored status)", got.Status)
		}

		// Clear: empty phase writes NULL and the display falls back to the
		// stored status.
		if err := store.SetActiveClaimPhaseSystem(ctx, orgID, runID, ""); err != nil {
			t.Fatalf("SetActiveClaimPhaseSystem clear: %v", err)
		}
		if claims := seed.ClaimRows(t, runID); len(claims) != 1 || claims[0].Phase != "" {
			t.Fatalf("claims after clear = %+v, want the phase cleared", claims)
		}
		if got, _ := store.Get(ctx, orgID, runID); got.Status != "running" {
			t.Errorf("Status after clear = %q, want running", got.Status)
		}
	})

	t.Run("SetActiveClaimPhaseSystem_NoOpOnReleasedClaim", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-phase-rel", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		if err := store.SetActiveClaimPhaseSystem(ctx, orgID, runID, "agent_starting"); err != nil {
			t.Fatalf("SetActiveClaimPhaseSystem: %v", err)
		}
		if err := store.Complete(ctx, orgID, runID, "completed", 0, 0, 0, "", "", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		// A write against the released claim is a silent no-op: the released
		// claim's phase stays whatever it held at release.
		if err := store.SetActiveClaimPhaseSystem(ctx, orgID, runID, "cloning"); err != nil {
			t.Fatalf("SetActiveClaimPhaseSystem on released: %v", err)
		}
		claims := seed.ClaimRows(t, runID)
		if len(claims) != 1 || !claims[0].Released || claims[0].Phase != "agent_starting" {
			t.Fatalf("claims = %+v, want one released claim still in phase agent_starting", claims)
		}
		// And the released claim's phase never leaks into the display: the
		// coalesce only reads the ACTIVE claim.
		if got, _ := store.Get(ctx, orgID, runID); got.Status != "completed" {
			t.Errorf("Status = %q, want completed (a released claim's phase is inert history)", got.Status)
		}
	})

	t.Run("ClaimDerivedFields_TrackLatestClaim", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		// First engagement.
		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-1", 1); err != nil {
			t.Fatalf("SetExecutorSystem 1: %v", err)
		}
		got, err := store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.ExecutorID != "exec-1" || got.Attempts != 1 || got.ClaimedAt == nil {
			t.Fatalf("first engagement: executor=%q attempts=%d claimedAt=%v", got.ExecutorID, got.Attempts, got.ClaimedAt)
		}
		firstClaimedAt := *got.ClaimedAt

		// Park (releases), then a second engagement — ClaimedAt advances to
		// the newest claim, Attempts counts both, ExecutorID is the live one.
		if ok, err := store.MarkOpen(ctx, orgID, runID); err != nil || !ok {
			t.Fatalf("MarkOpen: ok=%v err=%v", ok, err)
		}
		time.Sleep(1100 * time.Millisecond) // SQLite's second-granularity timestamps
		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-2", 2); err != nil {
			t.Fatalf("SetExecutorSystem 2: %v", err)
		}
		got, err = store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get 2: err=%v got=%v", err, got)
		}
		if got.ExecutorID != "exec-2" || got.Attempts != 2 {
			t.Errorf("second engagement: executor=%q attempts=%d, want exec-2/2", got.ExecutorID, got.Attempts)
		}
		if got.ClaimedAt == nil || !got.ClaimedAt.After(firstClaimedAt) {
			t.Errorf("ClaimedAt = %v, want later than the first claim's %v", got.ClaimedAt, firstClaimedAt)
		}
	})

	t.Run("ListForTask_OrderedByStartedAtDesc", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "list")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		// Two runs with a >1s sleep — SQLite's CURRENT_TIMESTAMP
		// default has 1-second granularity, so the gap is needed
		// for ORDER BY to discriminate. Two runs is enough to pin
		// "newest first"; three would risk landing in the same
		// second slot without making the assertion stronger.
		first := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		time.Sleep(1100 * time.Millisecond)
		second := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		runs, err := store.ListForTask(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("ListForTask: %v", err)
		}
		if len(runs) != 2 {
			t.Fatalf("len = %d, want 2", len(runs))
		}
		if runs[0].ID != second || runs[1].ID != first {
			t.Errorf("order = [%s, %s], want [%s, %s] (newest first)",
				runs[0].ID, runs[1].ID, second, first)
		}
	})

	t.Run("ListForTask_PreservesTriggerType", func(t *testing.T) {
		// The projection must round-trip trigger_type — when the column was
		// missing, every caller saw "" and the resume goroutine treated
		// event runs as manual on resume. Cover both branches across a
		// mixed list.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "list-trigger")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		manualID := seed.Run(t, domain.Conversation{
			TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", TriggerType: "manual",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})
		eventID := seed.Run(t, domain.Conversation{
			TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", TriggerType: "event",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})
		runs, err := store.ListForTask(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("ListForTask: %v", err)
		}
		gotByID := make(map[string]string, len(runs))
		for _, r := range runs {
			gotByID[r.ID] = r.TriggerType
		}
		if gotByID[manualID] != "manual" {
			t.Errorf("manual run TriggerType = %q, want manual", gotByID[manualID])
		}
		if gotByID[eventID] != "event" {
			t.Errorf("event run TriggerType = %q, want event", gotByID[eventID])
		}
	})

	t.Run("ListForTasks_BatchedAcrossTasks", func(t *testing.T) {
		// The batched twin of ListForTask: one query returns runs for
		// many tasks, each attributed to its own TaskID, with unknown
		// ids contributing nothing and empty input a no-op. Backs the
		// Board's aggregated agent-run fetch.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		if runs, err := store.ListForTasks(ctx, orgID, nil); err != nil || runs != nil {
			t.Fatalf("ListForTasks(nil) = (%v, %v), want (nil, nil)", runs, err)
		}

		entA := seed.Entity(t, "lft-a")
		evA := seed.Event(t, entA, domain.EventGitHubPROpened)
		taskA := seed.Task(t, entA, domain.EventGitHubPROpened, evA)
		entB := seed.Entity(t, "lft-b")
		evB := seed.Event(t, entB, domain.EventGitHubPROpened)
		taskB := seed.Task(t, entB, domain.EventGitHubPROpened, evB)

		runA1 := seedConversationForTaskTest(t, orgID, taskA, "running", seed)
		runA2 := seedConversationForTaskTest(t, orgID, taskA, "completed", seed)
		runB1 := seedConversationForTaskTest(t, orgID, taskB, "running", seed)

		// Mix in a valid-but-absent UUID and a non-UUID literal: both must be
		// tolerated (no rows, no error). The non-UUID guards the Postgres path,
		// where a uuid[] bind would otherwise 22P02 — filtered per uuid.go.
		runs, err := store.ListForTasks(ctx, orgID, []string{taskA, taskB, uuid.New().String(), "not-a-uuid"})
		if err != nil {
			t.Fatalf("ListForTasks: %v", err)
		}
		byTask := map[string][]string{}
		for _, r := range runs {
			byTask[r.TaskID] = append(byTask[r.TaskID], r.ID)
		}
		if len(byTask[taskA]) != 2 {
			t.Errorf("task A: got %v, want 2 runs", byTask[taskA])
		}
		if len(byTask[taskB]) != 1 {
			t.Errorf("task B: got %v, want 1 run", byTask[taskB])
		}
		seen := map[string]bool{}
		for _, ids := range byTask {
			for _, id := range ids {
				seen[id] = true
			}
		}
		for _, want := range []string{runA1, runA2, runB1} {
			if !seen[want] {
				t.Errorf("run %s missing from batched result", want)
			}
		}
	})

	t.Run("ActiveIDsForTask_FiltersTerminal", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "ha")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		// No runs yet → empty.
		ids, _ := store.ActiveIDsForTask(ctx, orgID, taskID)
		if len(ids) != 0 {
			t.Errorf("ActiveIDs with no runs: %v, want []", ids)
		}
		// One running + one terminal → ids=[running].
		runRun := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		_ = seedConversationForTaskTest(t, orgID, taskID, "completed", seed)
		ids, _ = store.ActiveIDsForTask(ctx, orgID, taskID)
		if len(ids) != 1 || ids[0] != runRun {
			t.Errorf("ActiveIDs = %v, want [%s]", ids, runRun)
		}
	})

	t.Run("HasActiveAutoRunForEntity", func(t *testing.T) {
		// Per-entity gate: any non-terminal trigger_type='event' run on any
		// task that belongs to the entity. Manual delegations are excluded
		// (by design — manual decoupled from the auto-queue gate); terminal
		// runs don't count either.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "ha-ent")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)

		// No runs → false.
		if has, _ := store.HasActiveAutoRunForEntity(ctx, orgID, ent); has {
			t.Error("HasActiveAutoRunForEntity with no runs: true, want false")
		}

		// Manual run — must NOT trip the gate.
		_ = seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		if has, _ := store.HasActiveAutoRunForEntity(ctx, orgID, ent); has {
			t.Error("manual run tripped the auto-run gate; gate must be event-only")
		}

		// Add an active event-trigger run on the same task — gate flips true.
		eventRunID := seed.Run(t, domain.Conversation{
			TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", TriggerType: "event",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})
		if has, _ := store.HasActiveAutoRunForEntity(ctx, orgID, ent); !has {
			t.Error("active event-trigger run should trip the gate")
		}

		// Terminate the event run; only terminal event-trigger rows
		// remain plus the still-running manual — gate flips back to
		// false.
		if err := store.Complete(ctx, orgID, eventRunID, "completed", 0, 0, 0, "", "", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if has, _ := store.HasActiveAutoRunForEntity(ctx, orgID, ent); has {
			t.Error("terminal event run + active manual should NOT trip the gate")
		}
	})

	t.Run("ActiveAutoRunIDForEntitySystem", func(t *testing.T) {
		// Same predicate as HasActiveAutoRunForEntity (non-terminal,
		// trigger_type='event'), but returns the run ID plus the ID of
		// the task the run belongs to instead of a bool — the router's
		// additive-injection branch needs the run ID to target
		// StageOrDeliverInjection, and the absorption rule needs the
		// task ID to confirm the run belongs to the firing's own task.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "ha-id-ent")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)

		// No runs → ("", "").
		if id, tid, err := store.ActiveAutoRunIDForEntitySystem(ctx, orgID, ent); err != nil || id != "" || tid != "" {
			t.Errorf("with no runs: id=%q taskID=%q err=%v, want empty/empty/nil", id, tid, err)
		}

		// Manual run only — must NOT resolve.
		_ = seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		if id, tid, err := store.ActiveAutoRunIDForEntitySystem(ctx, orgID, ent); err != nil || id != "" || tid != "" {
			t.Errorf("manual-only: id=%q taskID=%q err=%v, want empty (event-only)", id, tid, err)
		}

		// Active event-trigger run → resolves to its run ID and owning task ID.
		eventRunID := seed.Run(t, domain.Conversation{
			TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", TriggerType: "event",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})
		if id, tid, err := store.ActiveAutoRunIDForEntitySystem(ctx, orgID, ent); err != nil || id != eventRunID || tid != taskID {
			t.Errorf("ActiveAutoRunIDForEntitySystem = (%q, %q) err=%v, want (%q, %q)", id, tid, err, eventRunID, taskID)
		}

		// Terminate it — terminal-only, plus the still-active manual run,
		// resolves back to ("", "").
		if err := store.Complete(ctx, orgID, eventRunID, "completed", 0, 0, 0, "", "", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if id, tid, err := store.ActiveAutoRunIDForEntitySystem(ctx, orgID, ent); err != nil || id != "" || tid != "" {
			t.Errorf("terminal event run + active manual: id=%q taskID=%q err=%v, want empty", id, tid, err)
		}
	})

	t.Run("ListParkedWorktreePathsSystem_FiltersByStatusAndWorktree", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		// open WITH a worktree path → included.
		openRun := seedConversationForTest(t, orgID, seed, "open")
		if err := store.SetWorktreePath(ctx, orgID, openRun, "/tmp/triagefactory-runs/open"); err != nil {
			t.Fatalf("set worktree (open): %v", err)
		}
		// completed WITH a worktree → excluded by the status filter. A completed run
		// that left an unresolved artifact no longer parks, so its
		// worktree is not preserved as a warm resume cache.
		completed := seedConversationForTest(t, orgID, seed, "completed")
		if err := store.SetWorktreePath(ctx, orgID, completed, "/tmp/triagefactory-runs/completed"); err != nil {
			t.Fatalf("set worktree (completed): %v", err)
		}
		// open WITHOUT a worktree → excluded by the COALESCE filter.
		_ = seedConversationForTest(t, orgID, seed, "open")
		// running WITH a worktree → excluded by the status filter.
		running := seedConversationForTest(t, orgID, seed, "running")
		if err := store.SetWorktreePath(ctx, orgID, running, "/tmp/triagefactory-runs/running"); err != nil {
			t.Fatalf("set worktree (running): %v", err)
		}
		// open WITH a worktree but under an already-terminal blueprint_run →
		// excluded: a parked run under a terminal parent is not resumable, so its
		// worktree must not be preserved (else the boot reconcile orphans the row
		// and leaves its checked-out branch on disk).
		orphanTaskID := seed.Task(t, seed.Entity(t, "parked-orphan"), domain.EventGitHubPROpened,
			seed.Event(t, seed.Entity(t, "parked-orphan-ev"), domain.EventGitHubPROpened))
		orphanBR := seed.BlueprintRun(t, orphanTaskID)
		orphan := seed.Run(t, domain.Conversation{
			TaskID: orphanTaskID, PromptID: agentRunTestPrompt(t), Status: "open", Model: "m",
			BlueprintRunID: orphanBR,
		})
		if err := store.SetWorktreePath(ctx, orgID, orphan, "/tmp/triagefactory-runs/orphan"); err != nil {
			t.Fatalf("set worktree (orphan): %v", err)
		}
		seed.SetBlueprintRunStatus(t, orphanBR, "cancelled")

		paths, err := store.ListParkedWorktreePathsSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListParkedWorktreePathsSystem: %v", err)
		}
		got := map[string]bool{}
		for _, p := range paths {
			got[p] = true
		}
		if !got["/tmp/triagefactory-runs/open"] {
			t.Error("open worktree missing from ListParkedWorktreePathsSystem")
		}
		if got["/tmp/triagefactory-runs/completed"] {
			t.Error("completed worktree leaked — status filter failed (completed runs no longer park)")
		}
		if got["/tmp/triagefactory-runs/running"] {
			t.Error("running worktree leaked — status filter failed")
		}
		if got["/tmp/triagefactory-runs/orphan"] {
			t.Error("parked worktree under a terminal blueprint_run leaked — running-parent filter failed")
		}
		if len(got) != 1 {
			t.Errorf("got %d parked paths, want 1 (%v)", len(got), paths)
		}
	})

	t.Run("EntitiesWithOpenRuns_EmptyInputFastPath", func(t *testing.T) {
		store, orgID, _, _ := mk(t)
		got, err := store.EntitiesWithOpenRuns(context.Background(), orgID, nil)
		if err != nil {
			t.Fatalf("nil: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("nil input: %d entries, want 0", len(got))
		}
		got, err = store.EntitiesWithOpenRuns(context.Background(), orgID, []string{})
		if err != nil {
			t.Fatalf("empty: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("empty input: %d entries, want 0", len(got))
		}
	})

	t.Run("EntitiesWithOpenRuns_FiltersByStatus", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		entA := seed.Entity(t, "ewa-a")
		entB := seed.Entity(t, "ewa-b")
		evA := seed.Event(t, entA, domain.EventGitHubPROpened)
		evB := seed.Event(t, entB, domain.EventGitHubPROpened)
		taskA := seed.Task(t, entA, domain.EventGitHubPROpened, evA)
		taskB := seed.Task(t, entB, domain.EventGitHubPROpened, evB)

		// A has an open run; B has only a running run.
		runA := seedConversationForTaskTest(t, orgID, taskA, "running", seed)
		if _, err := store.MarkOpen(ctx, orgID, runA); err != nil {
			t.Fatalf("open A: %v", err)
		}
		_ = seedConversationForTaskTest(t, orgID, taskB, "running", seed)

		got, err := store.EntitiesWithOpenRuns(ctx, orgID, []string{entA, entB})
		if err != nil {
			t.Fatalf("EntitiesWithOpenRuns: %v", err)
		}
		if _, ok := got[entA]; !ok {
			t.Errorf("entA missing from open set")
		}
		if _, ok := got[entB]; ok {
			t.Errorf("entB leaked — only entA has an open run")
		}
	})

	t.Run("InsertMessage_StampsCreatedAtAndReturnsID", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		msg := &domain.Message{
			ConversationID: runID,
			Role:           "assistant",
			Content:        "hello",
			Subtype:        "text",
		}
		id, err := store.InsertMessage(ctx, orgID, msg)
		if err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}
		if id <= 0 {
			t.Errorf("returned id = %d, want > 0", id)
		}
		if msg.CreatedAt.IsZero() {
			t.Errorf("CreatedAt not stamped")
		}
	})

	t.Run("InsertMessage_PreservesExplicitCreatedAt", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		explicit := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		msg := &domain.Message{
			ConversationID: runID, Role: "assistant", Content: "x", Subtype: "text",
			CreatedAt: explicit,
		}
		if _, err := store.InsertMessage(ctx, orgID, msg); err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}
		if !msg.CreatedAt.Equal(explicit) {
			t.Errorf("CreatedAt rewritten: got %v, want %v", msg.CreatedAt, explicit)
		}
	})

	t.Run("Messages_RoundTripsToolCallsAndMetadata", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		msg := &domain.Message{
			ConversationID: runID,
			Role:           "assistant",
			Content:        "calling tool",
			Subtype:        "tool_use",
			ToolCalls: []domain.ToolCall{
				{ID: "call-1", Name: "Edit", Input: map[string]any{"path": "foo.go"}},
			},
			Metadata: map[string]any{"k": "v"},
		}
		if _, err := store.InsertMessage(ctx, orgID, msg); err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}
		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("len = %d, want 1", len(msgs))
		}
		got := msgs[0]
		if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "call-1" {
			t.Errorf("ToolCalls round-trip: %+v", got.ToolCalls)
		}
		if got.Metadata["k"] != "v" {
			t.Errorf("Metadata round-trip: %+v", got.Metadata)
		}
	})

	t.Run("InsertMessage_RoundTripsUserAndClaimAttribution", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		// Mint a claim so ClaimID has a real FK target, then attribute a
		// user message to (user, claim).
		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-msg", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claims := seed.ClaimRows(t, runID)
		if len(claims) != 1 {
			t.Fatalf("claims = %+v, want 1", claims)
		}
		claimID := claims[0].ID

		attributed := &domain.Message{
			ConversationID: runID, Role: "user", Subtype: "text", Content: "steer",
			UserID: userID, ClaimID: claimID,
		}
		if _, err := store.InsertMessage(ctx, orgID, attributed); err != nil {
			t.Fatalf("InsertMessage attributed: %v", err)
		}
		// A system row written outside the engagement (claim released)
		// leaves both empty (NULL on the row).
		if err := store.SetExecutorSystem(ctx, orgID, runID, "", 0); err != nil {
			t.Fatalf("SetExecutorSystem release: %v", err)
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: runID, Role: "assistant", Subtype: "text", Content: "reply",
		}); err != nil {
			t.Fatalf("InsertMessage system: %v", err)
		}

		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		if len(msgs) != 2 {
			t.Fatalf("len = %d, want 2", len(msgs))
		}
		if msgs[0].UserID != userID || msgs[0].ClaimID != claimID {
			t.Errorf("attributed row = (user=%q, claim=%q), want (%q, %q)", msgs[0].UserID, msgs[0].ClaimID, userID, claimID)
		}
		if msgs[1].UserID != "" || msgs[1].ClaimID != "" {
			t.Errorf("system row = (user=%q, claim=%q), want empty/empty", msgs[1].UserID, msgs[1].ClaimID)
		}
	})

	t.Run("InsertMessage_StampsActiveClaimServerSide", func(t *testing.T) {
		// The live-write attribution contract: rows written during an
		// engagement belong to it (the active claim's id is stamped
		// server-side), rows written outside one resolve NULL, and an
		// explicit ClaimID always wins over the active claim.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		// Before any engagement: no active claim to attribute to.
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "pre"}); err != nil {
			t.Fatalf("InsertMessage pre-claim: %v", err)
		}

		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-stamp", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claims := seed.ClaimRows(t, runID)
		if len(claims) != 1 {
			t.Fatalf("claims = %+v, want 1", claims)
		}
		claim1 := claims[0].ID

		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "during"}); err != nil {
			t.Fatalf("InsertMessage during claim: %v", err)
		}

		// Released engagement: later rows must NOT inherit the dead claim.
		if err := store.SetExecutorSystem(ctx, orgID, runID, "", 0); err != nil {
			t.Fatalf("SetExecutorSystem release: %v", err)
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "post"}); err != nil {
			t.Fatalf("InsertMessage post-release: %v", err)
		}

		// A second engagement goes live; an explicit ClaimID naming the
		// released first claim (the bundle-import shape) beats the active
		// one.
		if err := store.SetExecutorSystem(ctx, orgID, runID, "exec-stamp", 2); err != nil {
			t.Fatalf("SetExecutorSystem 2: %v", err)
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "explicit", ClaimID: claim1}); err != nil {
			t.Fatalf("InsertMessage explicit: %v", err)
		}

		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		if len(msgs) != 4 {
			t.Fatalf("len = %d, want 4", len(msgs))
		}
		if msgs[0].ClaimID != "" {
			t.Errorf("pre-claim row claim = %q, want empty (no engagement to attribute to)", msgs[0].ClaimID)
		}
		if msgs[1].ClaimID != claim1 {
			t.Errorf("live row claim = %q, want the active claim %q", msgs[1].ClaimID, claim1)
		}
		if msgs[2].ClaimID != "" {
			t.Errorf("post-release row claim = %q, want empty (the engagement is over)", msgs[2].ClaimID)
		}
		if msgs[3].ClaimID != claim1 {
			t.Errorf("explicit row claim = %q, want the explicit %q (explicit wins over the active claim)", msgs[3].ClaimID, claim1)
		}
	})

	t.Run("InsertMessage_DefaultsDeliveredTrueAndWindowStateActive", func(t *testing.T) {
		// Every existing caller in the repo leaves Delivered nil and
		// WindowState "" — the columnar-canon columns must not change their
		// observed behavior: delivered=true, window_state='active', seq=NULL.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		msg := &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "hi"}
		if _, err := store.InsertMessage(ctx, orgID, msg); err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}
		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("len = %d, want 1", len(msgs))
		}
		got := msgs[0]
		if got.Delivered == nil || !*got.Delivered {
			t.Errorf("Delivered = %v, want true (the schema default)", got.Delivered)
		}
		if got.WindowState != domain.MessageWindowActive {
			t.Errorf("WindowState = %q, want %q (the schema default)", got.WindowState, domain.MessageWindowActive)
		}
		if got.Seq != nil {
			t.Errorf("Seq = %v, want nil (no backfill, no insert-time dance)", *got.Seq)
		}
	})

	t.Run("Messages_RoundTripsReasoningContentBlocksAndExplicitOverrides", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		delivered := false
		seq := 12.5
		msg := &domain.Message{
			ConversationID: runID,
			Role:           "assistant",
			Content:        "thinking then answering",
			Subtype:        "text",
			Reasoning: []domain.ReasoningDetail{
				{Index: 0, Type: "text", Text: "step one", Signature: "sig-abc"},
			},
			ContentBlocks: []domain.ContentBlock{
				{Type: domain.ContentBlockImage, ImageURL: &domain.ContentImageURL{URL: "https://example/img.png"}},
			},
			Delivered:   &delivered,
			WindowState: domain.MessageWindowElided,
			Seq:         &seq,
		}
		if _, err := store.InsertMessage(ctx, orgID, msg); err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}
		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("len = %d, want 1", len(msgs))
		}
		got := msgs[0]
		if len(got.Reasoning) != 1 || got.Reasoning[0].Signature != "sig-abc" || got.Reasoning[0].Text != "step one" {
			t.Errorf("Reasoning round-trip: %+v", got.Reasoning)
		}
		if len(got.ContentBlocks) != 1 || got.ContentBlocks[0].Type != domain.ContentBlockImage ||
			got.ContentBlocks[0].ImageURL == nil || got.ContentBlocks[0].ImageURL.URL != "https://example/img.png" {
			t.Errorf("ContentBlocks round-trip: %+v", got.ContentBlocks)
		}
		if got.Delivered == nil || *got.Delivered {
			t.Errorf("Delivered = %v, want false (explicit override)", got.Delivered)
		}
		if got.WindowState != domain.MessageWindowElided {
			t.Errorf("WindowState = %q, want %q (explicit override)", got.WindowState, domain.MessageWindowElided)
		}
		if got.Seq == nil || *got.Seq != 12.5 {
			t.Errorf("Seq = %v, want 12.5", got.Seq)
		}
	})

	t.Run("Messages_SurfacesReasoningDecodeError", func(t *testing.T) {
		// reasoning/content_blocks are canonical replay context (read via
		// ListForAssembly by a future native loop) — a decode failure must
		// return an error, not silently produce an empty slice that reads
		// identically to "no reasoning on this message".
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		seed.SeedRawMessage(t, runID, "reasoning", `{"wrong":"shape"}`)
		if _, err := store.Messages(ctx, orgID, runID); err == nil {
			t.Fatal("Messages: want a decode error for wrong-shaped reasoning JSON, got nil")
		}
		if _, err := store.ListForAssemblySystem(ctx, orgID, runID); err == nil {
			t.Fatal("ListForAssembly: want a decode error for wrong-shaped reasoning JSON, got nil")
		}
	})

	t.Run("Messages_SurfacesContentBlocksDecodeError", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		seed.SeedRawMessage(t, runID, "content_blocks", `{"wrong":"shape"}`)
		if _, err := store.Messages(ctx, orgID, runID); err == nil {
			t.Fatal("Messages: want a decode error for wrong-shaped content_blocks JSON, got nil")
		}
		if _, err := store.ListForAssemblySystem(ctx, orgID, runID); err == nil {
			t.Fatal("ListForAssembly: want a decode error for wrong-shaped content_blocks JSON, got nil")
		}
	})

	t.Run("ListForAssembly_OrdersByCoalesceSeqAndIncludesEverythingButInactive", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		idA, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "a"})
		if err != nil {
			t.Fatalf("InsertMessage a: %v", err)
		}
		idB, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "b"})
		if err != nil {
			t.Fatalf("InsertMessage b: %v", err)
		}
		// c's seq lands it between a (COALESCE→id a) and b (COALESCE→id b) —
		// the compaction-result insertion shape.
		seqC := float64(idA) + (float64(idB)-float64(idA))/2
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: runID, Role: "user", Subtype: "injection:compaction-result", Content: "c", Seq: &seqC,
		}); err != nil {
			t.Fatalf("InsertMessage c: %v", err)
		}
		delivered := false
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: runID, Role: "user", Subtype: "injection:steer", Content: "pending", Delivered: &delivered,
		}); err != nil {
			t.Fatalf("InsertMessage pending: %v", err)
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: runID, Role: "assistant", Subtype: "text", Content: "elided", WindowState: domain.MessageWindowElided,
		}); err != nil {
			t.Fatalf("InsertMessage elided: %v", err)
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: runID, Role: "assistant", Subtype: "text", Content: "inactive", WindowState: domain.MessageWindowInactive,
		}); err != nil {
			t.Fatalf("InsertMessage inactive: %v", err)
		}

		got, err := store.ListForAssemblySystem(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("ListForAssembly: %v", err)
		}
		var contents []string
		for _, m := range got {
			contents = append(contents, m.Content)
		}
		want := []string{"a", "c", "b", "pending", "elided"}
		if len(contents) != len(want) {
			t.Fatalf("contents = %v, want %v (inactive must be excluded)", contents, want)
		}
		for i := range want {
			if contents[i] != want[i] {
				t.Errorf("contents[%d] = %q, want %q — full: %v", i, contents[i], want[i], contents)
			}
		}
		// The pending row surfaces its undelivered state rather than being
		// silently filtered — the loop decides when to consume it.
		for _, m := range got {
			if m.Content == "pending" && (m.Delivered == nil || *m.Delivered) {
				t.Errorf("pending row Delivered = %v, want false", m.Delivered)
			}
		}
	})

	t.Run("Messages_HideWithdrawnPendingRows", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		undelivered := false
		inserts := []*domain.Message{
			{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "plain"},
			// Withdrawn-pending: staged, then withdrawn before any flush —
			// "never happened", so the display reads must hide it.
			{ConversationID: runID, Role: "user", Subtype: "injection:system-note", Content: "withdrawn",
				Delivered: &undelivered, WindowState: domain.MessageWindowInactive},
			// Delivered + inactive is compacted history: retired from
			// assembly but still part of the rendered transcript.
			{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "compacted",
				WindowState: domain.MessageWindowInactive},
			// A still-pending active row stays visible (it will happen).
			{ConversationID: runID, Role: "user", Subtype: "injection:system-note", Content: "pending",
				Delivered: &undelivered},
		}
		for _, m := range inserts {
			if _, err := store.InsertMessage(ctx, orgID, m); err != nil {
				t.Fatalf("InsertMessage %q: %v", m.Content, err)
			}
		}

		wantVisible := []string{"plain", "compacted", "pending"}
		assertContents := func(desc string, msgs []domain.Message) {
			t.Helper()
			var contents []string
			for _, m := range msgs {
				contents = append(contents, m.Content)
			}
			if len(contents) != len(wantVisible) {
				t.Fatalf("%s = %v, want %v (withdrawn-pending hidden, compacted visible)", desc, contents, wantVisible)
			}
			for i := range wantVisible {
				if contents[i] != wantVisible[i] {
					t.Errorf("%s[%d] = %q, want %q", desc, i, contents[i], wantVisible[i])
				}
			}
		}

		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		assertContents("Messages", msgs)

		batched, err := store.MessagesForRuns(ctx, orgID, []string{runID})
		if err != nil {
			t.Fatalf("MessagesForRuns: %v", err)
		}
		assertContents("MessagesForRuns", batched)

		// Assembly excludes every inactive row — withdrawn AND compacted.
		asm, err := store.ListForAssemblySystem(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("ListForAssembly: %v", err)
		}
		var asmContents []string
		for _, m := range asm {
			asmContents = append(asmContents, m.Content)
		}
		wantAsm := []string{"plain", "pending"}
		if len(asmContents) != len(wantAsm) || asmContents[0] != wantAsm[0] || asmContents[1] != wantAsm[1] {
			t.Errorf("ListForAssembly = %v, want %v", asmContents, wantAsm)
		}
	})

	t.Run("MarkDelivered_FlipsOnlyGivenIDsScopedToRun", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		otherRunID := seedConversationForTest(t, orgID, seed, "running")

		delivered := false
		id1, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "user", Subtype: "injection:steer", Content: "1", Delivered: &delivered})
		if err != nil {
			t.Fatalf("InsertMessage 1: %v", err)
		}
		id2, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "user", Subtype: "injection:steer", Content: "2", Delivered: &delivered})
		if err != nil {
			t.Fatalf("InsertMessage 2: %v", err)
		}
		idOther, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: otherRunID, Role: "user", Subtype: "injection:steer", Content: "other", Delivered: &delivered})
		if err != nil {
			t.Fatalf("InsertMessage other: %v", err)
		}

		// Ask to flip id1 (belongs to runID) and idOther (belongs to a
		// DIFFERENT run) via a call scoped to runID — idOther must NOT flip.
		if err := store.MarkDeliveredSystem(ctx, orgID, runID, []int{int(id1), int(idOther)}, ""); err != nil {
			t.Fatalf("MarkDelivered: %v", err)
		}

		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages(runID): %v", err)
		}
		byID := map[int]*domain.Message{}
		for i := range msgs {
			byID[msgs[i].ID] = &msgs[i]
		}
		if d := byID[int(id1)].Delivered; d == nil || !*d {
			t.Errorf("id1 Delivered = %v, want true", d)
		}
		if d := byID[int(id2)].Delivered; d == nil || *d {
			t.Errorf("id2 Delivered = %v, want false (not in the flip list)", d)
		}

		otherMsgs, err := store.Messages(ctx, orgID, otherRunID)
		if err != nil {
			t.Fatalf("Messages(otherRunID): %v", err)
		}
		if len(otherMsgs) != 1 || otherMsgs[0].Delivered == nil || *otherMsgs[0].Delivered {
			t.Errorf("otherRunID message Delivered = %+v, want still false (run-scoped, must not leak across runs)", otherMsgs)
		}
	})

	t.Run("MarkDelivered_StampsSubtypeWhenGivenAndPreservesItWhenEmpty", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		delivered := false
		steered, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "user", Subtype: "text", Content: "mid-turn", Delivered: &delivered})
		if err != nil {
			t.Fatalf("InsertMessage steered: %v", err)
		}
		bare, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "user", Subtype: "text", Content: "bare", Delivered: &delivered})
		if err != nil {
			t.Fatalf("InsertMessage bare: %v", err)
		}

		// A steer drain flushes and stamps in one call.
		if err := store.MarkDeliveredSystem(ctx, orgID, runID, []int{int(steered)}, "injection:steer"); err != nil {
			t.Fatalf("MarkDelivered(steer): %v", err)
		}
		// A bare drain flushes without touching the row's own subtype.
		if err := store.MarkDeliveredSystem(ctx, orgID, runID, []int{int(bare)}, ""); err != nil {
			t.Fatalf("MarkDelivered(bare): %v", err)
		}

		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		byID := map[int]*domain.Message{}
		for i := range msgs {
			byID[msgs[i].ID] = &msgs[i]
		}
		if got := byID[int(steered)]; got == nil || got.Subtype != "injection:steer" {
			t.Errorf("steered row subtype = %+v, want injection:steer", got)
		} else if got.Delivered == nil || !*got.Delivered {
			t.Errorf("steered row Delivered = %v, want true", got.Delivered)
		}
		if got := byID[int(bare)]; got == nil || got.Subtype != "text" {
			t.Errorf("bare row subtype = %+v, want the row's own \"text\" preserved", got)
		} else if got.Delivered == nil || !*got.Delivered {
			t.Errorf("bare row Delivered = %v, want true", got.Delivered)
		}
	})

	t.Run("SetWindowState_BatchFlipsBeforeThresholdAndReturnsCount", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")

		var ids []int64
		for _, c := range []string{"m1", "m2", "m3", "m4"} {
			id, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: runID, Role: "assistant", Subtype: "text", Content: c})
			if err != nil {
				t.Fatalf("InsertMessage %s: %v", c, err)
			}
			ids = append(ids, id)
		}

		// Elide everything strictly before m3's assembly key — m1, m2 flip;
		// m3, m4 stay active.
		n, err := store.SetWindowStateSystem(ctx, orgID, runID, float64(ids[2]), domain.MessageWindowActive, domain.MessageWindowElided)
		if err != nil {
			t.Fatalf("SetWindowState: %v", err)
		}
		if n != 2 {
			t.Errorf("flipped count = %d, want 2", n)
		}

		msgs, err := store.Messages(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		byID := map[int64]domain.MessageWindowState{}
		for _, m := range msgs {
			byID[int64(m.ID)] = m.WindowState
		}
		if byID[ids[0]] != domain.MessageWindowElided || byID[ids[1]] != domain.MessageWindowElided {
			t.Errorf("m1/m2 window_state = %v/%v, want elided/elided", byID[ids[0]], byID[ids[1]])
		}
		if byID[ids[2]] != domain.MessageWindowActive || byID[ids[3]] != domain.MessageWindowActive {
			t.Errorf("m3/m4 window_state = %v/%v, want active/active (at/after threshold)", byID[ids[2]], byID[ids[3]])
		}

		// Re-running the identical flip now matches nothing (m1/m2 are no
		// longer in the `from` state) — idempotent, not cumulative.
		n2, err := store.SetWindowStateSystem(ctx, orgID, runID, float64(ids[2]), domain.MessageWindowActive, domain.MessageWindowElided)
		if err != nil {
			t.Fatalf("SetWindowState (rerun): %v", err)
		}
		if n2 != 0 {
			t.Errorf("rerun flipped count = %d, want 0", n2)
		}
	})

	t.Run("MessagesForRuns_BatchedAcrossRuns", func(t *testing.T) {
		// The batched twin of Messages: one query returns every message
		// for many runs, grouped by RunID with each run's messages still
		// in insertion (id ASC) order. Empty input is a no-op. Backs the
		// Board's aggregated include=messages read.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		if msgs, err := store.MessagesForRuns(ctx, orgID, nil); err != nil || msgs != nil {
			t.Fatalf("MessagesForRuns(nil) = (%v, %v), want (nil, nil)", msgs, err)
		}

		runA := seedConversationForTest(t, orgID, seed, "running")
		runB := seedConversationForTest(t, orgID, seed, "running")
		for _, c := range []string{"a-first", "a-second"} {
			if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
				ConversationID: runA, Role: "assistant", Content: c, Subtype: "text",
			}); err != nil {
				t.Fatalf("InsertMessage A: %v", err)
			}
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: runB, Role: "assistant", Content: "b-only", Subtype: "text",
		}); err != nil {
			t.Fatalf("InsertMessage B: %v", err)
		}

		// A non-UUID id must be tolerated (no rows, no error) — guards the
		// Postgres uuid[] bind path, same as ListForTasks above.
		msgs, err := store.MessagesForRuns(ctx, orgID, []string{runA, runB, "not-a-uuid"})
		if err != nil {
			t.Fatalf("MessagesForRuns: %v", err)
		}
		byRun := map[string][]string{}
		for _, m := range msgs {
			byRun[m.ConversationID] = append(byRun[m.ConversationID], m.Content)
		}
		if got := byRun[runA]; len(got) != 2 || got[0] != "a-first" || got[1] != "a-second" {
			t.Errorf("run A messages = %v, want [a-first a-second] (per-run order preserved)", got)
		}
		if got := byRun[runB]; len(got) != 1 || got[0] != "b-only" {
			t.Errorf("run B messages = %v, want [b-only]", got)
		}
	})

	t.Run("TokenTotalsSystem_SumsAssistantOnly", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "running")
		// Two assistant messages with tokens, plus a user message that
		// should NOT contribute to totals.
		i1, i2 := 100, 50
		o1, o2 := 200, 75
		for _, tup := range []struct {
			role           string
			input, output  int
			countsToTotals bool
		}{
			{"assistant", i1, o1, true},
			{"assistant", i2, o2, true},
			{"user", 99999, 99999, false},
		} {
			in, out := tup.input, tup.output
			msg := &domain.Message{
				ConversationID: runID, Role: tup.role, Content: "x", Subtype: "text",
				InputTokens: &in, OutputTokens: &out,
				Model: "claude-test",
			}
			if _, err := store.InsertMessage(ctx, orgID, msg); err != nil {
				t.Fatalf("InsertMessage(%s): %v", tup.role, err)
			}
		}
		tot, err := store.TokenTotalsSystem(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("TokenTotalsSystem: %v", err)
		}
		if tot.InputTokens != i1+i2 {
			t.Errorf("InputTokens = %d, want %d (user role must not count)", tot.InputTokens, i1+i2)
		}
		if tot.OutputTokens != o1+o2 {
			t.Errorf("OutputTokens = %d, want %d", tot.OutputTokens, o1+o2)
		}
		if tot.NumTurns != 2 {
			t.Errorf("NumTurns = %d, want 2 (assistant rows)", tot.NumTurns)
		}
	})

	t.Run("LastAgentActivityAtSystem_NewestNonUserMessage", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedConversationForTest(t, orgID, seed, "open")

		// No agent message yet → ok=false, the caller falls back to run start.
		if _, ok, err := store.LastAgentActivityAtSystem(ctx, orgID, runID); err != nil || ok {
			t.Fatalf("empty run: got ok=%v err=%v, want ok=false err=nil", ok, err)
		}

		// Insert in id order: an assistant turn, then a LATER user follow-up. The
		// watermark must be the assistant row, NOT the user message — a resume's
		// just-recorded user message must not poison the "agent last ran" mark.
		// Explicit timestamps make the assertion exact; ordering is by id, so the
		// user row (inserted last, newest id) would win a naive ORDER BY id query.
		agentAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
		for _, m := range []*domain.Message{
			{ConversationID: runID, Role: "assistant", Subtype: "text", Content: "agent turn", CreatedAt: agentAt},
			{ConversationID: runID, Role: "user", Subtype: "text", Content: "resume message", CreatedAt: agentAt.Add(time.Hour)},
		} {
			if _, err := store.InsertMessage(ctx, orgID, m); err != nil {
				t.Fatalf("InsertMessage(%s): %v", m.Role, err)
			}
		}
		at, ok, err := store.LastAgentActivityAtSystem(ctx, orgID, runID)
		if err != nil || !ok {
			t.Fatalf("after messages: got ok=%v err=%v, want ok=true err=nil", ok, err)
		}
		if !at.Equal(agentAt) {
			t.Errorf("watermark = %v, want the assistant row %v (user message must be excluded)", at, agentAt)
		}

		// A newer agent (tool) message advances the watermark.
		toolAt := agentAt.Add(2 * time.Hour)
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: runID, Role: "tool", Subtype: "tool", Content: "tool result", CreatedAt: toolAt,
		}); err != nil {
			t.Fatalf("InsertMessage(tool): %v", err)
		}
		at2, ok, err := store.LastAgentActivityAtSystem(ctx, orgID, runID)
		if err != nil || !ok {
			t.Fatalf("after tool message: got ok=%v err=%v", ok, err)
		}
		if !at2.Equal(toolAt) {
			t.Errorf("watermark = %v, want the newer tool row %v", at2, toolAt)
		}
	})

	t.Run("BlueprintSiblingCostUSDSystem_SumsSettledExcludingSelf", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "bp-cost")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		brID := seed.BlueprintRun(t, taskID)

		// Two runs sharing one blueprint_run — sibling steps. (The seeder
		// mints a fresh blueprint_run per call, so we reuse brID directly
		// to stage the multi-step shape the footer aggregates over.)
		step1 := seed.Run(t, domain.Conversation{
			TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", BlueprintRunID: brID,
		})
		step2 := seed.Run(t, domain.Conversation{
			TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", BlueprintRunID: brID,
		})
		// Each step streams a row and settles its cost lump on it — the
		// sibling sum reads the ledger, not any run-level column.
		settle := func(stepID string, cost float64) {
			t.Helper()
			if _, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: stepID, Role: "assistant", Subtype: "text", Content: "work"}); err != nil {
				t.Fatalf("InsertMessage %s: %v", stepID, err)
			}
			if err := store.Complete(ctx, orgID, stepID, "completed", cost, 1000, 1, "ok", "", "finish", "", ""); err != nil {
				t.Fatalf("Complete %s: %v", stepID, err)
			}
		}
		settle(step1, 0.01)
		settle(step2, 0.02)

		near := func(got, want float64) bool { return got-want < 1e-9 && want-got < 1e-9 }

		// Querying for step2 returns step1's settled cost only (self excluded).
		sib, err := store.BlueprintSiblingCostUSDSystem(ctx, orgID, brID, step2)
		if err != nil {
			t.Fatalf("BlueprintSiblingCostUSDSystem(step2): %v", err)
		}
		if !near(sib, 0.01) {
			t.Errorf("sibling cost excluding step2 = %v, want 0.01 (step1 only)", sib)
		}
		// Symmetric: querying for step1 returns step2's cost.
		sib, err = store.BlueprintSiblingCostUSDSystem(ctx, orgID, brID, step1)
		if err != nil {
			t.Fatalf("BlueprintSiblingCostUSDSystem(step1): %v", err)
		}
		if !near(sib, 0.02) {
			t.Errorf("sibling cost excluding step1 = %v, want 0.02 (step2 only)", sib)
		}
		// A blueprint_run with no other runs sums to 0, not an error.
		sib, err = store.BlueprintSiblingCostUSDSystem(ctx, orgID, uuid.New().String(), step1)
		if err != nil {
			t.Fatalf("BlueprintSiblingCostUSDSystem(empty): %v", err)
		}
		if !near(sib, 0) {
			t.Errorf("sibling cost for empty blueprint_run = %v, want 0", sib)
		}

		// An unsettled sibling (seeded, never completed → no settlement
		// stamps on its rows) contributes 0, not an error. Add a third,
		// never-completed step and re-query for step2 — the settled total
		// is unchanged (step1's 0.01 only).
		_ = seed.Run(t, domain.Conversation{
			TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", BlueprintRunID: brID,
		})
		sib, err = store.BlueprintSiblingCostUSDSystem(ctx, orgID, brID, step2)
		if err != nil {
			t.Fatalf("BlueprintSiblingCostUSDSystem(step2, unsettled sibling): %v", err)
		}
		if !near(sib, 0.01) {
			t.Errorf("sibling cost with an unsettled step = %v, want 0.01 (NULL cost omitted)", sib)
		}
	})

	t.Run("BlueprintSiblingDurationMsSystem_SumsSettledExcludingSelf", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "bp-dur")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		brID := seed.BlueprintRun(t, taskID)

		// Two runs sharing one blueprint_run — sibling steps. Duration lives
		// on the claim Complete releases, so each step goes live (minting a
		// claim) before it completes.
		step1 := seed.Run(t, domain.Conversation{
			TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", BlueprintRunID: brID,
		})
		step2 := seed.Run(t, domain.Conversation{
			TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", BlueprintRunID: brID,
		})
		settle := func(stepID string, durationMs int) {
			t.Helper()
			if err := store.SetExecutorSystem(ctx, orgID, stepID, "exec-dur", 1); err != nil {
				t.Fatalf("SetExecutorSystem %s: %v", stepID, err)
			}
			if err := store.Complete(ctx, orgID, stepID, "completed", 0, durationMs, 1, "ok", "", "finish", "", ""); err != nil {
				t.Fatalf("Complete %s: %v", stepID, err)
			}
		}
		settle(step1, 1000)
		settle(step2, 2000)

		// Querying for step2 returns step1's settled duration only (self excluded).
		ms, err := store.BlueprintSiblingDurationMsSystem(ctx, orgID, brID, step2)
		if err != nil {
			t.Fatalf("BlueprintSiblingDurationMsSystem(step2): %v", err)
		}
		if ms != 1000 {
			t.Errorf("sibling duration excluding step2 = %d, want 1000 (step1 only)", ms)
		}
		// Symmetric: querying for step1 returns step2's duration.
		ms, err = store.BlueprintSiblingDurationMsSystem(ctx, orgID, brID, step1)
		if err != nil {
			t.Fatalf("BlueprintSiblingDurationMsSystem(step1): %v", err)
		}
		if ms != 2000 {
			t.Errorf("sibling duration excluding step1 = %d, want 2000 (step2 only)", ms)
		}
		// A blueprint_run with no other runs sums to 0, not an error.
		ms, err = store.BlueprintSiblingDurationMsSystem(ctx, orgID, uuid.New().String(), step1)
		if err != nil {
			t.Fatalf("BlueprintSiblingDurationMsSystem(empty): %v", err)
		}
		if ms != 0 {
			t.Errorf("sibling duration for empty blueprint_run = %d, want 0", ms)
		}
		// An unsettled sibling (seeded, never claimed nor completed → no
		// claim telemetry) contributes 0: SUM skips it, COALESCE floors at 0.
		_ = seed.Run(t, domain.Conversation{
			TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", BlueprintRunID: brID,
		})
		ms, err = store.BlueprintSiblingDurationMsSystem(ctx, orgID, brID, step2)
		if err != nil {
			t.Fatalf("BlueprintSiblingDurationMsSystem(step2, unsettled sibling): %v", err)
		}
		if ms != 1000 {
			t.Errorf("sibling duration with an unsettled step = %d, want 1000 (NULL omitted)", ms)
		}
	})

	t.Run("MemoryMissing_DerivedFromRunMemoryJOIN", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "mem")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)

		// One run per memory-content state. memory_missing should be
		// true for no-row, NULL, "", whitespace; false for populated.
		runNoRow := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		runNullContent := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		runEmpty := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		runWhitespace := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		runPopulated := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		seed.SetRunMemory(t, runNullContent, ent, NullMemorySentinel)
		seed.SetRunMemory(t, runEmpty, ent, "")
		seed.SetRunMemory(t, runWhitespace, ent, "  \t\n ")
		seed.SetRunMemory(t, runPopulated, ent, "real reasoning text")

		want := map[string]bool{
			runNoRow:       true,
			runNullContent: true,
			runEmpty:       true,
			runWhitespace:  true,
			runPopulated:   false,
		}
		for id, expected := range want {
			got, err := store.Get(ctx, orgID, id)
			if err != nil || got == nil {
				t.Fatalf("Get %s: err=%v got=%v", id, err, got)
			}
			if got.MemoryMissing != expected {
				t.Errorf("run %s: memory_missing=%v, want %v", id, got.MemoryMissing, expected)
			}
		}
	})
}

// seedConversationForTest creates a fresh entity+event+task+run chain and
// returns the run ID. status is what we want the conversation row to land
// in; the seeder inserts the row with the status set directly rather
// than driving the lifecycle methods (which is the conformance
// suite's job to test).
func seedConversationForTest(t *testing.T, orgID string, seed ConversationSeeder, status string) string {
	t.Helper()
	ent := seed.Entity(t, "seed-"+status+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	ev := seed.Event(t, ent, domain.EventGitHubPROpened)
	taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
	return seedConversationForTaskTest(t, orgID, taskID, status, seed)
}

// seedConversationForTaskTest creates a run on an existing task, used
// by tests that need multiple runs on the same parent. Each run gets
// its own freshly-minted blueprint_run; independent firings on a shared
// task is the realistic shape.
func seedConversationForTaskTest(t *testing.T, orgID, taskID, status string, seed ConversationSeeder) string {
	t.Helper()
	_ = orgID
	return seed.Run(t, domain.Conversation{
		TaskID: taskID, PromptID: agentRunTestPrompt(t), Status: status, Model: "m",
		BlueprintRunID: seed.BlueprintRun(t, taskID),
	})
}

// agentRunTestPromptID is the prompt-row id the backend test files
// seed once per test factory call. Conformance subtests reference
// it by this constant when creating runs; the seeder doesn't surface
// it as a field because every call uses the same value within one
// subtest.
const agentRunTestPromptID = "p_agentrun_test"

func agentRunTestPrompt(_ *testing.T) string { return agentRunTestPromptID }
