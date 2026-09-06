package dbtest

import (
	"context"
	"errors"
	"maps"
	"slices"
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
	// PeakMemMB / CPUUsec are the engagement's measured sandbox cost, nil
	// when the columns are NULL ("not measured"). Projected here rather than
	// onto domain.Claim because nothing reads them yet — the seeder is the
	// only reader, so the suite can assert the write without putting an
	// unconsumed field on a wire type.
	PeakMemMB *int
	CPUUsec   *int64
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
	// don't care about the parent task status, only about the
	// conversations hanging off it.
	Task func(t *testing.T, entityID, eventType, primaryEventID string) string

	// Team inserts a second team in the org and returns its id, so a subtest
	// can stage conversations that belong to different teams. The default
	// team every other callback writes against is the one the factory
	// seeded; this is the only way to get a SECOND one, which is what the
	// team narrowing has to be told apart from.
	Team func(t *testing.T, slug string) string

	// Conversation inserts a conversations row directly and returns its id (conv.ID
	// when set, a fresh uuid otherwise). ConversationQueueStore.EnqueueConversation is the
	// only production mint; the conformance suite stages rows in arbitrary
	// status without driving the queue. Fields honored: ID, TaskID, TeamID
	// (default: the org's first team), PromptID, Status, Model, TriggerType
	// (default manual), TriggerID, BlueprintRunID. An empty Status inserts
	// SQL NULL — the mid-flight state a fresh mint carries, which the
	// display ladder reads as `queued` — not an empty string, which is not
	// a status at all.
	Conversation func(t *testing.T, conv domain.Conversation) string

	// BackdateStartedAt rewrites the conversation's started_at to `age` ago,
	// on the BACKEND's own clock and in the backend's own storage format, so
	// the suite can stage a queue whose order is unambiguous without
	// depending on Go/DB clock skew or on how a driver renders a time.Time.
	// No store method writes the column after the mint, and the default
	// resolution is coarse enough (one second, in SQLite) that rows seeded
	// back-to-back tie.
	BackdateStartedAt func(t *testing.T, conversationID string, age time.Duration)

	// BackdateQueuedAt is BackdateStartedAt for queued_at: it stages a
	// conversation whose current queue episode began `age` ago, so the suite
	// can tell a re-stamp from a stamp the row already carried. The wake is
	// the one store write that touches the column after the mint.
	BackdateQueuedAt func(t *testing.T, conversationID string, age time.Duration)

	// ClaimRows returns the conversation's claims rows oldest-first, so
	// the suite can assert mint/release bookkeeping.
	ClaimRows func(t *testing.T, conversationID string) []ClaimRow

	// PreferredExecutor reads the conversation's placement affinity stamp
	// (conversations.preferred_executor_id), "" for SQL NULL. No store read
	// projects the column — placement is the only consumer and it reads it
	// in-SQL — so the suite reads it directly to assert what the resume flip
	// writes.
	PreferredExecutor func(t *testing.T, conversationID string) string

	// CollapseClaimTimes forces every claim on the conversation to share one
	// claimed_at and created_at, leaving released_at as recorded. It stages
	// the tie a Postgres transaction produces for free — now() is fixed for
	// the transaction, so claims minted together are indistinguishable by
	// mint time — which is what any "which claim came before mine" read has
	// to stay deterministic through.
	//
	// It destroys the ordering ClaimRows itself sorts by, so a test that
	// calls it must read the claim ids first.
	CollapseClaimTimes func(t *testing.T, conversationID string)

	// BlueprintRun mints a blueprint + blueprint_run pair against the
	// given taskID and returns the blueprint_run id. Every conversation
	// row carries a NOT NULL blueprint_run_id FK (a single prompt is a
	// 1-step blueprint), so the conformance suite stages one of these
	// per conversation it creates and sets domain.Conversation.BlueprintRunID.
	// Each call mints a fresh independent blueprint_run — that matches the
	// real firing model (one delegation = one blueprint_run) and keeps
	// multi-conversation-per-task subtests realistic.
	BlueprintRun func(t *testing.T, taskID string) string

	// SetSnapshotState upserts the key's workspace_snapshots row to the given
	// state ('pending' | 'written' | 'failed'). WorkspaceSnapshotStore owns
	// that table, so it is a seeded precondition here rather than a store call
	// — the eviction enumeration reads it as its safety gate, and the suite
	// has to be able to stage all three states plus the no-row case.
	SetSnapshotState func(t *testing.T, blueprintRunID, state string)

	// SetBlueprintRunStatus raw-updates a blueprint_run's status, WITHOUT
	// touching its child conversations (a plain UPDATE, not
	// BlueprintStore.MarkRunStatus, which now cascades a terminal flip onto
	// children). Used to stage the "parked conversation under an
	// already-terminal parent" precondition the worktree-preserve filter must
	// exclude.
	SetBlueprintRunStatus func(t *testing.T, blueprintRunID, status string)

	// StampAgentClaim sets the task's claimed_by_agent_id directly.
	// Used to set up task-claim preconditions for claim-flip
	// assertions.
	StampAgentClaim func(t *testing.T, taskID, agentID string)

	// SetConversationMemory upserts a conversation_memory row with the given
	// agent_content. content="" inserts an empty string;
	// NullMemorySentinel inserts SQL NULL.
	SetConversationMemory func(t *testing.T, conversationID, entityID, content string)

	// SeedRawMessage inserts a messages row with rawJSON written
	// directly into the given column ("reasoning" or "content_blocks"),
	// bypassing InsertMessage's json.Marshal. Used to stage
	// well-formed-but-wrong-shaped JSON (a valid jsonb value in Postgres,
	// which enforces syntax at the storage layer, that nonetheless fails to
	// unmarshal into the target Go slice) so a test can assert the read
	// path surfaces the decode error instead of silently discarding it.
	// Returns the row's id.
	SeedRawMessage func(t *testing.T, conversationID, column, rawJSON string) int64

	// Artifact inserts an artifacts row on the conversation and returns its
	// id. detailsJSON is written verbatim (empty string included), because
	// the unresolved-artifact predicate reads the review's ready sentinel out
	// of it and the suite has to be able to stage a value that is not JSON at
	// all. ArtifactStore owns the table, so this is a seeded precondition
	// rather than a store call — the conversation list reads it as a
	// correlated predicate and never writes one.
	Artifact func(t *testing.T, conversationID, kind, state, detailsJSON string) string

	// PendingPermission inserts a state='pending' conversation_permissions
	// row owned by claimID. PermissionStore owns the table; the conversation
	// list only reads it, and "pending" is derived against the conversation's
	// active claim, so a test has to be able to stage a prompt on a claim it
	// names rather than on whichever claim happens to be live.
	PendingPermission func(t *testing.T, conversationID, claimID, toolCallID string) string

	// AgentID returns an identifier suitable for the
	// StampAgentClaim agentID and the conversation row's actor_agent_id.
	// Backends use this to thread their own seeded agent row (the
	// Postgres path needs a real FK; SQLite is more relaxed).
	AgentID string
}

// RunConversationStoreConformance covers the ConversationStore contract every
// backend impl must hold:
//
//   - Lifecycle methods (Complete / SetSession / ParkOpen /
//     MarkQueuedForResume / MarkFailedIfActive)
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

	t.Run("SeededConversation_GetReturnsIt", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got == nil || got.ID != conversationID {
			t.Fatalf("Get returned %v, want id=%s", got, conversationID)
		}
		if got.Status != "running" || got.Model != "m" {
			t.Errorf("Get fields drift: %+v", got)
		}
		if got.Attempts != 0 || got.ClaimedAt != nil || got.ExecutorID != "" {
			t.Errorf("never-claimed conversation carries claim state: attempts=%d claimedAt=%v executor=%q",
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
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-cost", 1); err != nil {
			t.Fatalf("SetExecutorSystem 1: %v", err)
		}
		msg1, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "turn 1"})
		if err != nil {
			t.Fatalf("InsertMessage 1: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 1.25, 4000, 3, "", "", "", ""); err != nil {
			t.Fatalf("first Complete: %v", err)
		}
		got, err := store.Get(ctx, orgID, conversationID)
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
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-cost", 2); err != nil {
			t.Fatalf("SetExecutorSystem 2: %v", err)
		}
		msg2, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "turn 2"})
		if err != nil {
			t.Fatalf("InsertMessage 2: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 0.75, 2000, 5, "all done", "abort", "needs human", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		got, err = store.Get(ctx, orgID, conversationID)
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
		// A terminal records no park reason: it did not park. The row's
		// park_reason is whatever an earlier park left, which here is nothing.
		if got.ParkReason != "" {
			t.Errorf("park_reason = %q after a terminal, want empty", got.ParkReason)
		}
		if got.Outcome != "abort" {
			t.Errorf("outcome = %q, want abort", got.Outcome)
		}
		if got.OutcomeReason != "needs human" {
			t.Errorf("outcome_reason = %q, want \"needs human\"", got.OutcomeReason)
		}

		// The stamps land exactly where reported: one lump per invocation's
		// last row, no proration.
		msgs, err := store.Messages(ctx, orgID, conversationID)
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

	deref := func(f *float64) any {
		if f == nil {
			return "<nil>"
		}
		return *f
	}

	t.Run("Complete_ZeroLumpPreservesPerRowStamps", func(t *testing.T) {
		// The native runtime settles spend per assistant row as it goes, and
		// its terminal Complete reports zero — there is nothing left to
		// settle. A zero lump must therefore stamp NOTHING: the first live
		// native conversation lost its final row's stamp to the unconditional
		// overwrite, and the conversation's total under-reported by exactly that
		// row.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-zero", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		// Exactly representable in float4: the postgres column is `real`, and
		// a value that widens on the round trip would fail the equality below
		// for the wrong reason.
		stamped := 0.25
		msgID, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "assistant",
			Content: "final turn", Model: "claude-sonnet-5", CostUSD: &stamped,
		})
		if err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 0, 4000, 3, "done", "continue", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}

		msgs, err := store.Messages(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		for i := range msgs {
			if msgs[i].ID != int(msgID) {
				continue
			}
			if c := msgs[i].CostUSD; c == nil || *c != stamped {
				t.Errorf("per-row stamp = %v, want %v preserved — a zero lump settles nothing", deref(c), stamped)
			}
		}
		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v, got=%v", err, got)
		}
		if got.Status != "completed" {
			t.Errorf("status = %q, want completed — skipping the lump must not skip the terminal write", got.Status)
		}
		if got.TotalCostUSD == nil || *got.TotalCostUSD != stamped {
			t.Errorf("total_cost_usd = %v, want %v (the ledger SUM over per-row stamps)", deref(got.TotalCostUSD), stamped)
		}
	})

	t.Run("Complete_SettlesOnEngagementRow_SkipsForeignNewerRow", func(t *testing.T) {
		// The lump must land on the settling engagement's own newest row —
		// NOT on the conversation's newest row when that row belongs to a
		// different (already-released) claim.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		// Engagement 1 records a row and settles.
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-a", 1); err != nil {
			t.Fatalf("SetExecutorSystem 1: %v", err)
		}
		claims := seed.ClaimRows(t, conversationID)
		if len(claims) != 1 {
			t.Fatalf("claims = %+v, want 1", claims)
		}
		claim1 := claims[0].ID
		msgA, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "a"})
		if err != nil {
			t.Fatalf("InsertMessage a: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 1.25, 0, 0, "", "abort", "wait", ""); err != nil {
			t.Fatalf("first Complete: %v", err)
		}

		// Engagement 2 goes live and records its row; then a NEWER row
		// attributed (explicitly) to the released first claim lands.
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-b", 2); err != nil {
			t.Fatalf("SetExecutorSystem 2: %v", err)
		}
		msgB, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "b"})
		if err != nil {
			t.Fatalf("InsertMessage b: %v", err)
		}
		msgC, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "c", ClaimID: claim1})
		if err != nil {
			t.Fatalf("InsertMessage c: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 0.75, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("second Complete: %v", err)
		}

		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.TotalCostUSD == nil || *got.TotalCostUSD != 2.0 {
			t.Errorf("total_cost_usd = %v, want 2.0 (one lump per engagement)", got.TotalCostUSD)
		}
		msgs, err := store.Messages(ctx, orgID, conversationID)
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
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		msg1, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "older"})
		if err != nil {
			t.Fatalf("InsertMessage 1: %v", err)
		}
		msg2, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "newest"})
		if err != nil {
			t.Fatalf("InsertMessage 2: %v", err)
		}
		// No active claim at all: the fallback owns the settle.
		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 1.25, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("first Complete: %v", err)
		}
		// A live claim whose engagement recorded nothing (both rows predate
		// it) falls back the same way, added.
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-rowless", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 0.75, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("rowless-engagement Complete: %v", err)
		}
		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.TotalCostUSD == nil || *got.TotalCostUSD != 2.0 {
			t.Errorf("total_cost_usd = %v, want 2.0 (1.25 fallback-added + 0.75 fallback-added)", got.TotalCostUSD)
		}
		msgs, err := store.Messages(ctx, orgID, conversationID)
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

	t.Run("Complete_SkipsSyntheticRowsWhenSettling", func(t *testing.T) {
		// An invocation that errored or was interrupted ends on a
		// runtime-composed row, which names no model. The lump must land on
		// the newest row of the engagement that actually ran a model, so the
		// per-model breakdown never bills dollars to "<synthetic>".
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-synth", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		real1, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: "real turn", Model: "claude-opus-5",
		})
		if err != nil {
			t.Fatalf("InsertMessage real: %v", err)
		}
		// A NULL-model row between the two: newer than the real-model row,
		// and not synthetic, so a filter-only settle would land here and drop
		// the lump out of the per-model breakdown just as surely.
		toolRow, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "tool", Content: "tool result",
		})
		if err != nil {
			t.Fatalf("InsertMessage tool: %v", err)
		}
		synth1, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "assistant",
			Content: "API Error: overloaded", Model: domain.ModelSynthetic,
		})
		if err != nil {
			t.Fatalf("InsertMessage synthetic: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, conversationID, "failed", 1.5, 1000, 2, "", "", "", "infra"); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		msgs, err := store.Messages(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		costByID := map[int]*float64{}
		for i := range msgs {
			costByID[msgs[i].ID] = msgs[i].CostUSD
		}
		if c := costByID[int(real1)]; c == nil || *c != 1.5 {
			t.Errorf("real-model row cost_usd = %v, want 1.5 (the lump skips past the newer no-model and synthetic rows)", c)
		}
		if c := costByID[int(toolRow)]; c != nil {
			t.Errorf("no-model row cost_usd = %v, want nil (a real-model row outranks it however much older)", *c)
		}
		if c := costByID[int(synth1)]; c != nil {
			t.Errorf("synthetic row cost_usd = %v, want nil (never a settle target)", *c)
		}

		// Last resort: an engagement whose rows are ALL synthetic still
		// records its dollars — totals stay exact even though the row names
		// no model (the usage breakdowns exclude it, by_day still counts it).
		onlySynthID := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.SetExecutorSystem(ctx, orgID, onlySynthID, "exec-synth-only", 1); err != nil {
			t.Fatalf("SetExecutorSystem only-synth: %v", err)
		}
		onlySynth, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: onlySynthID, Role: "assistant",
			Content: "API Error: overloaded", Model: domain.ModelSynthetic,
		})
		if err != nil {
			t.Fatalf("InsertMessage only-synthetic: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, onlySynthID, "failed", 0.25, 0, 0, "", "", "", "infra"); err != nil {
			t.Fatalf("Complete only-synth: %v", err)
		}
		gotOnly, err := store.Get(ctx, orgID, onlySynthID)
		if err != nil || gotOnly == nil {
			t.Fatalf("Get only-synth: err=%v got=%v", err, gotOnly)
		}
		if gotOnly.TotalCostUSD == nil || *gotOnly.TotalCostUSD != 0.25 {
			t.Errorf("all-synthetic total_cost_usd = %v, want 0.25 (dollars stay on the ledger)", gotOnly.TotalCostUSD)
		}
		onlyMsgs, err := store.Messages(ctx, orgID, onlySynthID)
		if err != nil {
			t.Fatalf("Messages only-synth: %v", err)
		}
		if len(onlyMsgs) != 1 || onlyMsgs[0].ID != int(onlySynth) {
			t.Fatalf("only-synth messages = %+v, want the one synthetic row", onlyMsgs)
		}
		if c := onlyMsgs[0].CostUSD; c == nil || *c != 0.25 {
			t.Errorf("last-resort row cost_usd = %v, want 0.25", c)
		}
	})

	t.Run("Complete_ConversationWideFallback_RanksRealModelThenNoModel", func(t *testing.T) {
		// The claim-less fallback arm ranks the same three tiers: a
		// real-model row first, then a no-model row, and a synthetic row only
		// when nothing else exists. Newer never beats a better tier.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		// Tier 1 beats a newer tier-2 row: an older real-model row wins over
		// the no-model row that follows it.
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		realRow, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: "real", Model: "claude-opus-5",
		})
		if err != nil {
			t.Fatalf("InsertMessage real: %v", err)
		}
		newerNull, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "tool", Content: "tool result",
		})
		if err != nil {
			t.Fatalf("InsertMessage tool: %v", err)
		}
		// No claim at all: the fallback arm owns this settle.
		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 1.25, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		msgs, err := store.Messages(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		costByID := map[int]*float64{}
		for i := range msgs {
			costByID[msgs[i].ID] = msgs[i].CostUSD
		}
		if c := costByID[int(realRow)]; c == nil || *c != 1.25 {
			t.Errorf("real-model row cost_usd = %v, want 1.25 (outranks the newer no-model row)", c)
		}
		if c := costByID[int(newerNull)]; c != nil {
			t.Errorf("newer no-model row cost_usd = %v, want nil", *c)
		}

		// Tier 2 beats a newer tier-3 row: with no real-model row anywhere,
		// the no-model row still wins over the synthetic row after it.
		noRealID := seedConversationForTest(t, orgID, seed, "running")
		nullRow, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: noRealID, Role: "user", Content: "go",
		})
		if err != nil {
			t.Fatalf("InsertMessage user: %v", err)
		}
		newerSynth, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: noRealID, Role: "assistant",
			Content: "API Error: overloaded", Model: domain.ModelSynthetic,
		})
		if err != nil {
			t.Fatalf("InsertMessage synthetic: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, noRealID, "failed", 0.5, 0, 0, "", "", "", "infra"); err != nil {
			t.Fatalf("Complete no-real: %v", err)
		}
		noRealMsgs, err := store.Messages(ctx, orgID, noRealID)
		if err != nil {
			t.Fatalf("Messages no-real: %v", err)
		}
		noRealCost := map[int]*float64{}
		for i := range noRealMsgs {
			noRealCost[noRealMsgs[i].ID] = noRealMsgs[i].CostUSD
		}
		if c := noRealCost[int(nullRow)]; c == nil || *c != 0.5 {
			t.Errorf("no-model row cost_usd = %v, want 0.5 (outranks the newer synthetic row)", c)
		}
		if c := noRealCost[int(newerSynth)]; c != nil {
			t.Errorf("newer synthetic row cost_usd = %v, want nil", *c)
		}
	})

	t.Run("Complete_NoRows_DropsLumpWithoutError", func(t *testing.T) {
		// A conversation with no message rows at all is the one truly
		// unattributable case: Complete succeeds (even with a live claim —
		// both settle paths find nothing), writes nothing to the ledger,
		// and must not invent a row.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-norows", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 9.99, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.Status != "completed" {
			t.Errorf("status = %q, want completed", got.Status)
		}
		if got.TotalCostUSD != nil {
			t.Errorf("total_cost_usd = %v, want nil (no ledger row to settle on)", *got.TotalCostUSD)
		}
		msgs, err := store.Messages(ctx, orgID, conversationID)
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
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		// messages is the source of truth (written by the streaming sink as
		// messages arrive); the read projection SUMs it live — no terminal
		// write is needed for the totals to appear, and re-Completing can't
		// double them. Two assistant rows so the SUM is non-trivial.
		ptr := func(n int) *int { return &n }
		for _, m := range []*domain.Message{
			{ConversationID: conversationID, Role: "assistant", Content: "a",
				InputTokens: ptr(100), OutputTokens: ptr(20), CacheReadTokens: ptr(1000), CacheCreationTokens: ptr(7)},
			{ConversationID: conversationID, Role: "assistant", Content: "b",
				InputTokens: ptr(50), OutputTokens: ptr(5), CacheReadTokens: ptr(500), CacheCreationTokens: ptr(3)},
		} {
			if _, err := store.InsertMessage(ctx, orgID, m); err != nil {
				t.Fatalf("InsertMessage: %v", err)
			}
		}

		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.InputTokens != 150 || got.OutputTokens != 25 || got.CacheReadTokens != 1500 || got.CacheCreationTokens != 10 {
			t.Errorf("token cols = (%d,%d,%d,%d), want (150,25,1500,10) — SUM over messages",
				got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheCreationTokens)
		}

		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 0, 0, 0, "done", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		got2, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got2 == nil {
			t.Fatalf("Get after Complete: err=%v got=%v", err, got2)
		}
		if got2.InputTokens != 150 || got2.OutputTokens != 25 || got2.CacheReadTokens != 1500 || got2.CacheCreationTokens != 10 {
			t.Errorf("after Complete token cols = (%d,%d,%d,%d), want unchanged (150,25,1500,10) — the ledger is the only source",
				got2.InputTokens, got2.OutputTokens, got2.CacheReadTokens, got2.CacheCreationTokens)
		}
	})

	// Tokens derive from the messages ledger at read time, so a conversation
	// that ends by cancel or infra-failure still reflects the tokens it streamed
	// with no terminal roll-up at all.
	t.Run("CancelAndFail_TokensStillDeriveFromLedger", func(t *testing.T) {
		ptr := func(n int) *int { return &n }
		seedRunningWithTokens := func(t *testing.T, store db.ConversationStore, orgID string, seed ConversationSeeder) string {
			t.Helper()
			ctx := context.Background()
			conversationID := seedConversationForTest(t, orgID, seed, "running")
			for _, m := range []*domain.Message{
				{ConversationID: conversationID, Role: "assistant", Content: "a",
					InputTokens: ptr(100), OutputTokens: ptr(20), CacheReadTokens: ptr(1000), CacheCreationTokens: ptr(7)},
				{ConversationID: conversationID, Role: "assistant", Content: "b",
					InputTokens: ptr(50), OutputTokens: ptr(5), CacheReadTokens: ptr(500), CacheCreationTokens: ptr(3)},
			} {
				if _, err := store.InsertMessage(ctx, orgID, m); err != nil {
					t.Fatalf("InsertMessage: %v", err)
				}
			}
			return conversationID
		}
		assertRolledUp := func(t *testing.T, store db.ConversationStore, orgID, conversationID string) {
			t.Helper()
			got, err := store.Get(context.Background(), orgID, conversationID)
			if err != nil || got == nil {
				t.Fatalf("Get: err=%v got=%v", err, got)
			}
			if got.InputTokens != 150 || got.OutputTokens != 25 || got.CacheReadTokens != 1500 || got.CacheCreationTokens != 10 {
				t.Errorf("token cols = (%d,%d,%d,%d), want (150,25,1500,10) — ledger SUM must survive the terminal write",
					got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheCreationTokens)
			}
		}

		t.Run("ParkOpen_stopped", func(t *testing.T) {
			store, orgID, _, seed := mk(t)
			conversationID := seedRunningWithTokens(t, store, orgID, seed)
			ok, err := store.ParkOpen(context.Background(), orgID, conversationID, db.ParkStopped(domain.ParkReasonUserCancelled, "cancelled"))
			if err != nil || !ok {
				t.Fatalf("ParkOpen stopped: ok=%v err=%v", ok, err)
			}
			assertRolledUp(t, store, orgID, conversationID)
		})

		t.Run("MarkFailedIfActive", func(t *testing.T) {
			store, orgID, _, seed := mk(t)
			conversationID := seedRunningWithTokens(t, store, orgID, seed)
			ok, err := store.MarkFailedIfActive(context.Background(), orgID, conversationID, "")
			if err != nil || !ok {
				t.Fatalf("MarkFailedIfActive: ok=%v err=%v", ok, err)
			}
			assertRolledUp(t, store, orgID, conversationID)
		})
	})

	t.Run("MarkFailedIfActive_PersistsFailureKind", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		// Kind supplied → hydrated back typed on Get.
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		ok, err := store.MarkFailedIfActive(ctx, orgID, conversationID, string(domain.ConversationFailureMemoryLimit))
		if err != nil || !ok {
			t.Fatalf("MarkFailedIfActive with kind: ok=%v err=%v", ok, err)
		}
		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: conversation=%v err=%v", got, err)
		}
		if got.FailureKind != domain.ConversationFailureMemoryLimit {
			t.Errorf("FailureKind = %q, want %q", got.FailureKind, domain.ConversationFailureMemoryLimit)
		}

		// Empty kind → NULL → hydrates as the unclassified zero value.
		plainID := seedConversationForTest(t, orgID, seed, "running")
		if ok, err := store.MarkFailedIfActive(ctx, orgID, plainID, ""); err != nil || !ok {
			t.Fatalf("MarkFailedIfActive without kind: ok=%v err=%v", ok, err)
		}
		if got, _ := store.Get(ctx, orgID, plainID); got == nil || got.FailureKind != domain.ConversationFailureUnclassified {
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

		conversationID := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.Complete(ctx, orgID, conversationID, "failed", 0, 0, 0, "", "", "", string(domain.ConversationFailureAgentError)); err != nil {
			t.Fatalf("Complete with kind: %v", err)
		}
		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: conversation=%v err=%v", got, err)
		}
		if got.Status != "failed" {
			t.Errorf("status = %q, want failed", got.Status)
		}
		if got.FailureKind != domain.ConversationFailureAgentError {
			t.Errorf("FailureKind = %q, want %q", got.FailureKind, domain.ConversationFailureAgentError)
		}

		// Empty kind on a failed Complete → NULL → unclassified zero value.
		plainID := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.Complete(ctx, orgID, plainID, "failed", 0, 0, 0, "", "", "", ""); err != nil {
			t.Fatalf("Complete without kind: %v", err)
		}
		if got, _ := store.Get(ctx, orgID, plainID); got == nil || got.FailureKind != domain.ConversationFailureUnclassified {
			t.Errorf("FailureKind on unclassified Complete failure = %q, want empty", got.FailureKind)
		}
	})

	t.Run("SetSession_PersistsSessionID", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.SetSession(ctx, orgID, conversationID, "sess-abc"); err != nil {
			t.Fatalf("SetSession: %v", err)
		}
		got, _ := store.Get(ctx, orgID, conversationID)
		if got == nil || got.SessionID != "sess-abc" {
			t.Errorf("sdk_session_id = %q, want sess-abc", got.SessionID)
		}
	})

	// worktree_path is half of what an SDK wake needs: the cwd `claude
	// --resume` keys its session storage by. Every claim of a conversation
	// re-stamps it — a warm re-claim with the same value, a cold rehydrate with
	// the tree it just rebuilt — so the write has to be an unconditional
	// overwrite on both dialects, not a set-once.
	//
	// The conversation here has no claim on it at all, which is the other half
	// of what this asserts: the unfenced door still works for a writer with no
	// engagement in scope. Its fenced twin covers the writer that has one.
	t.Run("SetWorktreePathSystem_StampsAndRestamps", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.SetWorktreePathSystem(ctx, orgID, conversationID, "/tmp/triagefactory-runs/warm"); err != nil {
			t.Fatalf("SetWorktreePathSystem: %v", err)
		}
		got, _ := store.Get(ctx, orgID, conversationID)
		if got == nil || got.WorktreePath != "/tmp/triagefactory-runs/warm" {
			t.Fatalf("worktree_path = %v, want /tmp/triagefactory-runs/warm", got)
		}
		if _, err := store.SetWorktreePathSystem(ctx, orgID, conversationID, "/tmp/triagefactory-runs/rebuilt"); err != nil {
			t.Fatalf("re-stamp: %v", err)
		}
		got, _ = store.Get(ctx, orgID, conversationID)
		if got == nil || got.WorktreePath != "/tmp/triagefactory-runs/rebuilt" {
			t.Errorf("worktree_path after a rehydrate onto a fresh path = %v, want /tmp/triagefactory-runs/rebuilt", got)
		}
	})

	t.Run("MarkOpen_FlipsRunning_RefusesTerminal", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		// Running → ok=true.
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle())
		if err != nil || !ok {
			t.Fatalf("ParkOpen on running: ok=%v err=%v", ok, err)
		}
		got, _ := store.Get(ctx, orgID, conversationID)
		if got.Status != "open" {
			t.Errorf("status = %q, want open", got.Status)
		}
		// Already open → ok=false (terminal-list excludes open, so an
		// idempotent re-call refuses).
		ok, err = store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle())
		if err != nil || ok {
			t.Errorf("re-call on open: ok=%v err=%v, want false/nil", ok, err)
		}
		// Terminal → ok=false.
		conversationID2 := seedConversationForTest(t, orgID, seed, "completed")
		ok, err = store.ParkOpen(ctx, orgID, conversationID2, db.ParkIdle())
		if err != nil || ok {
			t.Errorf("on completed: ok=%v err=%v, want false/nil", ok, err)
		}
	})

	t.Run("MarkQueuedForResume_CASGuard", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		// running → refused (only parked/abort states are resumable).
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		if ok, err := store.MarkQueuedForResume(ctx, orgID, conversationID); err != nil || ok {
			t.Errorf("on running: ok=%v err=%v, want false/nil", ok, err)
		}
		// open → ok, flips to queued. This flip must NOT stamp resume-time
		// ownership — the row goes back through ClaimNextConversation, which mints
		// the claim for whichever executor actually claims it. (The advisory
		// placement stamp it DOES write is a preference, not ownership — see
		// MarkQueuedForResume_StampsTheLastEngagementsExecutor.)
		if _, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle()); err != nil {
			t.Fatalf("open: %v", err)
		}
		if ok, err := store.MarkQueuedForResume(ctx, orgID, conversationID); err != nil || !ok {
			t.Fatalf("from open: ok=%v err=%v, want true", ok, err)
		}
		conv, err := store.GetSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("get after requeue: %v", err)
		}
		if conv.Status != "queued" {
			t.Errorf("status = %q, want queued", conv.Status)
		}
		if conv.ExecutorID != "" {
			t.Errorf("queued row carries an owner: executor_id=%q, want empty", conv.ExecutorID)
		}
		// The CAS loser: a second flip on the now-queued row finds no
		// resumable state and refuses — this is the guard that makes two
		// concurrent wakes resolve to exactly one requeue
		// (the loser surfaces ErrConversationNotResumable at the delegate layer).
		if ok, _ := store.MarkQueuedForResume(ctx, orgID, conversationID); ok {
			t.Errorf("second flip on a queued row succeeded; want refused (CAS loser)")
		}

		// completed → ok, whatever the outcome, once the blueprint that owned
		// the step has stopped. The agent stopping mid-work and the agent
		// finishing are both states a person can follow up on, and both left a
		// workspace behind to follow up in. Settling the blueprint first is the
		// order the reactor writes in; the window between the two writes is the
		// subtest below.
		abortConversation, abortBR := seedConversationWithBlueprintForTest(t, orgID, seed, "running")
		if _, err := store.Complete(ctx, orgID, abortConversation, "completed", 0, 0, 0, "stopped", "abort", "needs a human", ""); err != nil {
			t.Fatalf("complete+abort: %v", err)
		}
		seed.SetBlueprintRunStatus(t, abortBR, "aborted")
		if ok, err := store.MarkQueuedForResume(ctx, orgID, abortConversation); err != nil || !ok {
			t.Errorf("from completed+abort: ok=%v err=%v, want true", ok, err)
		}
		finishConversation, finishBR := seedConversationWithBlueprintForTest(t, orgID, seed, "running")
		if _, err := store.Complete(ctx, orgID, finishConversation, "completed", 0, 0, 0, "shipped", "finish", "", ""); err != nil {
			t.Fatalf("complete+finish: %v", err)
		}
		seed.SetBlueprintRunStatus(t, finishBR, "completed")
		if ok, err := store.MarkQueuedForResume(ctx, orgID, finishConversation); err != nil || !ok {
			t.Errorf("from completed+finish: ok=%v err=%v, want true — a follow-up on concluded work", ok, err)
		}
		// failed → refused. The infrastructure under it died, so there is no
		// workspace to land a follow-up in; this is the one rest state the CAS
		// still excludes.
		failedConversation := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.Complete(ctx, orgID, failedConversation, "failed", 0, 0, 0, "", "", "", ""); err != nil {
			t.Fatalf("fail: %v", err)
		}
		if ok, _ := store.MarkQueuedForResume(ctx, orgID, failedConversation); ok {
			t.Errorf("from failed succeeded; want refused")
		}
	})

	// The window between a step's terminal write and its blueprint reacting to
	// it, where the conversation reads `completed` to every status-only gate and
	// only a statement that also reads the parent can tell the difference.
	t.Run("MarkQueuedForResume_RefusesAConcludedStepOfARunningBlueprint", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		conversationID, brID := seedConversationWithBlueprintForTest(t, orgID, seed, "running")
		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 0, 0, 0, "handed off", "continue", "", ""); err != nil {
			t.Fatalf("complete step: %v", err)
		}
		// The blueprint has not reacted yet — exactly the live failure.
		if ok, err := store.MarkQueuedForResume(ctx, orgID, conversationID); err != nil || ok {
			t.Fatalf("mid-handoff: ok=%v err=%v, want false/nil — the reactor still has to read this terminal", ok, err)
		}
		got, err := store.GetSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("get after refused flip: %v", err)
		}
		if got.Status != "completed" {
			t.Errorf("status = %q, want completed — a refused CAS writes nothing", got.Status)
		}

		// Once the sequence stops, the same call lands: the step it came to
		// rest on is the follow-up's home.
		seed.SetBlueprintRunStatus(t, brID, "completed")
		if ok, err := store.MarkQueuedForResume(ctx, orgID, conversationID); err != nil || !ok {
			t.Fatalf("after the blueprint settled: ok=%v err=%v, want true", ok, err)
		}
	})

	// Why the guard is spelled per-arm rather than over the whole statement: a
	// PARKED step of a running blueprint is a paused step, not a concluded one,
	// so the `open` arm stays unconditional.
	t.Run("MarkQueuedForResume_WakesAParkedStepOfARunningBlueprint", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		conversationID, _ := seedConversationWithBlueprintForTest(t, orgID, seed, "running")
		if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkStopped(domain.ParkReasonUserCancelled, "")); err != nil || !ok {
			t.Fatalf("park: ok=%v err=%v", ok, err)
		}
		if ok, err := store.MarkQueuedForResume(ctx, orgID, conversationID); err != nil || !ok {
			t.Fatalf("stop→resume under a running blueprint: ok=%v err=%v, want true", ok, err)
		}
	})

	// The resume flip chases the warm workspace: the executor whose engagement
	// drove the conversation last is the one holding its tree (and, on the SDK
	// runtime, its session file), so that is what the flip stamps as the
	// placement preference. Nothing here depends on placement being ON — the
	// column is written in both dialects; only multi-mode reads it.
	t.Run("MarkQueuedForResume_StampsTheLastEngagementsExecutor", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		resume := func(t *testing.T, conversationID string) {
			t.Helper()
			if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkStopped(domain.ParkReasonUserCancelled, "")); err != nil || !ok {
				t.Fatalf("park: ok=%v err=%v", ok, err)
			}
			if ok, err := store.MarkQueuedForResume(ctx, orgID, conversationID); err != nil || !ok {
				t.Fatalf("MarkQueuedForResume: ok=%v err=%v", ok, err)
			}
		}

		// One engagement: its executor is the stamp.
		single := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.SetExecutorSystem(ctx, orgID, single, "exec-1", 1); err != nil {
			t.Fatalf("SetExecutorSystem exec-1: %v", err)
		}
		resume(t, single)
		if got := seed.PreferredExecutor(t, single); got != "exec-1" {
			t.Errorf("preferred_executor_id after a resume = %q, want exec-1 — the executor holding the warm tree", got)
		}

		// Two engagements: the NEWER one holds the tree, so an older
		// engagement's executor must not win.
		twice := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.SetExecutorSystem(ctx, orgID, twice, "exec-1", 1); err != nil {
			t.Fatalf("SetExecutorSystem exec-1: %v", err)
		}
		// The empty stamp releases the claim; without it the next call would
		// update that engagement in place instead of starting a second one.
		if _, err := store.SetExecutorSystem(ctx, orgID, twice, "", 0); err != nil {
			t.Fatalf("release the first engagement: %v", err)
		}
		if _, err := store.SetExecutorSystem(ctx, orgID, twice, "exec-2", 2); err != nil {
			t.Fatalf("SetExecutorSystem exec-2: %v", err)
		}
		resume(t, twice)
		if got := seed.PreferredExecutor(t, twice); got != "exec-2" {
			t.Errorf("preferred_executor_id after a second engagement = %q, want exec-2 (the newest claim)", got)
		}

		// Never claimed: nothing ever drove it, so there is no warmth to
		// chase and the row re-queues unowned.
		never := seedConversationForTest(t, orgID, seed, "running")
		resume(t, never)
		if got := seed.PreferredExecutor(t, never); got != "" {
			t.Errorf("preferred_executor_id on a never-claimed conversation = %q, want empty", got)
		}
	})

	t.Run("MarkQueuedForResume_ReStampsQueuedAt", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		// An hour-old queue episode behind an hour-old mint: the wake must
		// start a new episode without moving the mint.
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		seed.BackdateStartedAt(t, conversationID, time.Hour)
		seed.BackdateQueuedAt(t, conversationID, time.Hour)
		before, err := store.Get(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("Get before: %v", err)
		}
		if before.QueuedAt == nil {
			t.Fatal("seeded conversation has no queued_at to re-stamp")
		}

		if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkStopped(domain.ParkReasonUserCancelled, "")); err != nil || !ok {
			t.Fatalf("park: ok=%v err=%v", ok, err)
		}
		woke := time.Now()
		if ok, err := store.MarkQueuedForResume(ctx, orgID, conversationID); err != nil || !ok {
			t.Fatalf("MarkQueuedForResume: ok=%v err=%v", ok, err)
		}
		after, err := store.Get(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("Get after: %v", err)
		}
		if after.QueuedAt == nil {
			t.Fatal("queued_at is NULL after the wake")
		}
		// A generous tolerance: the two clocks (Go's and the backend's) and
		// SQLite's whole-second column format only have to agree that the
		// stamp is from the wake, not from an hour ago.
		if after.QueuedAt.Before(woke.Add(-time.Minute)) {
			t.Errorf("queued_at after the wake = %v, want re-stamped at the wake (%v); still carries the old episode (%v)", after.QueuedAt, woke, before.QueuedAt)
		}
		if !after.QueuedAt.After(*before.QueuedAt) {
			t.Errorf("queued_at after the wake = %v, not later than the seeded episode's %v", after.QueuedAt, before.QueuedAt)
		}
		if !after.StartedAt.Equal(before.StartedAt) {
			t.Errorf("started_at moved on the wake: %v -> %v; the mint stamp is the scheduler's fairness order", before.StartedAt, after.StartedAt)
		}
	})

	t.Run("MarkFailedIfActive_FailsOpen_RefusesTerminal", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		// `open` is intentionally failable (a warm open conversation has no
		// durable snapshot, so an infra error reaching failConversation must
		// terminate it).
		openConversation := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.ParkOpen(ctx, orgID, openConversation, db.ParkIdle()); err != nil {
			t.Fatalf("open: %v", err)
		}
		ok, err := store.MarkFailedIfActive(ctx, orgID, openConversation, "")
		if err != nil || !ok {
			t.Fatalf("MarkFailedIfActive on open: ok=%v err=%v, want true (open is failable)", ok, err)
		}
		if got, _ := store.Get(ctx, orgID, openConversation); got.Status != "failed" {
			t.Errorf("status = %q, want failed", got.Status)
		}
		// Already terminal → refused.
		doneConversation := seedConversationForTest(t, orgID, seed, "completed")
		if ok, _ := store.MarkFailedIfActive(ctx, orgID, doneConversation, ""); ok {
			t.Errorf("MarkFailedIfActive flipped a completed conversation; want refused")
		}
	})

	// A cancel PARKS the conversation `open` and keeps the workspace; the
	// cancellation itself is recorded by the blueprint layer and by the claim's
	// outcome. `cancelled` is not a status either dialect can produce.
	t.Run("ParkOpen_Stopped_ParksActive_RefusesTerminal", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkStopped(domain.ParkReasonUserCancelled, "user cancelled"))
		if err != nil || !ok {
			t.Fatalf("cancel active: ok=%v err=%v", ok, err)
		}
		got, _ := store.Get(ctx, orgID, conversationID)
		if got.Status != "open" || got.ParkReason != domain.ParkReasonUserCancelled {
			t.Errorf("after cancel: status=%q park_reason=%q, want (open, user_cancelled)", got.Status, got.ParkReason)
		}
		// The workspace is retained and the retention TTL is what collects it,
		// so the parked row must enumerate as a reapable snapshot key. That is
		// also the parked_at assertion: the sweep keys off it.
		keys, err := store.ListReapableSnapshotKeysSystem(ctx, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("ListReapableSnapshotKeysSystem: %v", err)
		}
		found := false
		for _, k := range keys {
			if k.BlueprintRunID == got.BlueprintRunID {
				found = true
			}
		}
		if !found {
			t.Errorf("cancel-parked conversation's snapshot key %q is not reapable; its workspace blob would leak forever", got.BlueprintRunID)
		}
		// A parked conversation cancels again (the gesture still has to finalize the
		// blueprint), but a terminal one is refused.
		if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkStopped(domain.ParkReasonUserCancelled, "")); err != nil || !ok {
			t.Errorf("re-cancel a parked conversation: ok=%v err=%v, want true/nil", ok, err)
		}
		doneConversation := seedConversationForTest(t, orgID, seed, "completed")
		if ok, err := store.ParkOpen(ctx, orgID, doneConversation, db.ParkStopped(domain.ParkReasonUserCancelled, "")); err != nil || ok {
			t.Errorf("cancel a completed conversation: ok=%v err=%v, want false/nil", ok, err)
		}
	})

	// An idle park is the other half of the same method, and its two
	// differences from a stop are both load-bearing: the claim releases
	// 'parked' rather than 'cancelled', and an already-parked row is left
	// alone (the live driver parks on every no-conclusion turn, so a flip
	// there would re-broadcast on every one).
	t.Run("ParkOpen_Idle_DoesNotRepark", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle()); err != nil || !ok {
			t.Fatalf("idle park: ok=%v err=%v", ok, err)
		}
		if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle()); err != nil || ok {
			t.Errorf("second idle park: ok=%v err=%v, want false/nil — a re-park must not re-broadcast", ok, err)
		}
		// A deliberate stop DOES land on the same parked row: the caller has
		// to learn it took so it can finalize the blueprint behind it.
		if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkStopped(domain.ParkReasonUserCancelled, "stopped")); err != nil || !ok {
			t.Errorf("stop on a parked row: ok=%v err=%v, want true/nil", ok, err)
		}
		got, _ := store.Get(ctx, orgID, conversationID)
		if got.ParkReason != domain.ParkReasonUserCancelled {
			t.Errorf("park_reason = %q, want user_cancelled", got.ParkReason)
		}
		// …and an idle park after it must not overwrite the reason it
		// recorded. An idle park carries `idle`, so this is a real write that
		// the already-parked guard is what stops.
		if _, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle()); err != nil {
			t.Fatalf("idle park after stop: %v", err)
		}
		if got, _ := store.Get(ctx, orgID, conversationID); got.ParkReason != domain.ParkReasonUserCancelled {
			t.Errorf("park_reason = %q after an idle park, want it preserved", got.ParkReason)
		}
	})

	// Every reason in the vocabulary round-trips through both dialects.
	// Coverage is derived from domain.AllParkReasons() rather than copied out
	// of it, so a reason added in Go and never taught to a store fails here on
	// both backends — the discipline AllClaimPhases is held to, for the same
	// reason: SQL cannot import a Go const, so this suite is the join.
	t.Run("ParkOpen_RoundTripsEveryParkReason", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		for _, reason := range domain.AllParkReasons() {
			conversationID := seedConversationForTest(t, orgID, seed, "running")
			if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkStopped(reason, "")); err != nil || !ok {
				t.Fatalf("park with %q: ok=%v err=%v", reason, ok, err)
			}
			got, err := store.Get(ctx, orgID, conversationID)
			if err != nil || got == nil {
				t.Fatalf("Get after park with %q: err=%v got=%v", reason, err, got)
			}
			if got.ParkReason != reason {
				t.Errorf("park_reason = %q, want %q", got.ParkReason, reason)
			}
		}
	})

	// The discriminator the Park type now carries explicitly. It used to be
	// inferred from the reason being non-empty, which made "someone stopped
	// this" and "there is something to display" one bit: an idle park that
	// wanted to say WHY it parked could only do so by also releasing its claim
	// as a cancellation. `idle` is exactly such a reason, so this is the arm
	// that would have silently broken.
	t.Run("ParkOpen_IdleRecordsItsReasonAndStillReleasesParked", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-idle-park", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}

		if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle()); err != nil || !ok {
			t.Fatalf("idle park: ok=%v err=%v", ok, err)
		}
		got, _ := store.Get(ctx, orgID, conversationID)
		if got.ParkReason != domain.ParkReasonIdle {
			t.Errorf("park_reason = %q, want idle — an idle park says why it parked too", got.ParkReason)
		}
		claims := seed.ClaimRows(t, conversationID)
		if len(claims) != 1 || !claims[0].Released || claims[0].Outcome != "parked" {
			t.Fatalf("claims = %+v, want the engagement released as parked, never cancelled", claims)
		}
	})

	// The completed-only blueprint: every step concluded cleanly, nothing parked
	// and nothing aborted. It is the case the retention query used to omit
	// wholesale — its rows matched neither `open` nor completed+abort, so the
	// blueprint appeared in no group and its shared workspace blob was never
	// reaped. Now that a clean completion snapshots, that omission is a permanent
	// leak rather than a harmless gap, so `completed` is reapable whatever the
	// outcome, and both halves of the TTL are pinned here: past the cutoff the key
	// enumerates, before it the key does not.
	t.Run("ListReapableSnapshotKeys_CompletedOnlyBlueprint", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "reap-completed-only")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)

		// Two steps of ONE blueprint — they share a snapshot key, so the sweep
		// has to reason about the blueprint rather than the row.
		bpr := seed.BlueprintRun(t, taskID)
		mkStep := func() string {
			return seed.Conversation(t, domain.Conversation{
				TaskID: taskID, PromptID: conversationTestPrompt(t), Status: "running",
				Model: "m", BlueprintRunID: bpr,
			})
		}
		step1, step2 := mkStep(), mkStep()
		if _, err := store.Complete(ctx, orgID, step1, "completed", 0, 0, 0, "handed off", "continue", "", ""); err != nil {
			t.Fatalf("complete step 1: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, step2, "completed", 0, 0, 0, "shipped it", "finish", "", ""); err != nil {
			t.Fatalf("complete step 2: %v", err)
		}

		// A cutoff in the future is "the whole blueprint has aged out".
		aged, err := store.ListReapableSnapshotKeysSystem(ctx, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("ListReapableSnapshotKeysSystem(aged): %v", err)
		}
		if !reapKeysContain(aged, bpr) {
			t.Errorf("blueprint %s whose every step completed is not reapable past the TTL; its workspace blob would never be collected", bpr)
		}

		// …and one in the past is "nothing has aged out yet".
		fresh, err := store.ListReapableSnapshotKeysSystem(ctx, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("ListReapableSnapshotKeysSystem(fresh): %v", err)
		}
		if reapKeysContain(fresh, bpr) {
			t.Errorf("just-completed blueprint %s is already reapable; the TTL has not elapsed", bpr)
		}
	})

	// Workspace eviction's enumeration. The warm tree is a cache whose only
	// licence to be deleted is that the durable blob exists, so every arm here
	// is about refusing rather than finding: the safety gates are the test.
	t.Run("ListEvictableWorkspaces_WrittenSnapshotAtRestAndUnclaimed", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID, bpr := seedConversationWithBlueprintForTest(t, orgID, seed, "running")
		const wtPath = "/tmp/triagefactory-runs/evictable"
		if _, err := store.SetWorktreePathSystem(ctx, orgID, conversationID, wtPath); err != nil {
			t.Fatalf("SetWorktreePathSystem: %v", err)
		}
		if _, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle()); err != nil {
			t.Fatalf("ParkOpen: %v", err)
		}
		seed.SetSnapshotState(t, bpr, domain.WorkspaceSnapshotWritten)

		got := evictableFor(t, store, ctx, time.Now().Add(time.Hour), bpr)
		if got == nil {
			t.Fatalf("parked key %s with a written snapshot is not evictable; its warm tree would be reclaimed only by a restart", bpr)
		}
		if len(got.WorktreePaths) != 1 || got.WorktreePaths[0] != wtPath {
			t.Errorf("worktree paths = %v, want [%s] — the caller evicts by path and has no other source for it", got.WorktreePaths, wtPath)
		}
		if got.OrgID != orgID {
			t.Errorf("org = %q, want %q", got.OrgID, orgID)
		}

		// The TTL half: a cutoff before the park is "nothing has been idle
		// long enough yet".
		if got := evictableFor(t, store, ctx, time.Now().Add(-time.Hour), bpr); got != nil {
			t.Errorf("just-parked key %s is already evictable; the idle window has not elapsed", bpr)
		}
	})

	t.Run("ListEvictableWorkspaces_RefusesUnlessSnapshotIsWritten", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		// A key whose snapshot never got a lifecycle row at all: the tree is
		// the only copy of the agent's work, and nothing says otherwise.
		absent := parkedEvictionCandidate(t, store, ctx, orgID, seed, "absent")
		if got := evictableFor(t, store, ctx, time.Now().Add(time.Hour), absent); got != nil {
			t.Errorf("key %s with no snapshot state row is evictable; its tree is the only copy of the work", absent)
		}
		for _, state := range []string{domain.WorkspaceSnapshotPending, domain.WorkspaceSnapshotFailed} {
			bpr := parkedEvictionCandidate(t, store, ctx, orgID, seed, "state-"+state)
			seed.SetSnapshotState(t, bpr, state)
			if got := evictableFor(t, store, ctx, time.Now().Add(time.Hour), bpr); got != nil {
				t.Errorf("key %s with snapshot state %q is evictable; only a written blob makes the tree a cache", bpr, state)
			}
		}
	})

	// Blueprint steps share one tree, so any live engagement anywhere under the
	// key is working in the directory this enumeration would hand to a delete.
	t.Run("ListEvictableWorkspaces_RefusesWhileAnyStepIsClaimed", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "evict-shared")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		bpr := seed.BlueprintRun(t, taskID)
		mkStep := func() string {
			return seed.Conversation(t, domain.Conversation{
				TaskID: taskID, PromptID: conversationTestPrompt(t), Status: "running",
				Model: "m", BlueprintRunID: bpr,
			})
		}
		parked, live := mkStep(), mkStep()
		if _, err := store.SetWorktreePathSystem(ctx, orgID, parked, "/tmp/triagefactory-runs/"+bpr); err != nil {
			t.Fatalf("SetWorktreePathSystem: %v", err)
		}
		if _, err := store.ParkOpen(ctx, orgID, parked, db.ParkIdle()); err != nil {
			t.Fatalf("ParkOpen: %v", err)
		}
		seed.SetSnapshotState(t, bpr, domain.WorkspaceSnapshotWritten)

		// The sibling step is mid-engagement.
		if _, err := store.SetExecutorSystem(ctx, orgID, live, "exec-live", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		if got := evictableFor(t, store, ctx, time.Now().Add(time.Hour), bpr); got != nil {
			t.Errorf("key %s is evictable while a sibling step holds a live claim; the tree would vanish under a running agent", bpr)
		}
		has, err := store.HasActiveClaimForBlueprintRunSystem(ctx, orgID, bpr)
		if err != nil || !has {
			t.Fatalf("HasActiveClaimForBlueprintRunSystem with a live sibling = %v (err %v), want true", has, err)
		}

		// Release it and the key becomes evictable — the claim, not the
		// sibling's existence, is what held it back.
		if _, err := store.SetExecutorSystem(ctx, orgID, live, "", 0); err != nil {
			t.Fatalf("release claim: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, live, "completed", 0, 0, 0, "done", "finish", "", ""); err != nil {
			t.Fatalf("complete sibling: %v", err)
		}
		if got := evictableFor(t, store, ctx, time.Now().Add(time.Hour), bpr); got == nil {
			t.Errorf("key %s is not evictable once every step is at rest and unclaimed", bpr)
		}
		has, err = store.HasActiveClaimForBlueprintRunSystem(ctx, orgID, bpr)
		if err != nil || has {
			t.Fatalf("HasActiveClaimForBlueprintRunSystem after release = %v (err %v), want false", has, err)
		}
	})

	// A conversation that never recorded a path names no tree, so there is
	// nothing on disk for the sweep to act on — enumerating it would hand the
	// caller a key it can only skip.
	t.Run("ListEvictableWorkspaces_SkipsRowsWithNoWorktreePath", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID, bpr := seedConversationWithBlueprintForTest(t, orgID, seed, "running")
		if _, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle()); err != nil {
			t.Fatalf("ParkOpen: %v", err)
		}
		seed.SetSnapshotState(t, bpr, domain.WorkspaceSnapshotWritten)
		if got := evictableFor(t, store, ctx, time.Now().Add(time.Hour), bpr); got != nil {
			t.Errorf("key %s with no recorded worktree_path enumerated as %+v; it names no tree to evict", bpr, got)
		}
	})

	t.Run("SetExecutorSystem_MintsUpdatesReleasesActiveClaim", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		// No claim yet → the go-live confirmation mints one.
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-a", 3); err != nil {
			t.Fatalf("SetExecutorSystem mint: %v", err)
		}
		claims := seed.ClaimRows(t, conversationID)
		if len(claims) != 1 || claims[0].ExecutorID != "exec-a" || claims[0].BootEpoch != 3 || claims[0].Released {
			t.Fatalf("after mint: claims = %+v, want one active (exec-a, 3)", claims)
		}

		// A live claim → idempotent identity update, no second row.
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-a", 4); err != nil {
			t.Fatalf("SetExecutorSystem update: %v", err)
		}
		claims = seed.ClaimRows(t, conversationID)
		if len(claims) != 1 || claims[0].BootEpoch != 4 || claims[0].Released {
			t.Fatalf("after update: claims = %+v, want the same single active claim at epoch 4", claims)
		}
		if got, _ := store.Get(ctx, orgID, conversationID); got.ExecutorID != "exec-a" || got.Attempts != 1 {
			t.Errorf("Get after update: executor=%q attempts=%d, want exec-a/1", got.ExecutorID, got.Attempts)
		}

		// Empty executorID → the legacy clear: the active claim releases
		// as requeued and the read-side owner goes empty.
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "", 0); err != nil {
			t.Fatalf("SetExecutorSystem clear: %v", err)
		}
		claims = seed.ClaimRows(t, conversationID)
		if len(claims) != 1 || !claims[0].Released || claims[0].Outcome != "requeued" {
			t.Fatalf("after clear: claims = %+v, want one released claim with outcome requeued", claims)
		}
		got, _ := store.Get(ctx, orgID, conversationID)
		if got.ExecutorID != "" {
			t.Errorf("ExecutorID after release = %q, want empty", got.ExecutorID)
		}
		if got.Attempts != 1 {
			t.Errorf("Attempts after release = %d, want 1 (released claims still count)", got.Attempts)
		}
		// This suite pins the DISPLAY read's Attempts, which is the lifetime
		// engagement count. The claim path returns a different quantity under
		// the same name — the retry budget's current queue episode, pinned in
		// RunClaimPredicateConformance. Neither is the other's regression.
		if got.ClaimedAt == nil {
			t.Errorf("ClaimedAt after release = nil, want the released claim's claimed_at")
		}

		// A re-mint after release is a NEW claim — attempts becomes the
		// lifetime claim count, and one-active-claim holds (the released row
		// stays).
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-b", 5); err != nil {
			t.Fatalf("SetExecutorSystem re-mint: %v", err)
		}
		claims = seed.ClaimRows(t, conversationID)
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
		if got, _ := store.Get(ctx, orgID, conversationID); got.ExecutorID != "exec-b" || got.Attempts != 2 {
			t.Errorf("Get after re-mint: executor=%q attempts=%d, want exec-b/2", got.ExecutorID, got.Attempts)
		}
	})

	t.Run("TerminalWrites_ReleaseActiveClaimWithOutcome", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		assertReleased := func(t *testing.T, conversationID, wantOutcome string) {
			t.Helper()
			claims := seed.ClaimRows(t, conversationID)
			if len(claims) != 1 {
				t.Fatalf("claims = %+v, want exactly 1", claims)
			}
			if !claims[0].Released || claims[0].Outcome != wantOutcome {
				t.Errorf("claim = %+v, want released with outcome %q", claims[0], wantOutcome)
			}
			if got, _ := store.Get(ctx, orgID, conversationID); got.ExecutorID != "" {
				t.Errorf("ExecutorID after release = %q, want empty", got.ExecutorID)
			}
		}
		stage := func(t *testing.T) string {
			t.Helper()
			conversationID := seedConversationForTest(t, orgID, seed, "running")
			if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-term", 1); err != nil {
				t.Fatalf("SetExecutorSystem: %v", err)
			}
			return conversationID
		}

		t.Run("Complete_completed", func(t *testing.T) {
			conversationID := stage(t)
			if _, err := store.Complete(ctx, orgID, conversationID, "completed", 0, 0, 0, "", "finish", "", ""); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			assertReleased(t, conversationID, "completed")
		})
		t.Run("Complete_failed", func(t *testing.T) {
			conversationID := stage(t)
			if _, err := store.Complete(ctx, orgID, conversationID, "failed", 0, 0, 0, "", "", "", ""); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			assertReleased(t, conversationID, "failed")
		})
		t.Run("ParkOpen_stopped", func(t *testing.T) {
			conversationID := stage(t)
			if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkStopped(domain.ParkReasonUserCancelled, "")); err != nil || !ok {
				t.Fatalf("ParkOpen stopped: ok=%v err=%v", ok, err)
			}
			assertReleased(t, conversationID, "cancelled")
		})
		t.Run("MarkFailedIfActive", func(t *testing.T) {
			conversationID := stage(t)
			if ok, err := store.MarkFailedIfActive(ctx, orgID, conversationID, ""); err != nil || !ok {
				t.Fatalf("MarkFailedIfActive: ok=%v err=%v", ok, err)
			}
			assertReleased(t, conversationID, "failed")
		})
		t.Run("ParkOpen_idle_parks", func(t *testing.T) {
			conversationID := stage(t)
			if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle()); err != nil || !ok {
				t.Fatalf("ParkOpen: ok=%v err=%v", ok, err)
			}
			assertReleased(t, conversationID, "parked")
		})
		t.Run("MarkQueuedForResume_requeues", func(t *testing.T) {
			conversationID := stage(t)
			if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle()); err != nil || !ok {
				t.Fatalf("ParkOpen: ok=%v err=%v", ok, err)
			}
			// ParkOpen already released as 'parked'; re-mint so the requeue
			// has an active claim to release.
			if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-park", 2); err != nil {
				t.Fatalf("SetExecutorSystem re-mint: %v", err)
			}
			if ok, err := store.MarkQueuedForResume(ctx, orgID, conversationID); err != nil || !ok {
				t.Fatalf("MarkQueuedForResume: ok=%v err=%v", ok, err)
			}
			claims := seed.ClaimRows(t, conversationID)
			if len(claims) != 2 {
				t.Fatalf("claims = %+v, want 2 (parked, then requeued)", claims)
			}
			last := claims[len(claims)-1]
			if !last.Released || last.Outcome != "requeued" {
				t.Errorf("latest claim = %+v, want released with outcome requeued", last)
			}
		})
		t.Run("RefusedWrite_LeavesClaimActive", func(t *testing.T) {
			// A guarded no-op must not release: complete the conversation,
			// re-mint a claim (simulating a racing engagement), then fail — the
			// guard refuses and the claim stays live.
			conversationID := stage(t)
			if _, err := store.Complete(ctx, orgID, conversationID, "completed", 0, 0, 0, "", "finish", "", ""); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-race", 9); err != nil {
				t.Fatalf("SetExecutorSystem: %v", err)
			}
			if ok, _ := store.MarkFailedIfActive(ctx, orgID, conversationID, ""); ok {
				t.Fatalf("MarkFailedIfActive on completed conversation succeeded; want refused")
			}
			claims := seed.ClaimRows(t, conversationID)
			last := claims[len(claims)-1]
			if last.Released {
				t.Errorf("refused terminal write released the claim: %+v", last)
			}
		})
	})

	t.Run("SetActiveClaimPhaseSystem_SetClearAndCoalesceIntoStatus", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-phase", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		// Set: the phase lands on the active claim and the display read
		// coalesces it over the stored 'running'. Coverage is derived from
		// the canonical vocabulary rather than a copy of it, so a phase added
		// in Go and never taught to a store fails here on both dialects.
		for _, phase := range domain.AllClaimPhases() {
			if _, err := store.SetActiveClaimPhaseSystem(ctx, orgID, conversationID, phase); err != nil {
				t.Fatalf("SetActiveClaimPhaseSystem set %s: %v", phase, err)
			}
			claims := seed.ClaimRows(t, conversationID)
			if len(claims) != 1 || claims[0].Phase != phase {
				t.Fatalf("claims after set %s = %+v, want one active claim in that phase", phase, claims)
			}
			got, err := store.Get(ctx, orgID, conversationID)
			if err != nil || got == nil {
				t.Fatalf("Get: err=%v got=%v", err, got)
			}
			if got.Status != phase {
				t.Errorf("Status = %q, want %s (active claim's phase coalesced over stored status)", got.Status, phase)
			}
		}

		// Clear: empty phase writes NULL and the display falls back to the
		// stored status.
		if _, err := store.SetActiveClaimPhaseSystem(ctx, orgID, conversationID, ""); err != nil {
			t.Fatalf("SetActiveClaimPhaseSystem clear: %v", err)
		}
		if claims := seed.ClaimRows(t, conversationID); len(claims) != 1 || claims[0].Phase != "" {
			t.Fatalf("claims after clear = %+v, want the phase cleared", claims)
		}
		if got, _ := store.Get(ctx, orgID, conversationID); got.Status != "running" {
			t.Errorf("Status after clear = %q, want running", got.Status)
		}
	})

	t.Run("SetActiveClaimPhaseSystem_NoOpOnReleasedClaim", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-phase-rel", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		if _, err := store.SetActiveClaimPhaseSystem(ctx, orgID, conversationID, "agent_starting"); err != nil {
			t.Fatalf("SetActiveClaimPhaseSystem: %v", err)
		}
		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 0, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		// A write against the released claim is a silent no-op: the released
		// claim's phase stays whatever it held at release.
		if _, err := store.SetActiveClaimPhaseSystem(ctx, orgID, conversationID, "cloning"); err != nil {
			t.Fatalf("SetActiveClaimPhaseSystem on released: %v", err)
		}
		claims := seed.ClaimRows(t, conversationID)
		if len(claims) != 1 || !claims[0].Released || claims[0].Phase != "agent_starting" {
			t.Fatalf("claims = %+v, want one released claim still in phase agent_starting", claims)
		}
		// And the released claim's phase never leaks into the display: the
		// coalesce only reads the ACTIVE claim.
		if got, _ := store.Get(ctx, orgID, conversationID); got.Status != "completed" {
			t.Errorf("Status = %q, want completed (a released claim's phase is inert history)", got.Status)
		}
	})

	// The claim-fenced engagement writes, driven with a LIVE claim. What
	// every backend must agree on is that naming your own claim changes
	// nothing about the write itself: same row, same attribution, same
	// terminal, same phase. The refusal half is the subtest after this one;
	// the Postgres backend test file additionally covers the locking the
	// refusal needs against a concurrent release, which SQLite's single
	// connection makes moot.
	t.Run("ClaimFencedWrites_ActiveClaimWritesExactlyLikeTheUnfencedPath", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-fenced", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claims := seed.ClaimRows(t, conversationID)
		if len(claims) != 1 {
			t.Fatalf("claims = %+v, want the one minted engagement", claims)
		}
		claimID := claims[0].ID

		if _, err := store.SetClaimPhaseSystem(ctx, orgID, conversationID, claimID, "cloning"); err != nil {
			t.Fatalf("SetClaimPhaseSystem: %v", err)
		}
		if got := seed.ClaimRows(t, conversationID); len(got) != 1 || got[0].Phase != "cloning" {
			t.Fatalf("claims after phase write = %+v, want phase cloning on the named claim", got)
		}

		pending := false
		msgID, err := store.InsertMessageForClaimSystem(ctx, orgID, claimID, &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: "streamed",
			Delivered: &pending,
		})
		if err != nil {
			t.Fatalf("InsertMessageForClaimSystem: %v", err)
		}
		msgs, err := store.Messages(ctx, orgID, conversationID)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("Messages = %v (err %v), want the one streamed row", msgs, err)
		}
		if msgs[0].ClaimID != claimID {
			t.Errorf("row claim_id = %q, want %q (the engagement that wrote it)", msgs[0].ClaimID, claimID)
		}

		if err := store.MarkDeliveredForClaimSystem(ctx, orgID, conversationID, claimID, []int{int(msgID)}, ""); err != nil {
			t.Fatalf("MarkDeliveredForClaimSystem: %v", err)
		}
		msgs, err = store.Messages(ctx, orgID, conversationID)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("Messages after deliver = %v (err %v)", msgs, err)
		}
		if msgs[0].Delivered == nil || !*msgs[0].Delivered {
			t.Errorf("row delivered = %v, want true", msgs[0].Delivered)
		}

		if _, err := store.CompleteForClaimSystem(ctx, orgID, conversationID, claimID, "completed", 0.5, 1500, 2, "done", "finish", "", ""); err != nil {
			t.Fatalf("CompleteForClaimSystem: %v", err)
		}
		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.Status != "completed" {
			t.Errorf("status = %q, want completed", got.Status)
		}
		if got.TotalCostUSD == nil || *got.TotalCostUSD != 0.5 {
			t.Errorf("total_cost_usd = %v, want 0.5 settled on the engagement's own row", got.TotalCostUSD)
		}
		if got.DurationMs == nil || *got.DurationMs != 1500 {
			t.Errorf("duration_ms = %v, want 1500 stamped on the released claim", got.DurationMs)
		}
		after := seed.ClaimRows(t, conversationID)
		if len(after) != 1 || !after[0].Released || after[0].Outcome != "completed" {
			t.Fatalf("claims after complete = %+v, want the engagement released as completed", after)
		}
	})

	// The refusal half, on both dialects, staged the way local mode actually
	// produces it: the stop verb's own park releases the claim out from under
	// the engagement. Every fenced write must then refuse rather than land —
	// the write succeeding is the harm, so each is asserted individually, and
	// the row is checked afterwards to be exactly what the stop left.
	t.Run("ClaimFencedWrites_ReleasedClaimRefusesEveryWrite", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-stopped", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claimID := seed.ClaimRows(t, conversationID)[0].ID
		// One row the engagement wrote while it still owned the conversation,
		// so the settle/deliver refusals below have a real target to miss.
		pending := false
		ownedID, err := store.InsertMessageForClaimSystem(ctx, orgID, claimID, &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: "owned", Delivered: &pending,
		})
		if err != nil {
			t.Fatalf("InsertMessageForClaimSystem (live): %v", err)
		}

		// The stop verb's write: unfenced, parks the row, releases the claim.
		if flipped, err := store.ParkOpenSystem(ctx, orgID, conversationID, db.ParkStopped(domain.ParkReasonUserCancelled, "")); err != nil || !flipped {
			t.Fatalf("ParkOpenSystem (the stop) = (%v, %v), want (true, nil)", flipped, err)
		}
		stopped := seed.ClaimRows(t, conversationID)
		if len(stopped) != 1 || !stopped[0].Released || stopped[0].Outcome != "cancelled" {
			t.Fatalf("claims after the stop = %+v, want the one claim released as cancelled", stopped)
		}

		refuse := func(name string, err error) {
			t.Helper()
			if !errors.Is(err, db.ErrClaimReleased) {
				t.Errorf("%s after the stop = %v, want ErrClaimReleased", name, err)
			}
		}
		_, err = store.SetSessionForClaimSystem(ctx, orgID, conversationID, claimID, "sess-zombie")
		refuse("SetSessionForClaimSystem", err)
		_, err = store.SetWorktreePathForClaimSystem(ctx, orgID, conversationID, claimID, "/tmp/zombie")
		refuse("SetWorktreePathForClaimSystem", err)
		_, err = store.InsertMessageForClaimSystem(ctx, orgID, claimID, &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: "zombie", Delivered: &pending,
		})
		refuse("InsertMessageForClaimSystem", err)
		refuse("MarkDeliveredForClaimSystem", store.MarkDeliveredForClaimSystem(ctx, orgID, conversationID, claimID, []int{int(ownedID)}, ""))
		_, err = store.CompleteForClaimSystem(ctx, orgID, conversationID, claimID, "completed", 0, 0, 0, "", "finish", "", "")
		refuse("CompleteForClaimSystem", err)
		_, err = store.MarkFailedIfActiveForClaimSystem(ctx, orgID, conversationID, claimID, string(domain.ConversationFailureCrash))
		refuse("MarkFailedIfActiveForClaimSystem", err)
		_, err = store.ParkOpenForClaimSystem(ctx, orgID, conversationID, claimID, db.ParkIdle())
		refuse("ParkOpenForClaimSystem", err)
		_, err = store.SetExecutorForClaimSystem(ctx, orgID, conversationID, claimID, "exec-zombie", 2)
		refuse("SetExecutorForClaimSystem", err)
		_, err = store.SetClaimPhaseSystem(ctx, orgID, conversationID, claimID, "agent_starting")
		refuse("SetClaimPhaseSystem", err)
		refuse("CompactForClaimSystem", store.CompactForClaimSystem(ctx, orgID, conversationID, claimID, nil,
			&domain.Message{Role: "user", Content: "summary"}, []int{int(ownedID)}))
		_, err = store.SettleCompactionRequestForClaimSystem(ctx, orgID, conversationID, claimID, int(ownedID), 1, 1, 0, 0, nil, "zombie")
		refuse("SettleCompactionRequestForClaimSystem", err)

		// Nothing landed: the transcript is the one owned row, undelivered and
		// unsettled, and the conversation is the stop's `open` with its claim
		// exactly as the stop released it.
		msgs, err := store.Messages(ctx, orgID, conversationID)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("Messages = %v (err %v), want only the row written while the claim was live", msgs, err)
		}
		if msgs[0].Delivered == nil || *msgs[0].Delivered {
			t.Errorf("owned row delivered = %v, want still pending (the refused flush wrote nothing)", msgs[0].Delivered)
		}
		if _, ok := msgs[0].Metadata["compaction_failure"]; ok {
			t.Error("owned row carries compaction_failure; the refused settle wrote its metadata")
		}
		if got, _ := store.Get(ctx, orgID, conversationID); got == nil || got.Status != "open" {
			t.Errorf("status after the refusals = %v, want open (the stop's own state, untouched)", got)
		}
		after := seed.ClaimRows(t, conversationID)
		if len(after) != 1 || after[0].ID != stopped[0].ID || !after[0].Released ||
			after[0].Outcome != stopped[0].Outcome || after[0].Phase != stopped[0].Phase ||
			after[0].ExecutorID != stopped[0].ExecutorID || after[0].BootEpoch != stopped[0].BootEpoch {
			t.Errorf("claims after the refusals = %+v, want unchanged from the stop's %+v", after, stopped)
		}
	})

	// A synthetic row is sometimes written to a position rather than to the
	// tail — the claim-start repair answers an interrupted tool call
	// underneath the user rows that arrived after it, so the answer assembles
	// adjacent to the call the provider requires it to follow. That placement
	// only holds if the fenced write an engagement makes carries the caller's
	// seq through to the column; an insert that dropped it would silently
	// restore the tail-append order and reject the very next request.
	t.Run("InsertMessageForClaimSystem_HonorsExplicitSeq", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-seq", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claimID := seed.ClaimRows(t, conversationID)[0].ID
		insert := func(msg domain.Message) int {
			id, err := store.InsertMessageForClaimSystem(ctx, orgID, claimID, &msg)
			if err != nil {
				t.Fatalf("InsertMessageForClaimSystem: %v", err)
			}
			return int(id)
		}

		callID := insert(domain.Message{ConversationID: conversationID, Role: "assistant", Content: "calling",
			ToolCalls: []domain.ToolCall{{ID: "t1", Name: "bash"}}})
		followUpID := insert(domain.Message{ConversationID: conversationID, Role: "user", Content: "one more thing"})
		anchored := float64(callID) + (float64(followUpID)-float64(callID))/2
		resultID := insert(domain.Message{ConversationID: conversationID, Role: "tool", ToolCallID: "t1",
			Content: "interrupted", IsError: true, Seq: &anchored})

		window, err := store.ListForAssemblySystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("ListForAssemblySystem: %v", err)
		}
		var order []int
		for _, m := range window {
			order = append(order, m.ID)
			if m.ID == resultID && (m.Seq == nil || *m.Seq != anchored) {
				t.Errorf("result row seq = %v, want the caller's %v", m.Seq, anchored)
			}
		}
		want := []int{callID, resultID, followUpID}
		if len(order) != len(want) {
			t.Fatalf("assembly window ids = %v, want %v (the newest row assembling in the middle)", order, want)
		}
		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("assembly window ids = %v, want %v (the newest row assembling in the middle)", order, want)
			}
		}
	})

	t.Run("CompactForClaimSystem_CommitsResultFlipsSpanAndReseqsQueue", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-compact", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claimID := seed.ClaimRows(t, conversationID)[0].ID

		insert := func(msg domain.Message) int {
			id, err := store.InsertMessageForClaimSystem(ctx, orgID, claimID, &msg)
			if err != nil {
				t.Fatalf("insert fixture row: %v", err)
			}
			return int(id)
		}
		pending := false
		openingID := insert(domain.Message{ConversationID: conversationID, Role: "user", Content: "the mission"})
		assistantID := insert(domain.Message{ConversationID: conversationID, Role: "assistant", Content: "working"})
		toolID := insert(domain.Message{ConversationID: conversationID, Role: "tool", ToolCallID: "t1", Content: "big result"})
		requestID := insert(domain.Message{ConversationID: conversationID, Role: "user",
			Subtype: domain.MessageSubtypeInjectionCompactionRequest, Content: "please compact"})
		// Two rows queued before the compaction commits — the ordering
		// contract says they must land AFTER the summary despite their
		// smaller ids, in their existing relative order.
		queuedA := insert(domain.Message{ConversationID: conversationID, Role: "user", Content: "queued first", Delivered: &pending})
		queuedB := insert(domain.Message{ConversationID: conversationID, Role: "user", Content: "queued second", Delivered: &pending})

		replyRow := &domain.Message{Role: "assistant", Content: "<analysis>a</analysis>\n\n<summary>s</summary>", Model: "claude-haiku-4-5"}
		resultRow := &domain.Message{Role: "user",
			Subtype: domain.MessageSubtypeInjectionCompactionResult, Content: "the summary"}
		span := []int{assistantID, toolID, requestID}
		if err := store.CompactForClaimSystem(ctx, orgID, conversationID, claimID, replyRow, resultRow, span); err != nil {
			t.Fatalf("CompactForClaimSystem: %v", err)
		}
		if replyRow.ID == 0 || resultRow.ID == 0 {
			t.Fatalf("assigned ids not written back: reply=%d result=%d", replyRow.ID, resultRow.ID)
		}

		// Assembly: the pinned opening survives, the span and the reply are
		// gone, the result row is present, and the queued rows sort after it.
		window, err := store.ListForAssemblySystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("ListForAssemblySystem: %v", err)
		}
		var order []int
		for _, m := range window {
			order = append(order, m.ID)
		}
		want := []int{openingID, resultRow.ID, queuedA, queuedB}
		if len(order) != len(want) {
			t.Fatalf("assembly window ids = %v, want %v", order, want)
		}
		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("assembly window ids = %v, want %v", order, want)
			}
		}

		// The queued fractions live strictly between resultID and resultID+1,
		// preserving relative order — so a row inserted after the commit sorts
		// after them by its integer id alone.
		byID := map[int]domain.Message{}
		for _, m := range window {
			byID[m.ID] = m
		}
		seqA, seqB := byID[queuedA].Seq, byID[queuedB].Seq
		if seqA == nil || seqB == nil {
			t.Fatalf("queued rows not re-seqed: a=%v b=%v", seqA, seqB)
		}
		lo, hi := float64(resultRow.ID), float64(resultRow.ID)+1
		if !(*seqA > lo && *seqA < *seqB && *seqB < hi) {
			t.Fatalf("queued seqs = (%v, %v), want ascending within (%v, %v)", *seqA, *seqB, lo, hi)
		}

		// Display: compacted history stays visible (delivered + inactive), and
		// the reconstructed reply row renders like any other row.
		display, err := store.Messages(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		var sawReply, sawAssistant bool
		for _, m := range display {
			if m.ID == replyRow.ID {
				sawReply = true
				if m.WindowState != domain.MessageWindowInactive {
					t.Errorf("reply row window_state = %q, want inactive", m.WindowState)
				}
				if m.Model != "claude-haiku-4-5" {
					t.Errorf("reply row model = %q, want the cheap model attribution", m.Model)
				}
			}
			if m.ID == assistantID {
				sawAssistant = true
				if m.WindowState != domain.MessageWindowInactive {
					t.Errorf("span row window_state = %q, want inactive", m.WindowState)
				}
			}
		}
		if !sawReply || !sawAssistant {
			t.Fatalf("display read lost compacted history: reply=%v assistant=%v", sawReply, sawAssistant)
		}
	})

	t.Run("SettleCompactionRequestForClaimSystem_StampsUsageCostAndReason", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-settle", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claimID := seed.ClaimRows(t, conversationID)[0].ID
		reqID, err := store.InsertMessageForClaimSystem(ctx, orgID, claimID, &domain.Message{
			ConversationID: conversationID, Role: "user",
			Subtype: domain.MessageSubtypeInjectionCompactionRequest, Content: "please compact",
		})
		if err != nil {
			t.Fatalf("insert request row: %v", err)
		}

		// Exactly representable in binary: messages.cost_usd is float4 on
		// Postgres, and a value like 0.42 would come back off by float32
		// rounding — a dialect fact, not a store bug.
		cost := 0.25
		if _, err := store.SettleCompactionRequestForClaimSystem(ctx, orgID, conversationID, claimID,
			int(reqID), 150000, 900, 140000, 2000, &cost, "tool-calls-in-reply"); err != nil {
			t.Fatalf("SettleCompactionRequestForClaimSystem: %v", err)
		}

		msgs, err := store.Messages(ctx, orgID, conversationID)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("Messages = %v (err %v), want the one request row", msgs, err)
		}
		got := msgs[0]
		if got.InputTokens == nil || *got.InputTokens != 150000 ||
			got.OutputTokens == nil || *got.OutputTokens != 900 ||
			got.CacheReadTokens == nil || *got.CacheReadTokens != 140000 ||
			got.CacheCreationTokens == nil || *got.CacheCreationTokens != 2000 {
			t.Errorf("usage = (%v %v %v %v), want the failed attempt settled",
				got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheCreationTokens)
		}
		if got.CostUSD == nil || *got.CostUSD != cost {
			t.Errorf("cost_usd = %v, want %v", got.CostUSD, cost)
		}
		if got.Metadata["compaction_failure"] != "tool-calls-in-reply" {
			t.Errorf("metadata = %v, want compaction_failure recorded", got.Metadata)
		}
	})

	t.Run("ParkOpenForClaimSystem_ParksAndReleasesLikeItsUnfencedTwin", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-fenced-cancel", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claimID := seed.ClaimRows(t, conversationID)[0].ID

		ok, err := store.ParkOpenForClaimSystem(ctx, orgID, conversationID, claimID, db.ParkStopped(domain.ParkReasonUserCancelled, "Run cancelled by user"))
		if err != nil {
			t.Fatalf("ParkOpenForClaimSystem: %v", err)
		}
		if !ok {
			t.Fatal("ParkOpenForClaimSystem reported no flip on a running run")
		}
		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.Status != "open" || got.ParkReason != domain.ParkReasonUserCancelled {
			t.Errorf("conversation = (%q, %q), want (open, user_cancelled)", got.Status, got.ParkReason)
		}
		claims := seed.ClaimRows(t, conversationID)
		if len(claims) != 1 || !claims[0].Released || claims[0].Outcome != "cancelled" {
			t.Fatalf("claims = %+v, want the engagement released as cancelled", claims)
		}
	})

	// The three resume-coordinate writes an engagement makes about itself:
	// where its session lives, where its workspace landed, and which executor
	// is running it. Named-claim variants of writes that already existed, so
	// what every backend must agree on is that naming the claim changes the
	// resulting row not at all.
	t.Run("SetSessionAndWorktreeAndExecutorForClaimSystem_WriteExactlyLikeTheirUnfencedTwins", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-coords", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claimID := seed.ClaimRows(t, conversationID)[0].ID

		if _, err := store.SetSessionForClaimSystem(ctx, orgID, conversationID, claimID, "sess-fenced"); err != nil {
			t.Fatalf("SetSessionForClaimSystem: %v", err)
		}
		if _, err := store.SetWorktreePathForClaimSystem(ctx, orgID, conversationID, claimID, "/tmp/triagefactory-runs/fenced"); err != nil {
			t.Fatalf("SetWorktreePathForClaimSystem: %v", err)
		}
		if _, err := store.SetExecutorForClaimSystem(ctx, orgID, conversationID, claimID, "exec-coords-live", 7); err != nil {
			t.Fatalf("SetExecutorForClaimSystem: %v", err)
		}

		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.SessionID != "sess-fenced" {
			t.Errorf("sdk_session_id = %q, want sess-fenced", got.SessionID)
		}
		if got.WorktreePath != "/tmp/triagefactory-runs/fenced" {
			t.Errorf("worktree_path = %q, want /tmp/triagefactory-runs/fenced", got.WorktreePath)
		}
		claims := seed.ClaimRows(t, conversationID)
		if len(claims) != 1 {
			t.Fatalf("claims = %+v, want the one engagement (the go-live stamp re-stamps, never mints a second)", claims)
		}
		if claims[0].ExecutorID != "exec-coords-live" || claims[0].BootEpoch != 7 {
			t.Errorf("claim identity = (%q, %d), want (exec-coords-live, 7)", claims[0].ExecutorID, claims[0].BootEpoch)
		}
		if claims[0].Released {
			t.Error("the go-live stamp released the claim; it only re-stamps identity")
		}

		// Re-stamping is the ordinary case (a warm re-claim, a cold rehydrate
		// onto a fresh path), so each of these is an unconditional overwrite.
		if _, err := store.SetWorktreePathForClaimSystem(ctx, orgID, conversationID, claimID, "/tmp/triagefactory-runs/rebuilt"); err != nil {
			t.Fatalf("re-stamp worktree path: %v", err)
		}
		if got, _ = store.Get(ctx, orgID, conversationID); got == nil || got.WorktreePath != "/tmp/triagefactory-runs/rebuilt" {
			t.Errorf("worktree_path after a rehydrate = %v, want /tmp/triagefactory-runs/rebuilt", got)
		}
	})

	// The half of SetExecutorForClaimSystem's contract that is NOT the fence,
	// so both dialects owe it: it writes the claim the caller named and mints
	// nothing. Its unfenced twin does neither — it resolves the active claim
	// and creates one when there is none — so a backend that reached for the
	// twin would pass every refusal test (local has no refusals) while
	// silently writing the wrong row.
	//
	// Asserted on the outcome rather than the error, because the two dialects
	// legitimately disagree there: Postgres refuses a released claim,
	// SQLite no-ops. What must not differ is the state left behind.
	t.Run("SetExecutorForClaimSystem_WritesTheNamedClaimAndNeverMints", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		// A conversation with no claim at all: the twin would mint one here.
		if _, err := store.SetExecutorForClaimSystem(ctx, orgID, conversationID, "6f1b7d3e-0000-4000-8000-000000000000", "exec-ghost", 1); err == nil {
			t.Log("no error naming a claim that does not exist (local no-ops; Postgres refuses) — the row state below is what matters")
		}
		if claims := seed.ClaimRows(t, conversationID); len(claims) != 0 {
			t.Fatalf("claims = %+v, want none — naming an absent claim must not mint ownership", claims)
		}

		// Now with a real engagement, plus a second one on another
		// conversation to catch a write that resolves "active" instead of
		// "named".
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-named", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claimID := seed.ClaimRows(t, conversationID)[0].ID

		otherConversation := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.SetExecutorSystem(ctx, orgID, otherConversation, "exec-other", 2); err != nil {
			t.Fatalf("SetExecutorSystem (other): %v", err)
		}
		otherClaim := seed.ClaimRows(t, otherConversation)[0].ID

		// A (conversation, claim) pair that does not agree writes neither row.
		if _, err := store.SetExecutorForClaimSystem(ctx, orgID, conversationID, otherClaim, "exec-crossed", 9); err == nil {
			t.Log("no error on a mis-threaded pair (local no-ops; Postgres refuses)")
		}
		if got := seed.ClaimRows(t, conversationID); len(got) != 1 || got[0].ExecutorID != "exec-named" {
			t.Errorf("named conversation's claim = %+v, want exec-named untouched", got)
		}
		if got := seed.ClaimRows(t, otherConversation); len(got) != 1 || got[0].ExecutorID != "exec-other" || got[0].BootEpoch != 2 {
			t.Errorf("other conversation's claim = %+v, want exec-other/2 untouched", got)
		}

		// The matching pair writes, and writes only its own row.
		if _, err := store.SetExecutorForClaimSystem(ctx, orgID, conversationID, claimID, "exec-named-live", 5); err != nil {
			t.Fatalf("SetExecutorForClaimSystem: %v", err)
		}
		if got := seed.ClaimRows(t, conversationID); len(got) != 1 || got[0].ExecutorID != "exec-named-live" || got[0].BootEpoch != 5 {
			t.Errorf("claim after the go-live stamp = %+v, want exec-named-live/5", got)
		}
		if got := seed.ClaimRows(t, otherConversation); len(got) != 1 || got[0].ExecutorID != "exec-other" {
			t.Errorf("other conversation's claim = %+v, want exec-other untouched", got)
		}
	})

	t.Run("MarkFailedIfActiveForClaimSystem_FlipsAndReleasesLikeItsUnfencedTwin", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-fenced-fail", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claimID := seed.ClaimRows(t, conversationID)[0].ID

		ok, err := store.MarkFailedIfActiveForClaimSystem(ctx, orgID, conversationID, claimID, string(domain.ConversationFailureCrash))
		if err != nil {
			t.Fatalf("MarkFailedIfActiveForClaimSystem: %v", err)
		}
		if !ok {
			t.Fatal("MarkFailedIfActiveForClaimSystem reported no flip on a running run")
		}
		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.Status != "failed" || got.FailureKind != domain.ConversationFailureCrash {
			t.Errorf("conversation = (%q, %q), want (failed, %s)", got.Status, got.FailureKind, domain.ConversationFailureCrash)
		}
		claims := seed.ClaimRows(t, conversationID)
		if len(claims) != 1 || !claims[0].Released || claims[0].Outcome != "failed" {
			t.Fatalf("claims = %+v, want the engagement released as failed", claims)
		}
	})

	t.Run("RecordClaimSandboxStatsSystem_StampsByIDIncludingReleasedClaims", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-actuals", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claims := seed.ClaimRows(t, conversationID)
		if len(claims) != 1 {
			t.Fatalf("claims = %+v, want 1", claims)
		}
		claimID := claims[0].ID

		// A fresh claim has measured nothing: NULL, not zero.
		if claims[0].PeakMemMB != nil || claims[0].CPUUsec != nil {
			t.Fatalf("new claim = %+v, want both actuals NULL", claims[0])
		}

		// The live-claim stamp (the shape a hibernating conversation hits — its
		// claim is parked, not yet released).
		peak, cpu := 731, int64(12_500_000)
		if _, err := store.RecordClaimSandboxStatsSystem(ctx, orgID, claimID, &peak, &cpu); err != nil {
			t.Fatalf("RecordClaimSandboxStatsSystem live: %v", err)
		}
		claims = seed.ClaimRows(t, conversationID)
		if len(claims) != 1 || claims[0].PeakMemMB == nil || *claims[0].PeakMemMB != peak ||
			claims[0].CPUUsec == nil || *claims[0].CPUUsec != cpu {
			t.Fatalf("claims after live stamp = %+v, want peak=%d cpu=%d", claims, peak, cpu)
		}

		// The load-bearing case: teardown runs AFTER the completion
		// bookkeeping releases the claim, so the stamp must land on a
		// released row. An active-claim predicate here would silently drop
		// every engagement's actuals.
		if _, err := store.Complete(ctx, orgID, conversationID, "completed", 0, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		peak2, cpu2 := 998, int64(20_000_000)
		if _, err := store.RecordClaimSandboxStatsSystem(ctx, orgID, claimID, &peak2, &cpu2); err != nil {
			t.Fatalf("RecordClaimSandboxStatsSystem released: %v", err)
		}
		claims = seed.ClaimRows(t, conversationID)
		if len(claims) != 1 || !claims[0].Released {
			t.Fatalf("claims = %+v, want one released claim", claims)
		}
		if claims[0].PeakMemMB == nil || *claims[0].PeakMemMB != peak2 ||
			claims[0].CPUUsec == nil || *claims[0].CPUUsec != cpu2 {
			t.Errorf("released claim actuals = %+v, want peak=%d cpu=%d", claims[0], peak2, cpu2)
		}

		// Partial measurement (a kernel with no memory.peak): the CPU time
		// records and the peak goes back to NULL rather than to a zero that
		// would read as a measured 0 MB engagement.
		if _, err := store.RecordClaimSandboxStatsSystem(ctx, orgID, claimID, nil, &cpu2); err != nil {
			t.Fatalf("RecordClaimSandboxStatsSystem partial: %v", err)
		}
		claims = seed.ClaimRows(t, conversationID)
		if claims[0].PeakMemMB != nil {
			t.Errorf("peak = %d after an unmeasured peak, want NULL", *claims[0].PeakMemMB)
		}
		if claims[0].CPUUsec == nil || *claims[0].CPUUsec != cpu2 {
			t.Errorf("cpu = %v after a partial stamp, want %d", claims[0].CPUUsec, cpu2)
		}

		// An unknown claim id is a no-op, not an error — the caller is on a
		// best-effort teardown path and must never fail a finished conversation
		// over accounting.
		if _, err := store.RecordClaimSandboxStatsSystem(ctx, orgID, uuid.New().String(), &peak, &cpu); err != nil {
			t.Errorf("RecordClaimSandboxStatsSystem unknown claim: %v, want a silent no-op", err)
		}
	})

	t.Run("ClaimDerivedFields_TrackLatestClaim", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		// First engagement.
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-1", 1); err != nil {
			t.Fatalf("SetExecutorSystem 1: %v", err)
		}
		got, err := store.Get(ctx, orgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.ExecutorID != "exec-1" || got.Attempts != 1 || got.ClaimedAt == nil {
			t.Fatalf("first engagement: executor=%q attempts=%d claimedAt=%v", got.ExecutorID, got.Attempts, got.ClaimedAt)
		}
		firstClaimedAt := *got.ClaimedAt

		// Park (releases), then a second engagement — ClaimedAt advances to
		// the newest claim, Attempts counts both, ExecutorID is the live one.
		if ok, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle()); err != nil || !ok {
			t.Fatalf("ParkOpen: ok=%v err=%v", ok, err)
		}
		time.Sleep(1100 * time.Millisecond) // SQLite's second-granularity timestamps
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-2", 2); err != nil {
			t.Fatalf("SetExecutorSystem 2: %v", err)
		}
		got, err = store.Get(ctx, orgID, conversationID)
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

	// The read behind the resume notice's executor sentence: from the claim an
	// engagement holds, name the executor that ran the one before it. Ordering
	// is the whole contract — the answer must be the immediate predecessor, not
	// just some earlier claim — so it is asserted across three engagements
	// rather than two.
	t.Run("PriorClaimExecutorSystem_NamesTheImmediatePredecessor", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		mint := func(t *testing.T, executorID string, epoch int64) {
			t.Helper()
			// The empty stamp releases the active claim; without it the next
			// mint would update in place instead of starting an engagement.
			if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "", 0); err != nil {
				t.Fatalf("release before minting %s: %v", executorID, err)
			}
			if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, executorID, epoch); err != nil {
				t.Fatalf("SetExecutorSystem %s: %v", executorID, err)
			}
		}
		prior := func(t *testing.T, claimID string) string {
			t.Helper()
			got, err := store.PriorClaimExecutorSystem(ctx, orgID, conversationID, claimID)
			if err != nil {
				t.Fatalf("PriorClaimExecutorSystem: %v", err)
			}
			return got
		}

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-1", 1); err != nil {
			t.Fatalf("SetExecutorSystem exec-1: %v", err)
		}
		claims := seed.ClaimRows(t, conversationID)
		if len(claims) != 1 {
			t.Fatalf("claims = %+v, want 1", claims)
		}
		// A conversation's first engagement has no predecessor, and that is a
		// value rather than an error — the caller asks on every claim.
		if got := prior(t, claims[0].ID); got != "" {
			t.Errorf("prior executor of the first claim = %q, want empty", got)
		}

		mint(t, "exec-2", 2)
		claims = seed.ClaimRows(t, conversationID)
		if len(claims) != 2 {
			t.Fatalf("claims = %+v, want 2", claims)
		}
		if got := prior(t, claims[1].ID); got != "exec-1" {
			t.Errorf("prior executor of the second claim = %q, want exec-1", got)
		}

		mint(t, "exec-3", 3)
		claims = seed.ClaimRows(t, conversationID)
		if len(claims) != 3 {
			t.Fatalf("claims = %+v, want 3", claims)
		}
		if got := prior(t, claims[2].ID); got != "exec-2" {
			t.Errorf("prior executor of the third claim = %q, want exec-2 (the immediate predecessor)", got)
		}

		// A conversation with no claims at all answers empty, not an error.
		bare := seedConversationForTest(t, orgID, seed, "queued")
		got, err := store.PriorClaimExecutorSystem(ctx, orgID, bare, claims[2].ID)
		if err != nil || got != "" {
			t.Errorf("PriorClaimExecutorSystem on an unclaimed conversation = %q, %v; want empty and no error", got, err)
		}

		// Mint times tied — the shape a single Postgres transaction produces,
		// since now() is fixed for its whole duration. The read must still name
		// the same predecessor rather than whichever row the plan happened to
		// emit first: an ordering that is only usually total would flip the
		// agent-facing executor sentence at random.
		//
		// Read the ids before collapsing: this is destroying the very column
		// ClaimRows orders by.
		ids := []string{claims[0].ID, claims[1].ID, claims[2].ID}
		seed.CollapseClaimTimes(t, conversationID)
		for i := 0; i < 8; i++ {
			if got := prior(t, ids[2]); got != "exec-2" {
				t.Fatalf("prior executor with tied mint times = %q, want exec-2 — the sort is not total", got)
			}
		}
	})

	t.Run("ListForTask_OrderedByStartedAtDesc", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "list")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		// Two conversations with a >1s sleep — SQLite's CURRENT_TIMESTAMP
		// default has 1-second granularity, so the gap is needed
		// for ORDER BY to discriminate. Two conversations are enough to pin
		// "newest first"; three would risk landing in the same
		// second slot without making the assertion stronger.
		first := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		time.Sleep(1100 * time.Millisecond)
		second := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		convs, err := store.ListForTask(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("ListForTask: %v", err)
		}
		if len(convs) != 2 {
			t.Fatalf("len = %d, want 2", len(convs))
		}
		if convs[0].ID != second || convs[1].ID != first {
			t.Errorf("order = [%s, %s], want [%s, %s] (newest first)",
				convs[0].ID, convs[1].ID, second, first)
		}
	})

	t.Run("ListForTask_PreservesTriggerType", func(t *testing.T) {
		// The projection must round-trip trigger_type — when the column was
		// missing, every caller saw "" and the resume goroutine treated
		// event conversations as manual on resume. Cover both branches across a
		// mixed list.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "list-trigger")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		manualID := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPrompt(t),
			Status: "running", Model: "m", TriggerType: "manual",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})
		eventID := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPrompt(t),
			Status: "running", Model: "m", TriggerType: "event",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})
		convs, err := store.ListForTask(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("ListForTask: %v", err)
		}
		gotByID := make(map[string]string, len(convs))
		for _, r := range convs {
			gotByID[r.ID] = r.TriggerType
		}
		if gotByID[manualID] != "manual" {
			t.Errorf("manual conversation TriggerType = %q, want manual", gotByID[manualID])
		}
		if gotByID[eventID] != "event" {
			t.Errorf("event conversation TriggerType = %q, want event", gotByID[eventID])
		}
	})

	t.Run("List_BatchedAcrossTasks", func(t *testing.T) {
		// The batched twin of ListForTask: one query returns conversations for
		// many tasks, each attributed to its own TaskID, with unknown
		// ids contributing nothing. Backs the Board's aggregated conversation
		// fetch.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		entA := seed.Entity(t, "lft-a")
		evA := seed.Event(t, entA, domain.EventGitHubPROpened)
		taskA := seed.Task(t, entA, domain.EventGitHubPROpened, evA)
		entB := seed.Entity(t, "lft-b")
		evB := seed.Event(t, entB, domain.EventGitHubPROpened)
		taskB := seed.Task(t, entB, domain.EventGitHubPROpened, evB)

		convA1 := seedConversationForTaskTest(t, orgID, taskA, "running", seed)
		convA2 := seedConversationForTaskTest(t, orgID, taskA, "completed", seed)
		convB1 := seedConversationForTaskTest(t, orgID, taskB, "running", seed)

		// Mix in a valid-but-absent UUID and a non-UUID literal: both must be
		// tolerated (no rows, no error). The non-UUID guards the Postgres path,
		// where a uuid[] bind would otherwise 22P02 — filtered per uuid.go.
		convs, total, err := store.List(ctx, orgID,
			db.ConversationListFilter{TaskIDs: []string{taskA, taskB, uuid.New().String(), "not-a-uuid"}},
			db.ListOpts{Limit: 200})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		byTask := map[string][]string{}
		for _, r := range convs {
			byTask[r.TaskID] = append(byTask[r.TaskID], r.ID)
		}
		if len(byTask[taskA]) != 2 {
			t.Errorf("task A: got %v, want 2 conversations", byTask[taskA])
		}
		if len(byTask[taskB]) != 1 {
			t.Errorf("task B: got %v, want 1 conversation", byTask[taskB])
		}
		seen := map[string]bool{}
		for _, ids := range byTask {
			for _, id := range ids {
				seen[id] = true
			}
		}
		for _, want := range []string{convA1, convA2, convB1} {
			if !seen[want] {
				t.Errorf("conversation %s missing from batched result", want)
			}
		}
		if total != 3 {
			t.Errorf("total = %d, want 3", total)
		}
		// The pages partition the (task_id, started_at DESC, id) order, and a
		// task's conversations stay contiguous inside it.
		page1, _, err := store.List(ctx, orgID,
			db.ConversationListFilter{TaskIDs: []string{taskA, taskB}}, db.ListOpts{Limit: 2})
		if err != nil {
			t.Fatalf("List page 1: %v", err)
		}
		page2, _, err := store.List(ctx, orgID,
			db.ConversationListFilter{TaskIDs: []string{taskA, taskB}}, db.ListOpts{Limit: 2, Offset: 2})
		if err != nil {
			t.Fatalf("List page 2: %v", err)
		}
		if len(page1) != 2 || len(page2) != 1 {
			t.Fatalf("pages = %d + %d, want 2 + 1", len(page1), len(page2))
		}
		walked := map[string]bool{page1[0].ID: true, page1[1].ID: true, page2[0].ID: true}
		if len(walked) != 3 {
			t.Errorf("paged walk returned a repeat: %v", walked)
		}
	})

	t.Run("List_TaskNarrowingIsOptionalButNotEmpty", func(t *testing.T) {
		// The two zero-adjacent readings the filter has to keep apart: naming
		// no task is no task filter (the rail's resource-wide question), while
		// naming tasks that all resolve to nothing is a request about nothing.
		// Collapsing them would answer a malformed selector with the whole set.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		ent := seed.Entity(t, "lft-opt")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		task := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		seedConversationForTaskTest(t, orgID, task, "running", seed)
		seedConversationForTaskTest(t, orgID, task, "completed", seed)

		convs, total, err := store.List(ctx, orgID, db.ConversationListFilter{}, db.ListOpts{Limit: 200})
		if err != nil {
			t.Fatalf("List unnarrowed: %v", err)
		}
		if len(convs) < 2 || total < 2 {
			t.Errorf("unnarrowed List = %d rows / total %d, want every seeded conversation", len(convs), total)
		}

		none, noneTotal, err := store.List(ctx, orgID,
			db.ConversationListFilter{TaskIDs: []string{uuid.New().String()}}, db.ListOpts{Limit: 200})
		if err != nil {
			t.Fatalf("List absent task: %v", err)
		}
		if len(none) != 0 || noneTotal != 0 {
			t.Errorf("List(absent task) = %d rows / total %d, want empty", len(none), noneTotal)
		}
	})

	t.Run("List_StatusFilterReadsTheDisplayLadder", func(t *testing.T) {
		// The filter has to run over the DISPLAY status, not the stored
		// column: `queued` and `running` are derived and never stored, so a
		// predicate on the column would make the count disagree with every
		// surface — claim-phase setup invisible, and a fresh mint counting as
		// nothing at all. The phase set is walked from AllClaimPhases so a
		// phase added to the vocabulary and taught to neither dialect fails
		// here rather than silently dropping out of the live count.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		ent := seed.Entity(t, "lft-status")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		task := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		filter := func(statuses ...string) []string {
			t.Helper()
			convs, total, err := store.List(ctx, orgID,
				db.ConversationListFilter{TaskIDs: []string{task}, Statuses: statuses},
				db.ListOpts{Limit: 200})
			if err != nil {
				t.Fatalf("List(%v): %v", statuses, err)
			}
			if total != len(convs) {
				t.Errorf("List(%v): total %d disagrees with %d rows", statuses, total, len(convs))
			}
			ids := make([]string, 0, len(convs))
			for _, c := range convs {
				ids = append(ids, c.ID)
			}
			return ids
		}

		// A conversation waiting with nobody driving it displays `queued`,
		// and the stored column says nothing at all — which is the whole
		// reason this filter cannot read it.
		queued := seedConversationForTaskTest(t, orgID, task, "running", seed)
		if _, err := store.ParkOpen(ctx, orgID, queued, db.ParkIdle()); err != nil {
			t.Fatalf("ParkOpen: %v", err)
		}
		if ok, err := store.MarkQueuedForResume(ctx, orgID, queued); err != nil || !ok {
			t.Fatalf("MarkQueuedForResume: ok=%v err=%v", ok, err)
		}
		done := seedConversationForTaskTest(t, orgID, task, "completed", seed)

		if got := filter(domain.StatusQueued); len(got) != 1 || got[0] != queued {
			t.Errorf("statuses=[queued] = %v, want [%s]", got, queued)
		}
		if got := filter(domain.StatusCompleted); len(got) != 1 || got[0] != done {
			t.Errorf("statuses=[completed] = %v, want [%s]", got, done)
		}
		if got := filter(domain.StatusQueued, domain.StatusCompleted); len(got) != 2 {
			t.Errorf("statuses=[queued,completed] = %v, want both", got)
		}
		if got := filter(domain.StatusOpen); len(got) != 0 {
			t.Errorf("statuses=[open] = %v, want none", got)
		}

		// An active claim with no phase displays `running`; each phase then
		// displays as itself. Both are claim state, invisible to the column.
		if _, err := store.SetExecutorSystem(ctx, orgID, queued, "exec-status", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		if got := filter(domain.StatusRunning); len(got) != 1 || got[0] != queued {
			t.Errorf("statuses=[running] = %v, want [%s]", got, queued)
		}
		if got := filter(domain.StatusQueued); len(got) != 0 {
			t.Errorf("a claimed conversation still counts as queued: %v", got)
		}
		for _, phase := range domain.AllClaimPhases() {
			if _, err := store.SetActiveClaimPhaseSystem(ctx, orgID, queued, phase); err != nil {
				t.Fatalf("SetActiveClaimPhaseSystem(%s): %v", phase, err)
			}
			if got := filter(phase); len(got) != 1 || got[0] != queued {
				t.Errorf("statuses=[%s] = %v, want [%s]", phase, got, queued)
			}
			// The live set is what the rail counts as `running`, and every
			// phase belongs to it.
			live := append([]string{domain.StatusRunning}, domain.AllClaimPhases()...)
			if got := filter(live...); len(got) != 1 || got[0] != queued {
				t.Errorf("phase %s missing from the live status set: %v", phase, got)
			}
		}
	})

	t.Run("List_TeamNarrowingScopesToTheOwningTeam", func(t *testing.T) {
		// The Overview's scope: "whatever the org · team mark says". The
		// narrowing reads conversations.team_id, denormalized at mint, so a
		// team-scoped count needs no task hop — and it composes with the
		// status filter rather than replacing it, because the Overview always
		// sends both.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		ent := seed.Entity(t, "lft-team")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		task := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		mine := seed.Team(t, "lft-mine")
		theirs := seed.Team(t, "lft-theirs")
		seedFor := func(teamID, status string) string {
			t.Helper()
			return seed.Conversation(t, domain.Conversation{
				TaskID: task, TeamID: teamID, PromptID: conversationTestPrompt(t),
				Status: status, Model: "m", BlueprintRunID: seed.BlueprintRun(t, task),
			})
		}
		// Terminal statuses, so the display ladder reads them off the stored
		// column and the composition case turns on the team, not on staging.
		myDone := seedFor(mine, domain.StatusCompleted)
		theirDone := seedFor(theirs, domain.StatusCompleted)
		theirFailed := seedFor(theirs, domain.StatusFailed)

		list := func(filter db.ConversationListFilter) []string {
			t.Helper()
			convs, total, err := store.List(ctx, orgID, filter, db.ListOpts{Limit: 200})
			if err != nil {
				t.Fatalf("List(%+v): %v", filter, err)
			}
			if total != len(convs) {
				t.Errorf("List(%+v): total %d disagrees with %d rows", filter, total, len(convs))
			}
			// The count-only read answers the same number without a row: a
			// team-scoped count and the page it heads cannot disagree.
			empty, countOnly, err := store.List(ctx, orgID, filter, db.ListOpts{Limit: 200, CountOnly: true})
			if err != nil {
				t.Fatalf("List(%+v, count-only): %v", filter, err)
			}
			if len(empty) != 0 {
				t.Errorf("List(%+v, count-only) returned %d rows; want none", filter, len(empty))
			}
			if countOnly != total {
				t.Errorf("List(%+v): count-only total %d disagrees with the paged %d", filter, countOnly, total)
			}
			ids := make([]string, 0, len(convs))
			for _, c := range convs {
				ids = append(ids, c.ID)
				if len(filter.TeamIDs) > 0 && !slices.Contains(filter.TeamIDs, c.TeamID) {
					t.Errorf("List(%+v) returned conversation %s owned by team %s", filter, c.ID, c.TeamID)
				}
			}
			return ids
		}

		if got := list(db.ConversationListFilter{TeamIDs: []string{mine}}); len(got) != 1 || got[0] != myDone {
			t.Errorf("team_ids=[mine] = %v, want [%s]", got, myDone)
		}
		if got := list(db.ConversationListFilter{TeamIDs: []string{theirs}}); len(got) != 2 {
			t.Errorf("team_ids=[theirs] = %v, want both %s and %s", got, theirDone, theirFailed)
		}
		if got := list(db.ConversationListFilter{TeamIDs: []string{mine, theirs}}); len(got) != 3 {
			t.Errorf("team_ids=[mine,theirs] = %v, want all three", got)
		}
		// Naming no team is no team narrowing — the viewer-wide read the
		// shell rail asks for — while naming a team that owns nothing is a
		// request about nothing. Collapsing the two would answer a scoped
		// count with the whole org's work.
		if got := list(db.ConversationListFilter{}); len(got) < 3 {
			t.Errorf("unnarrowed list = %v, want every seeded conversation", got)
		}
		// A present-but-empty slice is the same request as an absent one:
		// len(), never nil-ness, is what decides whether a narrowing exists.
		if got := list(db.ConversationListFilter{TeamIDs: []string{}}); len(got) < 3 {
			t.Errorf("team_ids=[] = %v, want every seeded conversation (empty is no narrowing)", got)
		}
		if got := list(db.ConversationListFilter{TeamIDs: []string{uuid.New().String()}}); len(got) != 0 {
			t.Errorf("team_ids=[absent team] = %v, want empty", got)
		}

		// Composition: the Overview sends team AND statuses, so the two
		// narrow together. A right-status wrong-team row and a right-team
		// wrong-status row are both out.
		got := list(db.ConversationListFilter{
			TeamIDs: []string{theirs}, Statuses: []string{domain.StatusCompleted},
		})
		if len(got) != 1 || got[0] != theirDone {
			t.Errorf("team_ids=[theirs] + statuses=[completed] = %v, want [%s]", got, theirDone)
		}
		if got := list(db.ConversationListFilter{
			TeamIDs: []string{mine}, Statuses: []string{domain.StatusFailed},
		}); len(got) != 0 {
			t.Errorf("team_ids=[mine] + statuses=[failed] = %v, want empty", got)
		}
	})

	t.Run("List_QueuePositionIsTheOrgLocalPlaceInLine", func(t *testing.T) {
		// The queued run's hourglass: 1-based, ordered by (started_at, id)
		// over the DISPLAY-queued rows, and absent for every other state. The
		// number frees the row's prose to name the work instead of every
		// queued row saying "waiting for a slot", so it has to move when the
		// line moves — claiming the head repositions everyone behind it on
		// the next read, with no write to any of their rows.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		ent := seed.Entity(t, "lft-qpos")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		task := seed.Task(t, ent, domain.EventGitHubPROpened, ev)

		positions := func() map[string]*int {
			t.Helper()
			convs, _, err := store.List(ctx, orgID,
				db.ConversationListFilter{TaskIDs: []string{task}}, db.ListOpts{Limit: 200})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			out := map[string]*int{}
			for _, c := range convs {
				out[c.ID] = c.QueuePosition
			}
			return out
		}
		at := func(got map[string]*int, id string) string {
			t.Helper()
			p, ok := got[id]
			if !ok {
				t.Fatalf("conversation %s missing from the list", id)
			}
			if p == nil {
				return "none"
			}
			return strconv.Itoa(*p)
		}

		// Three waiting, plus a concluded one that is older than all of them.
		// Each started_at is staged on the backend's own clock, so the line's
		// order is this fixture's rather than the insert timing's.
		first := seedConversationForTaskTest(t, orgID, task, "", seed)
		second := seedConversationForTaskTest(t, orgID, task, "", seed)
		third := seedConversationForTaskTest(t, orgID, task, "", seed)
		done := seedConversationForTaskTest(t, orgID, task, "completed", seed)
		seed.BackdateStartedAt(t, first, 30*time.Minute)
		seed.BackdateStartedAt(t, second, 20*time.Minute)
		seed.BackdateStartedAt(t, third, 10*time.Minute)
		seed.BackdateStartedAt(t, done, 40*time.Minute)

		got := positions()
		for id, want := range map[string]string{first: "1", second: "2", third: "3"} {
			if p := at(got, id); p != want {
				t.Errorf("queue position of %s = %s, want %s", id, p, want)
			}
		}
		// A terminal row is in no line, however old it is — the oldest
		// conversation here is deliberately the completed one, so a
		// derivation that ranked by start time alone would hand it 1.
		if p := at(got, done); p != "none" {
			t.Errorf("completed conversation carries queue position %s, want none", p)
		}

		// Claiming the head takes it out of the line — it displays `running`
		// now — and everyone behind it moves up by one, off the same read.
		if _, err := store.SetExecutorSystem(ctx, orgID, first, "exec-qpos", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		got = positions()
		if p := at(got, first); p != "none" {
			t.Errorf("claimed head carries queue position %s, want none", p)
		}
		for id, want := range map[string]string{second: "1", third: "2"} {
			if p := at(got, id); p != want {
				t.Errorf("after the head was claimed, queue position of %s = %s, want %s", id, p, want)
			}
		}

		// A deliberate park leaves the line too, and re-entering it by
		// undelivered input puts the row back — the display ladder's own
		// `queued` rung, which is why the position has to read the ladder and
		// not the stored column.
		if _, err := store.ParkOpen(ctx, orgID, second, db.ParkIdle()); err != nil {
			t.Fatalf("ParkOpen: %v", err)
		}
		got = positions()
		if p := at(got, second); p != "none" {
			t.Errorf("parked conversation carries queue position %s, want none", p)
		}
		if p := at(got, third); p != "1" {
			t.Errorf("after the park, queue position of %s = %s, want 1", third, p)
		}
		undelivered := false
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: second, Role: "user", Content: "carry on", Delivered: &undelivered,
		}); err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}
		got = positions()
		if p := at(got, second); p != "1" {
			t.Errorf("input-woken conversation queue position = %s, want 1", p)
		}
		if p := at(got, third); p != "2" {
			t.Errorf("after the wake, queue position of %s = %s, want 2", third, p)
		}
	})

	t.Run("List_AttentionIsThePerConversationYourMove", func(t *testing.T) {
		// The rail's `needs`: an unanswered permission prompt, or a not-live
		// conversation still holding an unresolved artifact. Counted per
		// conversation — three prompts on one run are one row — and matching
		// domain.HasUnresolvedArtifacts on which artifacts count.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		ent := seed.Entity(t, "lft-attn")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		task := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		attention := func() []string {
			t.Helper()
			convs, total, err := store.List(ctx, orgID,
				db.ConversationListFilter{TaskIDs: []string{task}, Attention: true},
				db.ListOpts{Limit: 200})
			if err != nil {
				t.Fatalf("List(attention): %v", err)
			}
			if total != len(convs) {
				t.Errorf("List(attention): total %d disagrees with %d rows", total, len(convs))
			}
			ids := make([]string, 0, len(convs))
			for _, c := range convs {
				ids = append(ids, c.ID)
			}
			return ids
		}

		// A terminal conversation carrying a draft PR is the plain case; the
		// same conversation's open PR and its unfinalized review are not.
		drafted := seedConversationForTaskTest(t, orgID, task, "completed", seed)
		quiet := seedConversationForTaskTest(t, orgID, task, "completed", seed)
		seed.Artifact(t, quiet, domain.ArtifactKindPullRequest, domain.ArtifactStatePROpen, "")
		seed.Artifact(t, quiet, domain.ArtifactKindReview, domain.ArtifactStateReviewPending,
			`{"number":7,"review_event":""}`)
		if got := attention(); len(got) != 0 {
			t.Errorf("attention with nothing unresolved = %v, want none", got)
		}
		seed.Artifact(t, drafted, domain.ArtifactKindPullRequest, domain.ArtifactStatePRDraft, "")
		if got := attention(); len(got) != 1 || got[0] != drafted {
			t.Errorf("attention with a draft PR = %v, want [%s]", got, drafted)
		}
		// A finalized pending review counts too, and a second unresolved
		// artifact on the same conversation is still one row.
		seed.Artifact(t, drafted, domain.ArtifactKindReview, domain.ArtifactStateReviewPending,
			`{"number":7,"review_event":"REQUEST_CHANGES"}`)
		if got := attention(); len(got) != 1 || got[0] != drafted {
			t.Errorf("two unresolved artifacts on one conversation = %v, want one row", got)
		}

		// A LIVE conversation's unresolved artifact is not a human's move: the
		// agent is still working, and the run view lights it as WORKING.
		live := seedConversationForTaskTest(t, orgID, task, "running", seed)
		seed.Artifact(t, live, domain.ArtifactKindPullRequest, domain.ArtifactStatePRDraft, "")
		if _, err := store.SetExecutorSystem(ctx, orgID, live, "exec-attn", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		if got := attention(); len(got) != 1 || got[0] != drafted {
			t.Errorf("a live conversation's draft PR counted as attention: %v", got)
		}

		// A prompt owned by the live claim IS a human's move, whatever the
		// conversation's status — and two prompts are still one row. A prompt
		// from a released claim is a question nobody can answer, so it drops
		// out the moment the claim does, without anything writing its expiry.
		claims := seed.ClaimRows(t, live)
		claimID := claims[len(claims)-1].ID
		seed.PendingPermission(t, live, claimID, "call-1")
		seed.PendingPermission(t, live, claimID, "call-2")
		got := attention()
		if len(got) != 2 {
			t.Fatalf("attention with a prompted live conversation = %v, want 2 rows", got)
		}
		if _, err := store.Complete(ctx, orgID, live, "completed", 0, 0, 0, "", "", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		// Its draft PR now counts (it stopped being live), but on its own
		// merit rather than the orphaned prompts'.
		if got := attention(); len(got) != 2 {
			t.Errorf("attention after the claim released = %v, want the two artifact-bearing rows", got)
		}
	})

	t.Run("ActiveIDsForTask_FiltersTerminal", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "ha")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		// No conversations yet → empty.
		ids, _ := store.ActiveIDsForTask(ctx, orgID, taskID)
		if len(ids) != 0 {
			t.Errorf("ActiveIDs with no conversations: %v, want []", ids)
		}
		// One running + one terminal → ids=[running].
		runningConversation := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		_ = seedConversationForTaskTest(t, orgID, taskID, "completed", seed)
		ids, _ = store.ActiveIDsForTask(ctx, orgID, taskID)
		if len(ids) != 1 || ids[0] != runningConversation {
			t.Errorf("ActiveIDs = %v, want [%s]", ids, runningConversation)
		}
	})

	t.Run("HasActiveAutoConversationForTask", func(t *testing.T) {
		// Per-task gate: any non-terminal trigger_type='event' conversation on
		// the task. Manual delegations are excluded (by design — manual is
		// decoupled from the auto-queue gate); terminal conversations don't
		// count either. A sibling task on the same entity is invisible here,
		// which is the whole point of the unit being the task.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "ha-ent")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)

		// No conversations → false.
		if has, _ := store.HasActiveAutoConversationForTask(ctx, orgID, taskID); has {
			t.Error("HasActiveAutoConversationForTask with no conversations: true, want false")
		}

		// Manual conversation — must NOT trip the gate.
		_ = seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		if has, _ := store.HasActiveAutoConversationForTask(ctx, orgID, taskID); has {
			t.Error("manual conversation tripped the auto gate; gate must be event-only")
		}

		// Add an active event-trigger conversation on the same task — gate flips
		// true.
		eventConversationID := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPrompt(t),
			Status: "running", Model: "m", TriggerType: "event",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})
		if has, _ := store.HasActiveAutoConversationForTask(ctx, orgID, taskID); !has {
			t.Error("active event-trigger conversation should trip the gate")
		}

		// A second task on the SAME entity is unaffected — the gate is the
		// task's, so a busy sibling never blocks it.
		ev2 := seed.Event(t, ent, domain.EventGitHubPRCICheckFailed)
		sibling := seed.Task(t, ent, domain.EventGitHubPRCICheckFailed, ev2)
		if has, _ := store.HasActiveAutoConversationForTask(ctx, orgID, sibling); has {
			t.Error("a busy sibling task on the same entity closed this task's gate")
		}

		// Terminate the event conversation; only terminal event-trigger rows
		// remain plus the still-running manual — gate flips back to
		// false.
		if _, err := store.Complete(ctx, orgID, eventConversationID, "completed", 0, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if has, _ := store.HasActiveAutoConversationForTask(ctx, orgID, taskID); has {
			t.Error("terminal event conversation + active manual should NOT trip the gate")
		}
	})

	t.Run("ActiveAutoConversationIDForTaskSystem", func(t *testing.T) {
		// Same predicate as HasActiveAutoConversationForTask (non-terminal,
		// trigger_type='event'), but returns the conversation ID instead of a
		// bool — a busy gate is the additive-injection path, which needs the
		// conversation to fold the new event into.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "ha-id-ent")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)

		// No conversations → "".
		if id, err := store.ActiveAutoConversationIDForTaskSystem(ctx, orgID, taskID); err != nil || id != "" {
			t.Errorf("with no conversations: id=%q err=%v, want empty/nil", id, err)
		}

		// Manual conversation only — must NOT resolve.
		_ = seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		if id, err := store.ActiveAutoConversationIDForTaskSystem(ctx, orgID, taskID); err != nil || id != "" {
			t.Errorf("manual-only: id=%q err=%v, want empty (event-only)", id, err)
		}

		// Active event-trigger conversation → resolves to its conversation ID.
		eventBR := seed.BlueprintRun(t, taskID)
		eventConversationID := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPrompt(t),
			Status: "running", Model: "m", TriggerType: "event",
			BlueprintRunID: eventBR,
		})
		if id, err := store.ActiveAutoConversationIDForTaskSystem(ctx, orgID, taskID); err != nil || id != eventConversationID {
			t.Errorf("ActiveAutoConversationIDForTaskSystem = %q err=%v, want %q", id, err, eventConversationID)
		}

		// A sibling task on the same entity still resolves to nothing.
		ev2 := seed.Event(t, ent, domain.EventGitHubPRCICheckFailed)
		sibling := seed.Task(t, ent, domain.EventGitHubPRCICheckFailed, ev2)
		if id, err := store.ActiveAutoConversationIDForTaskSystem(ctx, orgID, sibling); err != nil || id != "" {
			t.Errorf("sibling task: id=%q err=%v, want empty", id, err)
		}

		// The same answer for a conversation a user RESUMED. A resume keeps
		// trigger_type='event' and puts the row back mid-flight, so it reads
		// as a live auto conversation again — for its own task, and only its
		// own. A human-paced follow-up on one card must never hold up automated
		// triage of a different card on the same entity.
		if _, err := store.Complete(ctx, orgID, eventConversationID, "completed", 0, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("conclude before resume: %v", err)
		}
		// The blueprint settles first: a resume fixture that skipped this would
		// be staging a state the CAS refuses.
		seed.SetBlueprintRunStatus(t, eventBR, "completed")
		if flipped, err := store.MarkQueuedForResume(ctx, orgID, eventConversationID); err != nil || !flipped {
			t.Fatalf("MarkQueuedForResume: ok=%v err=%v", flipped, err)
		}
		if id, err := store.ActiveAutoConversationIDForTaskSystem(ctx, orgID, taskID); err != nil || id != eventConversationID {
			t.Errorf("resumed conversation, own task: id=%q err=%v, want %q", id, err, eventConversationID)
		}
		if id, err := store.ActiveAutoConversationIDForTaskSystem(ctx, orgID, sibling); err != nil || id != "" {
			t.Errorf("resumed conversation, sibling task on the same entity: id=%q err=%v, want empty", id, err)
		}

		// Terminate it — terminal-only, plus the still-active manual
		// conversation, resolves back to "".
		if _, err := store.Complete(ctx, orgID, eventConversationID, "completed", 0, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if id, err := store.ActiveAutoConversationIDForTaskSystem(ctx, orgID, taskID); err != nil || id != "" {
			t.Errorf("terminal event conversation + active manual: id=%q err=%v, want empty", id, err)
		}
	})

	t.Run("ListParkedWorktreePathsSystem_FiltersByStatusAndWorktree", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		// open WITH a worktree path → included.
		openConversation := seedConversationForTest(t, orgID, seed, "open")
		if _, err := store.SetWorktreePath(ctx, orgID, openConversation, "/tmp/triagefactory-runs/open"); err != nil {
			t.Fatalf("set worktree (open): %v", err)
		}
		// completed WITH a worktree → excluded by the status filter. A completed
		// conversation that left an unresolved artifact no longer parks, so its
		// worktree is not preserved as a warm resume cache.
		completed := seedConversationForTest(t, orgID, seed, "completed")
		if _, err := store.SetWorktreePath(ctx, orgID, completed, "/tmp/triagefactory-runs/completed"); err != nil {
			t.Fatalf("set worktree (completed): %v", err)
		}
		// open WITHOUT a worktree → excluded by the COALESCE filter.
		_ = seedConversationForTest(t, orgID, seed, "open")
		// running WITH a worktree → excluded by the status filter.
		running := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.SetWorktreePath(ctx, orgID, running, "/tmp/triagefactory-runs/running"); err != nil {
			t.Fatalf("set worktree (running): %v", err)
		}
		// open WITH a worktree but under an already-terminal blueprint_run →
		// excluded: a parked conversation under a terminal parent is not
		// resumable, so its worktree must not be preserved (else the boot
		// reconcile orphans the row and leaves its checked-out branch on disk).
		orphanTaskID := seed.Task(t, seed.Entity(t, "parked-orphan"), domain.EventGitHubPROpened,
			seed.Event(t, seed.Entity(t, "parked-orphan-ev"), domain.EventGitHubPROpened))
		orphanBR := seed.BlueprintRun(t, orphanTaskID)
		orphan := seed.Conversation(t, domain.Conversation{
			TaskID: orphanTaskID, PromptID: conversationTestPrompt(t), Status: "open", Model: "m",
			BlueprintRunID: orphanBR,
		})
		if _, err := store.SetWorktreePath(ctx, orgID, orphan, "/tmp/triagefactory-runs/orphan"); err != nil {
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
			t.Error("completed worktree leaked — status filter failed (completed conversations no longer park)")
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

	t.Run("EntitiesWithOpenConversations_EmptyInputFastPath", func(t *testing.T) {
		store, orgID, _, _ := mk(t)
		got, err := store.EntitiesWithOpenConversations(context.Background(), orgID, nil)
		if err != nil {
			t.Fatalf("nil: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("nil input: %d entries, want 0", len(got))
		}
		got, err = store.EntitiesWithOpenConversations(context.Background(), orgID, []string{})
		if err != nil {
			t.Fatalf("empty: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("empty input: %d entries, want 0", len(got))
		}
	})

	t.Run("EntitiesWithOpenConversations_FiltersByStatus", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		entA := seed.Entity(t, "ewa-a")
		entB := seed.Entity(t, "ewa-b")
		evA := seed.Event(t, entA, domain.EventGitHubPROpened)
		evB := seed.Event(t, entB, domain.EventGitHubPROpened)
		taskA := seed.Task(t, entA, domain.EventGitHubPROpened, evA)
		taskB := seed.Task(t, entB, domain.EventGitHubPROpened, evB)

		// A has an open conversation; B has only a running one.
		convA := seedConversationForTaskTest(t, orgID, taskA, "running", seed)
		if _, err := store.ParkOpen(ctx, orgID, convA, db.ParkIdle()); err != nil {
			t.Fatalf("open A: %v", err)
		}
		_ = seedConversationForTaskTest(t, orgID, taskB, "running", seed)

		got, err := store.EntitiesWithOpenConversations(ctx, orgID, []string{entA, entB})
		if err != nil {
			t.Fatalf("EntitiesWithOpenConversations: %v", err)
		}
		if _, ok := got[entA]; !ok {
			t.Errorf("entA missing from open set")
		}
		if _, ok := got[entB]; ok {
			t.Errorf("entB leaked — only entA has an open conversation")
		}
	})

	t.Run("InsertMessage_StampsCreatedAtAndReturnsID", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		msg := &domain.Message{
			ConversationID: conversationID,
			Role:           "assistant",
			Content:        "hello",
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
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		explicit := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		msg := &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: "x",
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
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		msg := &domain.Message{
			ConversationID: conversationID,
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
		msgs, err := store.Messages(ctx, orgID, conversationID)
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
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		// Mint a claim so ClaimID has a real FK target, then attribute a
		// user message to (user, claim).
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-msg", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claims := seed.ClaimRows(t, conversationID)
		if len(claims) != 1 {
			t.Fatalf("claims = %+v, want 1", claims)
		}
		claimID := claims[0].ID

		attributed := &domain.Message{
			ConversationID: conversationID, Role: "user", Content: "steer",
			UserID: userID, ClaimID: claimID,
		}
		if _, err := store.InsertMessage(ctx, orgID, attributed); err != nil {
			t.Fatalf("InsertMessage attributed: %v", err)
		}
		// A system row written outside the engagement (claim released)
		// leaves both empty (NULL on the row).
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "", 0); err != nil {
			t.Fatalf("SetExecutorSystem release: %v", err)
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: "reply",
		}); err != nil {
			t.Fatalf("InsertMessage system: %v", err)
		}

		msgs, err := store.Messages(ctx, orgID, conversationID)
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
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		// Before any engagement: no active claim to attribute to.
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "pre"}); err != nil {
			t.Fatalf("InsertMessage pre-claim: %v", err)
		}

		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-stamp", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claims := seed.ClaimRows(t, conversationID)
		if len(claims) != 1 {
			t.Fatalf("claims = %+v, want 1", claims)
		}
		claim1 := claims[0].ID

		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "during"}); err != nil {
			t.Fatalf("InsertMessage during claim: %v", err)
		}

		// Released engagement: later rows must NOT inherit the dead claim.
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "", 0); err != nil {
			t.Fatalf("SetExecutorSystem release: %v", err)
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "post"}); err != nil {
			t.Fatalf("InsertMessage post-release: %v", err)
		}

		// A second engagement goes live; an explicit ClaimID naming the
		// released first claim (the bundle-import shape) beats the active
		// one.
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-stamp", 2); err != nil {
			t.Fatalf("SetExecutorSystem 2: %v", err)
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "explicit", ClaimID: claim1}); err != nil {
			t.Fatalf("InsertMessage explicit: %v", err)
		}

		msgs, err := store.Messages(ctx, orgID, conversationID)
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
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		msg := &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "hi"}
		if _, err := store.InsertMessage(ctx, orgID, msg); err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}
		msgs, err := store.Messages(ctx, orgID, conversationID)
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
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		delivered := false
		seq := 12.5
		msg := &domain.Message{
			ConversationID: conversationID,
			Role:           "assistant",
			Content:        "thinking then answering",
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
		msgs, err := store.Messages(ctx, orgID, conversationID)
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

	t.Run("Messages_RoundTripsDurationMs", func(t *testing.T) {
		// duration_ms has three distinct states the transcript renders
		// differently, and the store must keep them apart: a measured value, a
		// measured zero (fast enough to round to nothing — still a
		// measurement), and unmeasured (every row written before the runtime
		// stamped timing, and every non-agent role).
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		measured := 4312
		instant := 0
		for _, m := range []*domain.Message{
			{ConversationID: conversationID, Role: "assistant", Content: "thought", DurationMs: &measured},
			{ConversationID: conversationID, Role: "tool", Subtype: "tool", Content: "ok", ToolCallID: "t1", DurationMs: &instant},
			{ConversationID: conversationID, Role: "user", Content: "no timing on a user row"},
		} {
			if _, err := store.InsertMessage(ctx, orgID, m); err != nil {
				t.Fatalf("InsertMessage: %v", err)
			}
		}

		msgs, err := store.Messages(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		if len(msgs) != 3 {
			t.Fatalf("len = %d, want 3", len(msgs))
		}
		if msgs[0].DurationMs == nil || *msgs[0].DurationMs != measured {
			t.Errorf("assistant DurationMs = %v, want %d", msgs[0].DurationMs, measured)
		}
		if msgs[1].DurationMs == nil || *msgs[1].DurationMs != 0 {
			t.Errorf("tool DurationMs = %v, want a measured 0 (not nil)", msgs[1].DurationMs)
		}
		if msgs[2].DurationMs != nil {
			t.Errorf("unstamped DurationMs = %v, want nil (not measured)", *msgs[2].DurationMs)
		}
		if dto := msgs[2].ToDTO(); dto.DurationMs != nil {
			t.Errorf("unstamped row's DTO carries duration %v; the wire must omit the key entirely", *dto.DurationMs)
		}
	})

	t.Run("Messages_RoundTripsStopReason", func(t *testing.T) {
		// The MODEL's stop reason, per turn. Both runtimes write it — the SDK
		// parser off each assistant event, the native loop off the
		// completion's finish reason — so a `max_tokens` truncation is visible
		// whichever engine ran, in either dialect. Its scope is the point: a
		// conversation of N turns has N of these, which is why recording it at
		// conversation scope (where the last terminal write won) said nothing.
		//
		// The vocabulary is the provider's and it is OPEN — a model ships a new
		// reason whenever it likes — so the column is a passthrough, not a
		// closed set like park_reason. Empty stays empty: "the stream reported
		// none" and "unknown" are the same thing, and a placeholder would make
		// them look different.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		for _, m := range []*domain.Message{
			{ConversationID: conversationID, Role: "assistant", Content: "calling", StopReason: "tool_use"},
			{ConversationID: conversationID, Role: "assistant", Content: "cut off", StopReason: "max_tokens"},
			{ConversationID: conversationID, Role: "assistant", Content: "unreported"},
		} {
			if _, err := store.InsertMessage(ctx, orgID, m); err != nil {
				t.Fatalf("InsertMessage: %v", err)
			}
		}

		msgs, err := store.Messages(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		if len(msgs) != 3 {
			t.Fatalf("len = %d, want 3", len(msgs))
		}
		for i, want := range []string{"tool_use", "max_tokens", ""} {
			if msgs[i].StopReason != want {
				t.Errorf("message %d StopReason = %q, want %q", i, msgs[i].StopReason, want)
			}
		}
		if dto := msgs[2].ToDTO(); dto.StopReason != "" {
			t.Errorf("unreported row's DTO carries stop_reason %q; the wire must omit the key entirely", dto.StopReason)
		}
	})

	t.Run("Messages_SurfacesReasoningDecodeError", func(t *testing.T) {
		// reasoning/content_blocks are canonical replay context (read via
		// ListForAssembly by a future native loop) — a decode failure must
		// return an error, not silently produce an empty slice that reads
		// identically to "no reasoning on this message".
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		seed.SeedRawMessage(t, conversationID, "reasoning", `{"wrong":"shape"}`)
		if _, err := store.Messages(ctx, orgID, conversationID); err == nil {
			t.Fatal("Messages: want a decode error for wrong-shaped reasoning JSON, got nil")
		}
		if _, err := store.ListForAssemblySystem(ctx, orgID, conversationID); err == nil {
			t.Fatal("ListForAssembly: want a decode error for wrong-shaped reasoning JSON, got nil")
		}
	})

	t.Run("Messages_SurfacesContentBlocksDecodeError", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		seed.SeedRawMessage(t, conversationID, "content_blocks", `{"wrong":"shape"}`)
		if _, err := store.Messages(ctx, orgID, conversationID); err == nil {
			t.Fatal("Messages: want a decode error for wrong-shaped content_blocks JSON, got nil")
		}
		if _, err := store.ListForAssemblySystem(ctx, orgID, conversationID); err == nil {
			t.Fatal("ListForAssembly: want a decode error for wrong-shaped content_blocks JSON, got nil")
		}
	})

	t.Run("ListForAssembly_OrdersByCoalesceSeqAndIncludesEverythingButInactive", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		idA, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "a"})
		if err != nil {
			t.Fatalf("InsertMessage a: %v", err)
		}
		idB, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "b"})
		if err != nil {
			t.Fatalf("InsertMessage b: %v", err)
		}
		// c's seq lands it between a (COALESCE→id a) and b (COALESCE→id b) —
		// the compaction-result insertion shape.
		seqC := float64(idA) + (float64(idB)-float64(idA))/2
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "user", Subtype: "injection:compaction-result", Content: "c", Seq: &seqC,
		}); err != nil {
			t.Fatalf("InsertMessage c: %v", err)
		}
		delivered := false
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "user", Subtype: "injection:steer", Content: "pending", Delivered: &delivered,
		}); err != nil {
			t.Fatalf("InsertMessage pending: %v", err)
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: "elided", WindowState: domain.MessageWindowElided,
		}); err != nil {
			t.Fatalf("InsertMessage elided: %v", err)
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: "inactive", WindowState: domain.MessageWindowInactive,
		}); err != nil {
			t.Fatalf("InsertMessage inactive: %v", err)
		}

		got, err := store.ListForAssemblySystem(ctx, orgID, conversationID)
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
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		undelivered := false
		inserts := []*domain.Message{
			{ConversationID: conversationID, Role: "assistant", Content: "plain"},
			// Withdrawn-pending: staged, then withdrawn before any flush —
			// "never happened", so the display reads must hide it.
			{ConversationID: conversationID, Role: "user", Subtype: "injection:system-note", Content: "withdrawn",
				Delivered: &undelivered, WindowState: domain.MessageWindowInactive},
			// Delivered + inactive is compacted history: retired from
			// assembly but still part of the rendered transcript.
			{ConversationID: conversationID, Role: "assistant", Content: "compacted",
				WindowState: domain.MessageWindowInactive},
			// A still-pending active row stays visible (it will happen).
			{ConversationID: conversationID, Role: "user", Subtype: "injection:system-note", Content: "pending",
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

		msgs, err := store.Messages(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		assertContents("Messages", msgs)

		batched, err := store.MessagesForConversations(ctx, orgID, []string{conversationID})
		if err != nil {
			t.Fatalf("MessagesForConversations: %v", err)
		}
		assertContents("MessagesForConversations", batched)

		// The incremental read is the same display read from a watermark, so
		// an unbounded one has to answer exactly what Messages does — a client
		// repairing a transcript must not be shown a row the full read hides.
		sinceZero, err := store.MessagesSince(ctx, orgID, conversationID, 0)
		if err != nil {
			t.Fatalf("MessagesSince: %v", err)
		}
		assertContents("MessagesSince(0)", sinceZero)

		// Assembly excludes every inactive row — withdrawn AND compacted.
		asm, err := store.ListForAssemblySystem(ctx, orgID, conversationID)
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

	t.Run("Messages_OrderBySeqMatchesAssembly", func(t *testing.T) {
		// seq is the placement override: a row carrying one belongs where seq
		// puts it, not where it was inserted. The display reads and the
		// assembly read must agree about that, or the transcript renders a
		// different conversation than the one the agent had.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		idA, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "a"})
		if err != nil {
			t.Fatalf("InsertMessage a: %v", err)
		}
		idB, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "b"})
		if err != nil {
			t.Fatalf("InsertMessage b: %v", err)
		}
		// Written last, placed between a and b — the compaction-result shape.
		seqMid := float64(idA) + (float64(idB)-float64(idA))/2
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "user", Subtype: "injection:compaction-result", Content: "mid", Seq: &seqMid,
		}); err != nil {
			t.Fatalf("InsertMessage mid: %v", err)
		}
		// A later NULL-seq row still lands last: COALESCE falls back to its id,
		// which is above both seq keys.
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: "c"}); err != nil {
			t.Fatalf("InsertMessage c: %v", err)
		}

		want := []string{"a", "mid", "b", "c"}
		eqContents := func(desc string, msgs []domain.Message) {
			t.Helper()
			var got []string
			for _, m := range msgs {
				got = append(got, m.Content)
			}
			if len(got) != len(want) {
				t.Fatalf("%s = %v, want %v", desc, got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("%s[%d] = %q, want %q — full: %v", desc, i, got[i], want[i], got)
				}
			}
		}

		msgs, err := store.Messages(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		eqContents("Messages", msgs)

		sinceZero, err := store.MessagesSince(ctx, orgID, conversationID, 0)
		if err != nil {
			t.Fatalf("MessagesSince: %v", err)
		}
		eqContents("MessagesSince(0)", sinceZero)

		batched, err := store.MessagesForConversations(ctx, orgID, []string{conversationID})
		if err != nil {
			t.Fatalf("MessagesForConversations: %v", err)
		}
		eqContents("MessagesForConversations", batched)

		// Same rows, same order as what the model reads — every row here is
		// active and delivered, so the two reads' visibility filters coincide
		// and only the ordering is under test.
		asm, err := store.ListForAssemblySystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("ListForAssembly: %v", err)
		}
		eqContents("ListForAssembly", asm)
	})

	t.Run("MessagesSince_ReturnsOnlyRowsAboveTheWatermark", func(t *testing.T) {
		// Backs RunStation's transcript repair: the client holds every
		// row up to the watermark and asks for what it missed while its
		// websocket was down.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		otherID := seedConversationForTest(t, orgID, seed, "running")

		insert := func(conversationID, content string) int {
			t.Helper()
			id, err := store.InsertMessage(ctx, orgID, &domain.Message{
				ConversationID: conversationID, Role: "assistant", Content: content,
			})
			if err != nil {
				t.Fatalf("InsertMessage %q: %v", content, err)
			}
			return int(id)
		}
		first := insert(conversationID, "first")
		second := insert(conversationID, "second")
		insert(otherID, "other-conv")
		third := insert(conversationID, "third")

		contents := func(desc string, sinceID int) []string {
			t.Helper()
			msgs, err := store.MessagesSince(ctx, orgID, conversationID, sinceID)
			if err != nil {
				t.Fatalf("MessagesSince(%s): %v", desc, err)
			}
			var out []string
			for _, m := range msgs {
				out = append(out, m.Content)
			}
			return out
		}
		eq := func(desc string, got, want []string) {
			t.Helper()
			if len(got) != len(want) {
				t.Fatalf("MessagesSince(%s) = %v, want %v", desc, got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("MessagesSince(%s)[%d] = %q, want %q", desc, i, got[i], want[i])
				}
			}
		}

		// Below every id: the whole transcript, in id order — and only this
		// conversation's rows, so a shared watermark can't leak a sibling
		// conversation.
		eq("0", contents("0", 0), []string{"first", "second", "third"})
		// Strictly greater than: the row at the watermark is the last one the
		// caller already holds, so re-sending it would be a duplicate.
		eq("first", contents("first", first), []string{"second", "third"})
		eq("second", contents("second", second), []string{"third"})
		// Caught up — the common answer once no frames are being dropped.
		eq("third", contents("third", third), nil)

		// A withdrawn-pending row above the watermark stays hidden: the
		// incremental read is not a back door around the display filter.
		undelivered := false
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "user", Subtype: "injection:system-note", Content: "withdrawn",
			Delivered: &undelivered, WindowState: domain.MessageWindowInactive,
		}); err != nil {
			t.Fatalf("InsertMessage withdrawn: %v", err)
		}
		visible := insert(conversationID, "fourth")
		eq("third, after a withdrawn row", contents("third", third), []string{"fourth"})
		eq("fourth", contents("fourth", visible), nil)
	})

	t.Run("MarkDelivered_FlipsOnlyGivenIDsScopedToConversation", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		otherConversationID := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-scope", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claimID := seed.ClaimRows(t, conversationID)[0].ID

		delivered := false
		id1, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "user", Subtype: "injection:steer", Content: "1", Delivered: &delivered})
		if err != nil {
			t.Fatalf("InsertMessage 1: %v", err)
		}
		id2, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "user", Subtype: "injection:steer", Content: "2", Delivered: &delivered})
		if err != nil {
			t.Fatalf("InsertMessage 2: %v", err)
		}
		idOther, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: otherConversationID, Role: "user", Subtype: "injection:steer", Content: "other", Delivered: &delivered})
		if err != nil {
			t.Fatalf("InsertMessage other: %v", err)
		}

		// Ask to flip id1 (belongs to conversationID) and idOther (belongs to a
		// DIFFERENT conversation) via a call scoped to conversationID — idOther
		// must NOT flip.
		if err := store.MarkDeliveredForClaimSystem(ctx, orgID, conversationID, claimID, []int{int(id1), int(idOther)}, ""); err != nil {
			t.Fatalf("MarkDelivered: %v", err)
		}

		msgs, err := store.Messages(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("Messages(conversationID): %v", err)
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

		otherMsgs, err := store.Messages(ctx, orgID, otherConversationID)
		if err != nil {
			t.Fatalf("Messages(otherConversationID): %v", err)
		}
		if len(otherMsgs) != 1 || otherMsgs[0].Delivered == nil || *otherMsgs[0].Delivered {
			t.Errorf("otherConversationID message Delivered = %+v, want still false (conversation-scoped, must not leak across conversations)", otherMsgs)
		}
	})

	t.Run("MarkDelivered_StampsSubtypeWhenGivenAndPreservesItWhenEmpty", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		if _, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-stamp", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		claimID := seed.ClaimRows(t, conversationID)[0].ID

		delivered := false
		steered, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "user", Content: "mid-turn", Delivered: &delivered})
		if err != nil {
			t.Fatalf("InsertMessage steered: %v", err)
		}
		bare, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "user", Subtype: "injection:system-note", Content: "bare", Delivered: &delivered})
		if err != nil {
			t.Fatalf("InsertMessage bare: %v", err)
		}

		// A steer drain flushes and stamps in one call.
		if err := store.MarkDeliveredForClaimSystem(ctx, orgID, conversationID, claimID, []int{int(steered)}, "injection:steer"); err != nil {
			t.Fatalf("MarkDelivered(steer): %v", err)
		}
		// A bare drain flushes without touching the row's own subtype.
		if err := store.MarkDeliveredForClaimSystem(ctx, orgID, conversationID, claimID, []int{int(bare)}, ""); err != nil {
			t.Fatalf("MarkDelivered(bare): %v", err)
		}

		msgs, err := store.Messages(ctx, orgID, conversationID)
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
		if got := byID[int(bare)]; got == nil || got.Subtype != "injection:system-note" {
			t.Errorf("bare row subtype = %+v, want the row's own subtype preserved", got)
		} else if got.Delivered == nil || !*got.Delivered {
			t.Errorf("bare row Delivered = %v, want true", got.Delivered)
		}
	})

	t.Run("SetWindowState_BatchFlipsBeforeThresholdAndReturnsCount", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		var ids []int64
		for _, c := range []string{"m1", "m2", "m3", "m4"} {
			id, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: conversationID, Role: "assistant", Content: c})
			if err != nil {
				t.Fatalf("InsertMessage %s: %v", c, err)
			}
			ids = append(ids, id)
		}

		// Elide everything strictly before m3's assembly key — m1, m2 flip;
		// m3, m4 stay active.
		n, err := store.SetWindowStateSystem(ctx, orgID, conversationID, float64(ids[2]), domain.MessageWindowActive, domain.MessageWindowElided)
		if err != nil {
			t.Fatalf("SetWindowState: %v", err)
		}
		if n != 2 {
			t.Errorf("flipped count = %d, want 2", n)
		}

		msgs, err := store.Messages(ctx, orgID, conversationID)
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
		n2, err := store.SetWindowStateSystem(ctx, orgID, conversationID, float64(ids[2]), domain.MessageWindowActive, domain.MessageWindowElided)
		if err != nil {
			t.Fatalf("SetWindowState (rerun): %v", err)
		}
		if n2 != 0 {
			t.Errorf("rerun flipped count = %d, want 0", n2)
		}
	})

	t.Run("MessagesForConversations_BatchedAcrossConversations", func(t *testing.T) {
		// The batched twin of Messages: one query returns every message
		// for many conversations, grouped by ConversationID with each
		// conversation's messages still in insertion (id ASC) order.
		// Empty input is a no-op. Backs the Board's aggregated
		// include=messages read.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		if msgs, err := store.MessagesForConversations(ctx, orgID, nil); err != nil || msgs != nil {
			t.Fatalf("MessagesForConversations(nil) = (%v, %v), want (nil, nil)", msgs, err)
		}

		convA := seedConversationForTest(t, orgID, seed, "running")
		convB := seedConversationForTest(t, orgID, seed, "running")
		for _, c := range []string{"a-first", "a-second"} {
			if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
				ConversationID: convA, Role: "assistant", Content: c,
			}); err != nil {
				t.Fatalf("InsertMessage A: %v", err)
			}
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: convB, Role: "assistant", Content: "b-only",
		}); err != nil {
			t.Fatalf("InsertMessage B: %v", err)
		}

		// A non-UUID id must be tolerated (no rows, no error) — guards the
		// Postgres uuid[] bind path, same as ListForTasks above.
		msgs, err := store.MessagesForConversations(ctx, orgID, []string{convA, convB, "not-a-uuid"})
		if err != nil {
			t.Fatalf("MessagesForConversations: %v", err)
		}
		byConversation := map[string][]string{}
		for _, m := range msgs {
			byConversation[m.ConversationID] = append(byConversation[m.ConversationID], m.Content)
		}
		if got := byConversation[convA]; len(got) != 2 || got[0] != "a-first" || got[1] != "a-second" {
			t.Errorf("conversation A messages = %v, want [a-first a-second] (per-conversation order preserved)", got)
		}
		if got := byConversation[convB]; len(got) != 1 || got[0] != "b-only" {
			t.Errorf("conversation B messages = %v, want [b-only]", got)
		}
	})

	t.Run("NewestAssistantToolCallsForConversations_NewestAssistantRowWins", func(t *testing.T) {
		// The read behind current_action: per conversation, the tool calls on
		// its newest ASSISTANT message and nothing else. Two things have to be
		// true of "newest" for the derived line to describe what the agent is
		// doing now — a later assistant turn supersedes an earlier one, and a
		// user or tool row landing after it does not, because neither says
		// anything about what the agent reached for.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		if calls, err := store.NewestAssistantToolCallsForConversations(ctx, orgID, nil); err != nil || calls != nil {
			t.Fatalf("NewestAssistantToolCallsForConversations(nil) = (%v, %v), want (nil, nil)", calls, err)
		}

		working := seedConversationForTest(t, orgID, seed, "running")
		quiet := seedConversationForTest(t, orgID, seed, "running")
		bare := seedConversationForTest(t, orgID, seed, "running")

		insert := func(conversationID, role string, calls []domain.ToolCall) {
			t.Helper()
			if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
				ConversationID: conversationID, Role: role, Content: "x", ToolCalls: calls,
			}); err != nil {
				t.Fatalf("InsertMessage(%s, %s): %v", conversationID, role, err)
			}
		}

		// Superseded by the turn below it.
		insert(working, "assistant", []domain.ToolCall{{ID: "t0", Name: "read", Input: map[string]any{"path": "stale.go"}}})
		// The newest assistant turn: two calls, and their order is part of the
		// answer — the composition reads the LAST one, the call still in flight.
		insert(working, "assistant", []domain.ToolCall{
			{ID: "t1", Name: "grep", Input: map[string]any{"pattern": "needle"}},
			{ID: "t2", Name: "bash", Input: map[string]any{"command": "go test ./..."}},
		})
		// Newer rows in the roles this read ignores: the tool result for the
		// call above, and a person steering mid-run.
		insert(working, "tool", nil)
		insert(working, "user", nil)

		// No assistant message at all, and an assistant message carrying no
		// tool calls: both answer nothing rather than an empty entry.
		insert(quiet, "user", nil)
		insert(bare, "assistant", nil)

		// A non-UUID id must be tolerated (no rows, no error), guarding the
		// Postgres uuid[] bind path like the batched reads above.
		got, err := store.NewestAssistantToolCallsForConversations(ctx, orgID,
			[]string{working, quiet, bare, "not-a-uuid"})
		if err != nil {
			t.Fatalf("NewestAssistantToolCallsForConversations: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("keyed conversations = %v, want only %s", slices.Sorted(maps.Keys(got)), working)
		}
		calls := got[working]
		if len(calls) != 2 {
			t.Fatalf("newest turn's calls = %v, want the 2 from the newest assistant row", calls)
		}
		if calls[0].Name != "grep" || calls[1].Name != "bash" {
			t.Errorf("call order = [%s %s], want [grep bash] (stored order preserved)", calls[0].Name, calls[1].Name)
		}
		if cmd, _ := calls[1].Input["command"].(string); cmd != "go test ./..." {
			t.Errorf("last call input[command] = %q, want the stored command (arguments survive the round trip)", cmd)
		}
	})

	t.Run("TokenTotalsSystem_SumsAssistantOnly", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
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
				ConversationID: conversationID, Role: tup.role, Content: "x",
				InputTokens: &in, OutputTokens: &out,
				Model: "claude-test",
			}
			if _, err := store.InsertMessage(ctx, orgID, msg); err != nil {
				t.Fatalf("InsertMessage(%s): %v", tup.role, err)
			}
		}
		tot, err := store.TokenTotalsSystem(ctx, orgID, conversationID)
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
		conversationID := seedConversationForTest(t, orgID, seed, "open")

		// No agent message yet → ok=false, the caller falls back to the
		// conversation's start.
		if _, ok, err := store.LastAgentActivityAtSystem(ctx, orgID, conversationID); err != nil || ok {
			t.Fatalf("empty conversation: got ok=%v err=%v, want ok=false err=nil", ok, err)
		}

		// Insert in id order: an assistant turn, then a LATER user follow-up. The
		// watermark must be the assistant row, NOT the user message — a resume's
		// just-recorded user message must not poison the "agent last ran" mark.
		// Explicit timestamps make the assertion exact; ordering is by id, so the
		// user row (inserted last, newest id) would win a naive ORDER BY id query.
		agentAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
		for _, m := range []*domain.Message{
			{ConversationID: conversationID, Role: "assistant", Content: "agent turn", CreatedAt: agentAt},
			{ConversationID: conversationID, Role: "user", Content: "resume message", CreatedAt: agentAt.Add(time.Hour)},
		} {
			if _, err := store.InsertMessage(ctx, orgID, m); err != nil {
				t.Fatalf("InsertMessage(%s): %v", m.Role, err)
			}
		}
		at, ok, err := store.LastAgentActivityAtSystem(ctx, orgID, conversationID)
		if err != nil || !ok {
			t.Fatalf("after messages: got ok=%v err=%v, want ok=true err=nil", ok, err)
		}
		if !at.Equal(agentAt) {
			t.Errorf("watermark = %v, want the assistant row %v (user message must be excluded)", at, agentAt)
		}

		// A newer agent (tool) message advances the watermark.
		toolAt := agentAt.Add(2 * time.Hour)
		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, Role: "tool", Content: "tool result", CreatedAt: toolAt,
		}); err != nil {
			t.Fatalf("InsertMessage(tool): %v", err)
		}
		at2, ok, err := store.LastAgentActivityAtSystem(ctx, orgID, conversationID)
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

		// Two conversations sharing one blueprint_run — sibling steps. (The
		// seeder mints a fresh blueprint_run per call, so we reuse brID
		// directly to stage the multi-step shape the footer aggregates
		// over.)
		step1 := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPrompt(t),
			Status: "running", Model: "m", BlueprintRunID: brID,
		})
		step2 := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPrompt(t),
			Status: "running", Model: "m", BlueprintRunID: brID,
		})
		// Each step streams a row and settles its cost lump on it — the
		// sibling sum reads the ledger, not any conversation-level column.
		settle := func(stepID string, cost float64) {
			t.Helper()
			if _, err := store.InsertMessage(ctx, orgID, &domain.Message{ConversationID: stepID, Role: "assistant", Content: "work"}); err != nil {
				t.Fatalf("InsertMessage %s: %v", stepID, err)
			}
			if _, err := store.Complete(ctx, orgID, stepID, "completed", cost, 1000, 1, "", "finish", "", ""); err != nil {
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
		// A blueprint_run with no other conversations sums to 0, not an error.
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
		_ = seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPrompt(t),
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

		// Two conversations sharing one blueprint_run — sibling steps. Duration
		// lives on the claim Complete releases, so each step goes live (minting
		// a claim) before it completes.
		step1 := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPrompt(t),
			Status: "running", Model: "m", BlueprintRunID: brID,
		})
		step2 := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPrompt(t),
			Status: "running", Model: "m", BlueprintRunID: brID,
		})
		settle := func(stepID string, durationMs int) {
			t.Helper()
			if _, err := store.SetExecutorSystem(ctx, orgID, stepID, "exec-dur", 1); err != nil {
				t.Fatalf("SetExecutorSystem %s: %v", stepID, err)
			}
			if _, err := store.Complete(ctx, orgID, stepID, "completed", 0, durationMs, 1, "", "finish", "", ""); err != nil {
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
		// A blueprint_run with no other conversations sums to 0, not an error.
		ms, err = store.BlueprintSiblingDurationMsSystem(ctx, orgID, uuid.New().String(), step1)
		if err != nil {
			t.Fatalf("BlueprintSiblingDurationMsSystem(empty): %v", err)
		}
		if ms != 0 {
			t.Errorf("sibling duration for empty blueprint_run = %d, want 0", ms)
		}
		// An unsettled sibling (seeded, never claimed nor completed → no
		// claim telemetry) contributes 0: SUM skips it, COALESCE floors at 0.
		_ = seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPrompt(t),
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

	t.Run("MemoryMissing_DerivedFromConversationMemoryJOIN", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "mem")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)

		// One conversation per memory-content state. memory_missing should be
		// true for no-row, NULL, "", whitespace; false for populated.
		conversationNoRow := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		conversationNullContent := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		conversationEmpty := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		conversationWhitespace := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		conversationPopulated := seedConversationForTaskTest(t, orgID, taskID, "running", seed)
		seed.SetConversationMemory(t, conversationNullContent, ent, NullMemorySentinel)
		seed.SetConversationMemory(t, conversationEmpty, ent, "")
		seed.SetConversationMemory(t, conversationWhitespace, ent, "  \t\n ")
		seed.SetConversationMemory(t, conversationPopulated, ent, "real reasoning text")

		want := map[string]bool{
			conversationNoRow:       true,
			conversationNullContent: true,
			conversationEmpty:       true,
			conversationWhitespace:  true,
			conversationPopulated:   false,
		}
		for id, expected := range want {
			got, err := store.Get(ctx, orgID, id)
			if err != nil || got == nil {
				t.Fatalf("Get %s: err=%v got=%v", id, err, got)
			}
			if got.MemoryMissing != expected {
				t.Errorf("conversation %s: memory_missing=%v, want %v", id, got.MemoryMissing, expected)
			}
		}
	})
}

// reapKeysContain reports whether the retention sweep's key set names this
// blueprint_run.
// evictableFor runs the eviction enumeration and returns the entry for
// blueprintRunID, or nil when the key is not evictable. The suite asserts on
// one key at a time because the query is fleet-wide and other subtests' rows
// share the backend.
func evictableFor(t *testing.T, store db.ConversationStore, ctx context.Context, cutoff time.Time, blueprintRunID string) *domain.EvictableWorkspace {
	t.Helper()
	keys, err := store.ListEvictableWorkspacesSystem(ctx, cutoff)
	if err != nil {
		t.Fatalf("ListEvictableWorkspacesSystem: %v", err)
	}
	for i := range keys {
		if keys[i].BlueprintRunID == blueprintRunID {
			return &keys[i]
		}
	}
	return nil
}

// parkedEvictionCandidate stages the shape every eviction refusal starts from:
// one conversation, parked with a worktree path recorded, so the ONLY thing
// left for a subtest to vary is the snapshot state. Returns the blueprint run
// id — the snapshot key.
func parkedEvictionCandidate(t *testing.T, store db.ConversationStore, ctx context.Context, orgID string, seed ConversationSeeder, suffix string) string {
	t.Helper()
	conversationID, bpr := seedConversationWithBlueprintForTest(t, orgID, seed, "running")
	if _, err := store.SetWorktreePathSystem(ctx, orgID, conversationID, "/tmp/triagefactory-runs/"+suffix); err != nil {
		t.Fatalf("SetWorktreePathSystem: %v", err)
	}
	if _, err := store.ParkOpen(ctx, orgID, conversationID, db.ParkIdle()); err != nil {
		t.Fatalf("ParkOpen: %v", err)
	}
	return bpr
}

func reapKeysContain(keys []domain.SnapshotReapKey, blueprintRunID string) bool {
	for _, k := range keys {
		if k.BlueprintRunID == blueprintRunID {
			return true
		}
	}
	return false
}

// seedConversationForTest creates a fresh entity+event+task+conversation chain
// and returns the conversation ID. status is what we want the conversation row
// to land in; the seeder inserts the row with the status set directly rather
// than driving the lifecycle methods (which is the conformance
// suite's job to test).
func seedConversationForTest(t *testing.T, orgID string, seed ConversationSeeder, status string) string {
	t.Helper()
	ent := seed.Entity(t, "seed-"+status+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	ev := seed.Event(t, ent, domain.EventGitHubPROpened)
	taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
	return seedConversationForTaskTest(t, orgID, taskID, status, seed)
}

// seedConversationWithBlueprintForTest is seedConversationForTest plus the
// blueprint_run id it minted (running, like a real firing), for subtests whose
// subject is the parent's state rather than the conversation's.
func seedConversationWithBlueprintForTest(t *testing.T, orgID string, seed ConversationSeeder, status string) (conversationID, blueprintRunID string) {
	t.Helper()
	_ = orgID
	ent := seed.Entity(t, "seed-bp-"+status+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	ev := seed.Event(t, ent, domain.EventGitHubPROpened)
	taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
	brID := seed.BlueprintRun(t, taskID)
	return seed.Conversation(t, domain.Conversation{
		TaskID: taskID, PromptID: conversationTestPrompt(t), Status: status, Model: "m",
		BlueprintRunID: brID,
	}), brID
}

// seedConversationForTaskTest creates a conversation on an existing task,
// used by tests that need multiple conversations on the same parent. Each
// conversation gets its own freshly-minted blueprint_run; independent
// firings on a shared task is the realistic shape.
func seedConversationForTaskTest(t *testing.T, orgID, taskID, status string, seed ConversationSeeder) string {
	t.Helper()
	_ = orgID
	return seed.Conversation(t, domain.Conversation{
		TaskID: taskID, PromptID: conversationTestPrompt(t), Status: status, Model: "m",
		BlueprintRunID: seed.BlueprintRun(t, taskID),
	})
}

// conversationTestPromptID is the prompt-row id the backend test files
// seed once per test factory call. Conformance subtests reference
// it by this constant when creating conversations; the seeder doesn't surface
// it as a field because every call uses the same value within one
// subtest.
const conversationTestPromptID = "p_conversation_test"

func conversationTestPrompt(_ *testing.T) string { return conversationTestPromptID }
