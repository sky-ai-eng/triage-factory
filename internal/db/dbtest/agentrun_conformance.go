package dbtest

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// AgentRunStoreFactory is what a per-backend test file hands to
// RunAgentRunStoreConformance. Returns:
//   - the wired AgentRunStore impl,
//   - the orgID to pass to every call,
//   - a userID for user-attributed write paths,
//   - an AgentRunSeeder for entity/task/run_memory/task-claim
//     fixtures the harness needs but doesn't go through the store
//     to provide.
type AgentRunStoreFactory func(t *testing.T) (
	store db.AgentRunStore,
	orgID, userID string,
	seed AgentRunSeeder,
)

// AgentRunSeeder is a bag of callbacks the conformance suite uses
// to stage fixture rows that aren't agent-run-shaped. Each backend
// test file implements them against its own SQL.
type AgentRunSeeder struct {
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

	// EventHandler inserts a minimal enabled rule handler and returns
	// its id, used as a stable runs.trigger_id FK target for the
	// SKY-424 fence subtest. A rule (not a trigger) keeps the seed
	// cheap — no blueprint row to wire — and the fence only needs a
	// non-NULL handler id, since the partial unique index treats NULL
	// trigger_id as distinct (the fence would silently not engage).
	EventHandler func(t *testing.T, eventType string) string

	// BlueprintRun mints a blueprint + blueprint_run pair against the
	// given taskID and returns the blueprint_run id. Every runs row now
	// carries a NOT NULL blueprint_run_id FK (a single prompt is a
	// 1-step blueprint), so the conformance suite stages one of these
	// per run it creates and sets domain.AgentRun.BlueprintRunID. Each
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

	// SetRunMemory upserts a run_memory row with the given
	// agent_content. content="" inserts an empty string;
	// NullMemorySentinel inserts SQL NULL.
	SetRunMemory func(t *testing.T, runID, entityID, content string)

	// AgentID returns an identifier suitable for the
	// StampAgentClaim agentID and the run row's actor_agent_id.
	// Backends use this to thread their own seeded agent row (the
	// Postgres path needs a real FK; SQLite is more relaxed).
	AgentID string
}

// RunAgentRunStoreConformance covers the agent-run contract every
// backend impl must hold:
//
//   - Lifecycle methods (Create / Complete / AddPartialTotals /
//     SetSession / MarkOpen / MarkResuming /
//     MarkCancelledIfActive / MarkDiscarded) refuse terminal
//     statuses and produce correct side effects when they accept.
//   - Queries return what they advertise (status filters, sort
//     orders, JOIN-derived projections).
//   - Transcript layer round-trips messages including JSONB
//     metadata + tool_calls, and TokenTotals sums correctly.
//   - Memory_missing derivation matches the four noncompliance
//     forms (no row / NULL / "" / whitespace) + the populated
//     baseline.
//
// Cross-org leakage is Postgres-only and lives in the backend test
// file directly. The SQLite assertLocalOrg guard is also pinned in
// the backend test file.
func RunAgentRunStoreConformance(t *testing.T, mk AgentRunStoreFactory) {
	t.Helper()

	t.Run("Create_PersistsRun_GetReturnsIt", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "create")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		runID := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.AgentRun{
			ID: runID, TaskID: taskID, PromptID: agentRunTestPrompt(t), Status: "running", Model: "claude-test",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := store.Get(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got == nil || got.ID != runID {
			t.Fatalf("Get returned %v, want id=%s", got, runID)
		}
		if got.Status != "running" || got.Model != "claude-test" {
			t.Errorf("Get fields drift: %+v", got)
		}
	})

	t.Run("Create_EventTriggered_PersistsWithNullCreator", func(t *testing.T) {
		// SKY-285 review: event-triggered Create must succeed and
		// land creator_user_id=NULL (the runs_creator_matches_trigger_type
		// CHECK demands NULL for trigger_type='event'). On Postgres
		// this routes through the admin pool because the runs_insert
		// RLS policy is incompatible with NULL creator; on SQLite the
		// pool distinction doesn't exist but the contract is the same.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "create-event")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		runID := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.AgentRun{
			ID:             runID,
			TaskID:         taskID,
			PromptID:       agentRunTestPrompt(t),
			Status:         "running",
			Model:          "claude-test",
			TriggerType:    "event",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
			// CreatorUserID intentionally empty — the CHECK
			// forces NULL for event-triggered runs and the
			// store impl must accept that.
		}); err != nil {
			t.Fatalf("Create event-triggered: %v", err)
		}
		got, err := store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.Status != "running" {
			t.Errorf("status = %q, want running", got.Status)
		}
		// trigger_type is part of the standard Get projection. Pinning
		// the round-trip here so a future projection edit can't
		// silently drop the column — when the column was missing,
		// every caller saw "" and the resume goroutine treated event
		// runs as manual on resume.
		if got.TriggerType != "event" {
			t.Errorf("TriggerType = %q, want event", got.TriggerType)
		}
	})

	t.Run("CreateIfNotFiredSystem_FencesReplayPerEventTrigger", func(t *testing.T) {
		// SKY-424: the (triggering_event_id, trigger_id) fence makes
		// event-triggered auto-delegation exactly-once under the
		// at-least-once router queue. A replayed event whose first run
		// already committed must not insert a second run; two DISTINCT
		// event instances firing the same trigger still insert
		// independently (the fence is per event instance, not per trigger).
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "fence")
		ev1 := seed.Event(t, ent, domain.EventGitHubPROpened)
		ev2 := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev1)
		trigID := seed.EventHandler(t, domain.EventGitHubPROpened)

		mkRun := func(eventID string) domain.AgentRun {
			return domain.AgentRun{
				ID:                uuid.New().String(),
				TaskID:            taskID,
				PromptID:          agentRunTestPrompt(t),
				Status:            "initializing",
				Model:             "claude-test",
				TriggerType:       "event",
				TriggerID:         trigID,
				TriggeringEventID: eventID,
				BlueprintRunID:    seed.BlueprintRun(t, taskID),
			}
		}

		// First fire for (ev1, trig): inserts.
		run1 := mkRun(ev1)
		inserted, err := store.CreateIfNotFiredSystem(ctx, orgID, run1)
		if err != nil {
			t.Fatalf("first fire: %v", err)
		}
		if !inserted {
			t.Fatal("first fire should insert (no prior run for this event+trigger)")
		}

		// Replay of the SAME event instance: fence trips, no row inserted.
		replayed, err := store.CreateIfNotFiredSystem(ctx, orgID, mkRun(ev1))
		if err != nil {
			t.Fatalf("replay fire: %v", err)
		}
		if replayed {
			t.Error("replay of the same (event, trigger) must NOT insert a second run")
		}

		// The first run still exists and is unchanged.
		got, err := store.Get(ctx, orgID, run1.ID)
		if err != nil || got == nil {
			t.Fatalf("Get run1: err=%v got=%v", err, got)
		}

		// A distinct event instance firing the same trigger inserts.
		inserted2, err := store.CreateIfNotFiredSystem(ctx, orgID, mkRun(ev2))
		if err != nil {
			t.Fatalf("distinct-event fire: %v", err)
		}
		if !inserted2 {
			t.Error("a distinct event instance must fire independently of the fence")
		}

		// Exactly two event-fired runs on the task: ev1 once, ev2 once.
		runs, err := store.ListForTask(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("ListForTask: %v", err)
		}
		if len(runs) != 2 {
			t.Errorf("want exactly 2 runs (replay fenced, distinct event fired), got %d", len(runs))
		}
	})

	t.Run("Create_ManualRuns_NotFencedByNullTriggeringEvent", func(t *testing.T) {
		// SKY-424: manual runs carry no triggering_event_id, so the
		// partial fence index never applies — multiple manual runs of one
		// task stay allowed. Pins that the fence didn't accidentally
		// constrain the manual path.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "manual-fence")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)

		for i := 0; i < 2; i++ {
			if err := store.Create(ctx, orgID, domain.AgentRun{
				ID:             uuid.New().String(),
				TaskID:         taskID,
				PromptID:       agentRunTestPrompt(t),
				Status:         "running",
				Model:          "claude-test",
				BlueprintRunID: seed.BlueprintRun(t, taskID),
				// TriggerType defaults to manual; TriggeringEventID empty → NULL.
			}); err != nil {
				t.Fatalf("manual run %d: %v", i, err)
			}
		}
		runs, err := store.ListForTask(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("ListForTask: %v", err)
		}
		if len(runs) != 2 {
			t.Errorf("manual runs must not be fenced: want 2, got %d", len(runs))
		}
	})

	t.Run("CreateIfNotFiredSystem_RejectsMissingEventOrTrigger", func(t *testing.T) {
		// The fenced insert must refuse an empty TriggeringEventID or
		// TriggerID rather than bind NULL and silently skip the fence —
		// otherwise the method meant to enforce exactly-once would hand
		// back an unfenced row (SKY-424).
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "fence-guard")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		trigID := seed.EventHandler(t, domain.EventGitHubPROpened)

		base := domain.AgentRun{
			TaskID:      taskID,
			PromptID:    agentRunTestPrompt(t),
			Status:      "initializing",
			Model:       "claude-test",
			TriggerType: "event",
		}

		// Missing triggering event.
		missingEvent := base
		missingEvent.ID = uuid.New().String()
		missingEvent.TriggerID = trigID
		if inserted, err := store.CreateIfNotFiredSystem(ctx, orgID, missingEvent); inserted || !errors.Is(err, db.ErrFenceRequiresEventAndTrigger) {
			t.Errorf("missing TriggeringEventID: got inserted=%v err=%v, want inserted=false err=ErrFenceRequiresEventAndTrigger", inserted, err)
		}

		// Missing trigger.
		missingTrigger := base
		missingTrigger.ID = uuid.New().String()
		missingTrigger.TriggeringEventID = ev
		if inserted, err := store.CreateIfNotFiredSystem(ctx, orgID, missingTrigger); inserted || !errors.Is(err, db.ErrFenceRequiresEventAndTrigger) {
			t.Errorf("missing TriggerID: got inserted=%v err=%v, want inserted=false err=ErrFenceRequiresEventAndTrigger", inserted, err)
		}

		// Neither attempt wrote a row.
		runs, err := store.ListForTask(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("ListForTask: %v", err)
		}
		if len(runs) != 0 {
			t.Errorf("guard must reject before insert: want 0 runs, got %d", len(runs))
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

	t.Run("Complete_FoldsTotalsAndStampsCompletedAt", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedAgentRunForTest(t, store, orgID, seed, "running")

		// Pre-seed a partial total so Complete's COALESCE+add semantics
		// are exercised end-to-end (a fresh run starts with NULL
		// totals; we want to verify a resume-style accumulation).
		if err := store.AddPartialTotals(ctx, orgID, runID, 1.25, 4000, 3); err != nil {
			t.Fatalf("AddPartialTotals: %v", err)
		}
		if err := store.Complete(ctx, orgID, runID, "completed", 0.75, 2000, 5, "ok", "all done", "abort", "needs human", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		got, err := store.Get(ctx, orgID, runID)
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
			t.Errorf("total_cost_usd = %v, want 2.0 (partial 1.25 + terminal 0.75)", got.TotalCostUSD)
		}
		if got.DurationMs == nil || *got.DurationMs != 6000 {
			t.Errorf("duration_ms = %v, want 6000", got.DurationMs)
		}
		if got.NumTurns == nil || *got.NumTurns != 8 {
			t.Errorf("num_turns = %v, want 8", got.NumTurns)
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
	})

	t.Run("Complete_DenormalizesRunMessageTokens_AbsoluteAcrossResumes", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedAgentRunForTest(t, store, orgID, seed, "running")

		// run_messages is the source of truth (written by the streaming
		// sink as messages arrive). Complete sums the four token columns
		// onto the run. Two assistant rows so the SUM is non-trivial.
		ptr := func(n int) *int { return &n }
		for _, m := range []*domain.AgentMessage{
			{RunID: runID, Role: "assistant", Subtype: "text", Content: "a",
				InputTokens: ptr(100), OutputTokens: ptr(20), CacheReadTokens: ptr(1000), CacheCreationTokens: ptr(7)},
			{RunID: runID, Role: "assistant", Subtype: "text", Content: "b",
				InputTokens: ptr(50), OutputTokens: ptr(5), CacheReadTokens: ptr(500), CacheCreationTokens: ptr(3)},
		} {
			if _, err := store.InsertMessage(ctx, orgID, m); err != nil {
				t.Fatalf("InsertMessage: %v", err)
			}
		}

		if err := store.Complete(ctx, orgID, runID, "completed", 0.1, 1000, 2, "ok", "done", "finish", "", ""); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		got, err := store.Get(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.InputTokens != 150 || got.OutputTokens != 25 || got.CacheReadTokens != 1500 || got.CacheCreationTokens != 10 {
			t.Errorf("token cols = (%d,%d,%d,%d), want (150,25,1500,10) — SUM over run_messages",
				got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheCreationTokens)
		}

		// Re-Complete simulates a resume re-running completion. Tokens are
		// SET to the absolute run_messages SUM (not added like cost), so a
		// second Complete with no new messages re-sets the same value — they
		// must stay (150,25,1500,10), NOT double.
		if err := store.Complete(ctx, orgID, runID, "completed", 0, 0, 0, "ok", "done", "finish", "", ""); err != nil {
			t.Fatalf("re-Complete: %v", err)
		}
		got2, err := store.Get(ctx, orgID, runID)
		if err != nil || got2 == nil {
			t.Fatalf("Get after re-Complete: err=%v got=%v", err, got2)
		}
		if got2.InputTokens != 150 || got2.OutputTokens != 25 || got2.CacheReadTokens != 1500 || got2.CacheCreationTokens != 10 {
			t.Errorf("after re-Complete token cols = (%d,%d,%d,%d), want unchanged (150,25,1500,10) — SET must be absolute, not additive",
				got2.InputTokens, got2.OutputTokens, got2.CacheReadTokens, got2.CacheCreationTokens)
		}
	})

	// Every terminal write — not just Complete — refreshes the token cache from
	// the run_messages SUM, so a run that ends by cancel or infra-failure still
	// reflects the tokens it streamed rather than stranding the columns at 0
	// (TFAC-473).
	t.Run("CancelAndFail_DenormalizeRunMessageTokens", func(t *testing.T) {
		ptr := func(n int) *int { return &n }
		seedRunningWithTokens := func(t *testing.T, store db.AgentRunStore, orgID string, seed AgentRunSeeder) string {
			t.Helper()
			ctx := context.Background()
			runID := seedAgentRunForTest(t, store, orgID, seed, "running")
			for _, m := range []*domain.AgentMessage{
				{RunID: runID, Role: "assistant", Subtype: "text", Content: "a",
					InputTokens: ptr(100), OutputTokens: ptr(20), CacheReadTokens: ptr(1000), CacheCreationTokens: ptr(7)},
				{RunID: runID, Role: "assistant", Subtype: "text", Content: "b",
					InputTokens: ptr(50), OutputTokens: ptr(5), CacheReadTokens: ptr(500), CacheCreationTokens: ptr(3)},
			} {
				if _, err := store.InsertMessage(ctx, orgID, m); err != nil {
					t.Fatalf("InsertMessage: %v", err)
				}
			}
			return runID
		}
		assertRolledUp := func(t *testing.T, store db.AgentRunStore, orgID, runID string) {
			t.Helper()
			got, err := store.Get(context.Background(), orgID, runID)
			if err != nil || got == nil {
				t.Fatalf("Get: err=%v got=%v", err, got)
			}
			if got.InputTokens != 150 || got.OutputTokens != 25 || got.CacheReadTokens != 1500 || got.CacheCreationTokens != 10 {
				t.Errorf("token cols = (%d,%d,%d,%d), want (150,25,1500,10) — terminal write did not roll up the run_messages SUM",
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
		runID := seedAgentRunForTest(t, store, orgID, seed, "running")
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
		plainID := seedAgentRunForTest(t, store, orgID, seed, "running")
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

		runID := seedAgentRunForTest(t, store, orgID, seed, "running")
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
		plainID := seedAgentRunForTest(t, store, orgID, seed, "running")
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
		runID := seedAgentRunForTest(t, store, orgID, seed, "running")
		if err := store.SetSession(ctx, orgID, runID, "sess-abc"); err != nil {
			t.Fatalf("SetSession: %v", err)
		}
		got, _ := store.Get(ctx, orgID, runID)
		if got == nil || got.SessionID != "sess-abc" {
			t.Errorf("session_id = %q, want sess-abc", got.SessionID)
		}
	})

	t.Run("MarkOpen_FlipsRunning_RefusesTerminal", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		// Running → ok=true.
		runID := seedAgentRunForTest(t, store, orgID, seed, "running")
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
		runID2 := seedAgentRunForTest(t, store, orgID, seed, "completed")
		ok, err = store.MarkOpen(ctx, orgID, runID2)
		if err != nil || ok {
			t.Errorf("on completed: ok=%v err=%v, want false/nil", ok, err)
		}
	})

	t.Run("MarkResuming_FromResumableStates", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		// running → refused (not a parked/terminal resumable state).
		runID := seedAgentRunForTest(t, store, orgID, seed, "running")
		if ok, err := store.MarkResuming(ctx, orgID, runID); err != nil || ok {
			t.Errorf("Resuming on running: ok=%v err=%v, want false", ok, err)
		}
		// open → ok; the second call (now running) is refused.
		if _, err := store.MarkOpen(ctx, orgID, runID); err != nil {
			t.Fatalf("open: %v", err)
		}
		if ok, err := store.MarkResuming(ctx, orgID, runID); err != nil || !ok {
			t.Fatalf("Resuming from open: ok=%v err=%v", ok, err)
		}
		if ok, _ := store.MarkResuming(ctx, orgID, runID); ok {
			t.Errorf("double-resume from running succeeded; want refused")
		}

		// completed + outcome=abort → ok (an aborted run is message-resumable).
		abortRun := seedAgentRunForTest(t, store, orgID, seed, "running")
		if err := store.Complete(ctx, orgID, abortRun, "completed", 0, 0, 0, "", "stopped", "abort", "needs a human", ""); err != nil {
			t.Fatalf("complete+abort: %v", err)
		}
		if ok, err := store.MarkResuming(ctx, orgID, abortRun); err != nil || !ok {
			t.Errorf("Resuming from completed+abort: ok=%v err=%v, want true", ok, err)
		}

		// completed + outcome=finish → refused (finish runs are deliberately
		// excluded from the resumable set).
		finishRun := seedAgentRunForTest(t, store, orgID, seed, "running")
		if err := store.Complete(ctx, orgID, finishRun, "completed", 0, 0, 0, "", "shipped", "finish", "", ""); err != nil {
			t.Fatalf("complete+finish: %v", err)
		}
		if ok, _ := store.MarkResuming(ctx, orgID, finishRun); ok {
			t.Errorf("Resuming from completed+finish succeeded; want refused (finish excluded)")
		}
	})

	t.Run("MarkFailedIfActive_FailsOpen_RefusesTerminalAndPendingApproval", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		// `open` is intentionally failable (a warm open run has no durable
		// snapshot, so an infra error reaching failRun must terminate it).
		openRun := seedAgentRunForTest(t, store, orgID, seed, "running")
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
		paRun := seedAgentRunForTest(t, store, orgID, seed, "pending_approval")
		if ok, _ := store.MarkFailedIfActive(ctx, orgID, paRun, ""); ok {
			t.Errorf("MarkFailedIfActive flipped a pending_approval run; want refused")
		}
		// Already terminal → refused.
		doneRun := seedAgentRunForTest(t, store, orgID, seed, "completed")
		if ok, _ := store.MarkFailedIfActive(ctx, orgID, doneRun, ""); ok {
			t.Errorf("MarkFailedIfActive flipped a completed run; want refused")
		}
	})

	t.Run("MarkCancelledIfActive_FlipsActive_RefusesTerminal", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedAgentRunForTest(t, store, orgID, seed, "running")
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

	t.Run("MarkDiscarded_OnlyPendingApproval", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runRunning := seedAgentRunForTest(t, store, orgID, seed, "running")
		runPending := seedAgentRunForTest(t, store, orgID, seed, "pending_approval")

		// Running → refused (Discard targets pending_approval only).
		ok, _ := store.MarkDiscarded(ctx, orgID, runRunning, "discarded")
		if ok {
			t.Errorf("Discarded on running: ok=true, want false")
		}
		// pending_approval → ok.
		ok, err := store.MarkDiscarded(ctx, orgID, runPending, "discarded")
		if err != nil || !ok {
			t.Fatalf("Discarded on pending_approval: ok=%v err=%v", ok, err)
		}
		got, _ := store.Get(ctx, orgID, runPending)
		if got.Status != "cancelled" {
			t.Errorf("after discard: status=%q, want cancelled", got.Status)
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
		first := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.AgentRun{
			ID: first, TaskID: taskID, PromptID: agentRunTestPrompt(t), Status: "running", Model: "m",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		}); err != nil {
			t.Fatalf("Create first: %v", err)
		}
		time.Sleep(1100 * time.Millisecond)
		second := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.AgentRun{
			ID: second, TaskID: taskID, PromptID: agentRunTestPrompt(t), Status: "running", Model: "m",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		}); err != nil {
			t.Fatalf("Create second: %v", err)
		}
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
		// Same projection bug that motivated the Get assertion above
		// applies to ListForTask (shares pgRunColumns / sqliteRunColumns).
		// Cover both branches: manual round-trip + event round-trip
		// across a mixed list.
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "list-trigger")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		manualID := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.AgentRun{
			ID: manualID, TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", TriggerType: "manual",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		}); err != nil {
			t.Fatalf("Create manual: %v", err)
		}
		eventID := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.AgentRun{
			ID: eventID, TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", TriggerType: "event",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		}); err != nil {
			t.Fatalf("Create event: %v", err)
		}
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

		runA1 := seedAgentRunForTaskTest(t, store, orgID, taskA, "running", seed)
		runA2 := seedAgentRunForTaskTest(t, store, orgID, taskA, "completed", seed)
		runB1 := seedAgentRunForTaskTest(t, store, orgID, taskB, "running", seed)

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

	t.Run("HasActiveAndActiveIDs_ForTask", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "ha")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		// No runs yet → false / empty.
		if has, _ := store.HasActiveForTask(ctx, orgID, taskID); has {
			t.Error("HasActive with no runs: true, want false")
		}
		ids, _ := store.ActiveIDsForTask(ctx, orgID, taskID)
		if len(ids) != 0 {
			t.Errorf("ActiveIDs with no runs: %v, want []", ids)
		}
		// One running + one terminal → has=true, ids=[running].
		runRun := seedAgentRunForTaskTest(t, store, orgID, taskID, "running", seed)
		_ = seedAgentRunForTaskTest(t, store, orgID, taskID, "completed", seed)
		if has, _ := store.HasActiveForTask(ctx, orgID, taskID); !has {
			t.Error("HasActive with running: false, want true")
		}
		ids, _ = store.ActiveIDsForTask(ctx, orgID, taskID)
		if len(ids) != 1 || ids[0] != runRun {
			t.Errorf("ActiveIDs = %v, want [%s]", ids, runRun)
		}
	})

	t.Run("HasActiveAutoRunForEntity", func(t *testing.T) {
		// Per-entity sibling of HasActiveForTask: any non-terminal
		// trigger_type='event' run on any task that belongs to the
		// entity. Manual delegations are excluded (SKY-189 design —
		// manual decoupled from the auto-queue gate); terminal runs
		// don't count either.
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
		_ = seedAgentRunForTaskTest(t, store, orgID, taskID, "running", seed)
		if has, _ := store.HasActiveAutoRunForEntity(ctx, orgID, ent); has {
			t.Error("manual run tripped the auto-run gate; gate must be event-only")
		}

		// Add an active event-trigger run on the same task — gate flips true.
		eventRunID := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.AgentRun{
			ID: eventRunID, TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", TriggerType: "event",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		}); err != nil {
			t.Fatalf("Create event-triggered: %v", err)
		}
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

	t.Run("PendingApprovalIDForTask", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		ent := seed.Entity(t, "pa")
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		// Empty → "".
		id, _ := store.PendingApprovalIDForTask(ctx, orgID, taskID)
		if id != "" {
			t.Errorf("with no pending: id=%q, want empty", id)
		}
		// Seed one pending_approval run.
		runID := seedAgentRunForTaskTest(t, store, orgID, taskID, "pending_approval", seed)
		id, _ = store.PendingApprovalIDForTask(ctx, orgID, taskID)
		if id != runID {
			t.Errorf("PendingApprovalID = %q, want %q", id, runID)
		}
	})

	t.Run("ListParkedWorktreePaths_FiltersByStatusAndWorktree", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()

		// open WITH a worktree path → included.
		openRun := seedAgentRunForTest(t, store, orgID, seed, "open")
		if err := store.SetWorktreePath(ctx, orgID, openRun, "/tmp/triagefactory-runs/open"); err != nil {
			t.Fatalf("set worktree (open): %v", err)
		}
		// completed WITH a worktree → excluded by the status filter. A completed run
		// that left an unresolved artifact no longer parks, so its
		// worktree is not preserved as a warm resume cache.
		completed := seedAgentRunForTest(t, store, orgID, seed, "completed")
		if err := store.SetWorktreePath(ctx, orgID, completed, "/tmp/triagefactory-runs/completed"); err != nil {
			t.Fatalf("set worktree (completed): %v", err)
		}
		// open WITHOUT a worktree → excluded by the COALESCE filter.
		_ = seedAgentRunForTest(t, store, orgID, seed, "open")
		// running WITH a worktree → excluded by the status filter.
		running := seedAgentRunForTest(t, store, orgID, seed, "running")
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
		orphan := seedAgentRunUnderBlueprintForTest(t, store, orgID, orphanTaskID, orphanBR, "open")
		if err := store.SetWorktreePath(ctx, orgID, orphan, "/tmp/triagefactory-runs/orphan"); err != nil {
			t.Fatalf("set worktree (orphan): %v", err)
		}
		seed.SetBlueprintRunStatus(t, orphanBR, "cancelled")

		paths, err := store.ListParkedWorktreePaths(ctx, orgID)
		if err != nil {
			t.Fatalf("ListParkedWorktreePaths: %v", err)
		}
		got := map[string]bool{}
		for _, p := range paths {
			got[p] = true
		}
		if !got["/tmp/triagefactory-runs/open"] {
			t.Error("open worktree missing from ListParkedWorktreePaths")
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

		// System variant (admin pool; the startup sweep has no JWT-claims
		// context) returns the same set as the app-pool variant.
		sysPaths, err := store.ListParkedWorktreePathsSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListParkedWorktreePathsSystem: %v", err)
		}
		if len(sysPaths) != len(paths) {
			t.Errorf("System returned %d paths, app returned %d", len(sysPaths), len(paths))
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
		runA := seedAgentRunForTaskTest(t, store, orgID, taskA, "running", seed)
		if _, err := store.MarkOpen(ctx, orgID, runA); err != nil {
			t.Fatalf("open A: %v", err)
		}
		_ = seedAgentRunForTaskTest(t, store, orgID, taskB, "running", seed)

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
		runID := seedAgentRunForTest(t, store, orgID, seed, "running")

		msg := &domain.AgentMessage{
			RunID:   runID,
			Role:    "assistant",
			Content: "hello",
			Subtype: "text",
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
		runID := seedAgentRunForTest(t, store, orgID, seed, "running")
		explicit := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		msg := &domain.AgentMessage{
			RunID: runID, Role: "assistant", Content: "x", Subtype: "text",
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
		runID := seedAgentRunForTest(t, store, orgID, seed, "running")
		msg := &domain.AgentMessage{
			RunID:   runID,
			Role:    "assistant",
			Content: "calling tool",
			Subtype: "tool_use",
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

		runA := seedAgentRunForTest(t, store, orgID, seed, "running")
		runB := seedAgentRunForTest(t, store, orgID, seed, "running")
		for _, c := range []string{"a-first", "a-second"} {
			if _, err := store.InsertMessage(ctx, orgID, &domain.AgentMessage{
				RunID: runA, Role: "assistant", Content: c, Subtype: "text",
			}); err != nil {
				t.Fatalf("InsertMessage A: %v", err)
			}
		}
		if _, err := store.InsertMessage(ctx, orgID, &domain.AgentMessage{
			RunID: runB, Role: "assistant", Content: "b-only", Subtype: "text",
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
			byRun[m.RunID] = append(byRun[m.RunID], m.Content)
		}
		if got := byRun[runA]; len(got) != 2 || got[0] != "a-first" || got[1] != "a-second" {
			t.Errorf("run A messages = %v, want [a-first a-second] (per-run order preserved)", got)
		}
		if got := byRun[runB]; len(got) != 1 || got[0] != "b-only" {
			t.Errorf("run B messages = %v, want [b-only]", got)
		}
	})

	t.Run("TokenTotals_SumsAssistantOnly", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedAgentRunForTest(t, store, orgID, seed, "running")
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
			msg := &domain.AgentMessage{
				RunID: runID, Role: tup.role, Content: "x", Subtype: "text",
				InputTokens: &in, OutputTokens: &out,
				Model: "claude-test",
			}
			if _, err := store.InsertMessage(ctx, orgID, msg); err != nil {
				t.Fatalf("InsertMessage(%s): %v", tup.role, err)
			}
		}
		tot, err := store.TokenTotals(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("TokenTotals: %v", err)
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
		runID := seedAgentRunForTest(t, store, orgID, seed, "open")

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
		for _, m := range []*domain.AgentMessage{
			{RunID: runID, Role: "assistant", Subtype: "text", Content: "agent turn", CreatedAt: agentAt},
			{RunID: runID, Role: "user", Subtype: "text", Content: "resume message", CreatedAt: agentAt.Add(time.Hour)},
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
		if _, err := store.InsertMessage(ctx, orgID, &domain.AgentMessage{
			RunID: runID, Role: "tool", Subtype: "tool", Content: "tool result", CreatedAt: toolAt,
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
		step1, step2 := uuid.New().String(), uuid.New().String()
		for _, id := range []string{step1, step2} {
			if err := store.Create(ctx, orgID, domain.AgentRun{
				ID: id, TaskID: taskID, PromptID: agentRunTestPrompt(t),
				Status: "running", Model: "m", BlueprintRunID: brID,
			}); err != nil {
				t.Fatalf("Create %s: %v", id, err)
			}
		}
		if err := store.Complete(ctx, orgID, step1, "completed", 0.01, 1000, 1, "ok", "", "finish", "", ""); err != nil {
			t.Fatalf("Complete step1: %v", err)
		}
		if err := store.Complete(ctx, orgID, step2, "completed", 0.02, 1000, 1, "ok", "", "finish", "", ""); err != nil {
			t.Fatalf("Complete step2: %v", err)
		}

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

		// An unsettled sibling (created, never completed → NULL
		// total_cost_usd) contributes 0, not an error: SUM skips the NULL
		// and COALESCE floors the all-NULL case at 0. Add a third,
		// never-completed step and re-query for step2 — the settled total
		// is unchanged (step1's 0.01 only).
		step3 := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.AgentRun{
			ID: step3, TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", BlueprintRunID: brID,
		}); err != nil {
			t.Fatalf("Create step3: %v", err)
		}
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

		// Two runs sharing one blueprint_run — sibling steps.
		step1, step2 := uuid.New().String(), uuid.New().String()
		for _, id := range []string{step1, step2} {
			if err := store.Create(ctx, orgID, domain.AgentRun{
				ID: id, TaskID: taskID, PromptID: agentRunTestPrompt(t),
				Status: "running", Model: "m", BlueprintRunID: brID,
			}); err != nil {
				t.Fatalf("Create %s: %v", id, err)
			}
		}
		// Complete's 6th/7th args are durationMs / numTurns.
		if err := store.Complete(ctx, orgID, step1, "completed", 0.01, 1000, 1, "ok", "", "finish", "", ""); err != nil {
			t.Fatalf("Complete step1: %v", err)
		}
		if err := store.Complete(ctx, orgID, step2, "completed", 0.02, 2000, 1, "ok", "", "finish", "", ""); err != nil {
			t.Fatalf("Complete step2: %v", err)
		}

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
		// An unsettled sibling (created, never completed → NULL duration_ms)
		// contributes 0: SUM skips the NULL, COALESCE floors at 0.
		step3 := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.AgentRun{
			ID: step3, TaskID: taskID, PromptID: agentRunTestPrompt(t),
			Status: "running", Model: "m", BlueprintRunID: brID,
		}); err != nil {
			t.Fatalf("Create step3: %v", err)
		}
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
		runNoRow := uuid.New().String()
		runNullContent := uuid.New().String()
		runEmpty := uuid.New().String()
		runWhitespace := uuid.New().String()
		runPopulated := uuid.New().String()
		for _, id := range []string{runNoRow, runNullContent, runEmpty, runWhitespace, runPopulated} {
			if err := store.Create(ctx, orgID, domain.AgentRun{
				ID: id, TaskID: taskID, PromptID: agentRunTestPrompt(t), Status: "running", Model: "m",
				BlueprintRunID: seed.BlueprintRun(t, taskID),
			}); err != nil {
				t.Fatalf("Create %s: %v", id, err)
			}
		}
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

// seedAgentRunForTest creates a fresh entity+event+task+run chain and
// returns the run ID. status is what we want the run row to land in;
// the seeder calls the store with the status set directly rather
// than driving the lifecycle methods (which is the conformance
// suite's job to test).
func seedAgentRunForTest(t *testing.T, store db.AgentRunStore, orgID string, seed AgentRunSeeder, status string) string {
	t.Helper()
	ent := seed.Entity(t, "seed-"+status+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	ev := seed.Event(t, ent, domain.EventGitHubPROpened)
	taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
	return seedAgentRunForTaskTest(t, store, orgID, taskID, status, seed)
}

// seedAgentRunForTaskTest creates a run on an existing task, used
// by tests that need multiple runs on the same parent. Each run gets
// its own freshly-minted blueprint_run (runs.blueprint_run_id is NOT
// NULL); independent firings on a shared task is the realistic shape.
func seedAgentRunForTaskTest(t *testing.T, store db.AgentRunStore, orgID, taskID, status string, seed AgentRunSeeder) string {
	t.Helper()
	id := uuid.New().String()
	ctx := context.Background()
	if err := store.Create(ctx, orgID, domain.AgentRun{
		ID: id, TaskID: taskID, PromptID: agentRunTestPrompt(t), Status: status, Model: "m",
		BlueprintRunID: seed.BlueprintRun(t, taskID),
	}); err != nil {
		t.Fatalf("Create %s: %v", status, err)
	}
	return id
}

// seedAgentRunUnderBlueprintForTest creates a run on an existing task under a
// caller-supplied blueprint_run id, rather than minting a fresh parent. Used by
// the worktree-preserve subtest, which needs the run and the blueprint_run it
// then terminates to be the same pair.
func seedAgentRunUnderBlueprintForTest(t *testing.T, store db.AgentRunStore, orgID, taskID, blueprintRunID, status string) string {
	t.Helper()
	id := uuid.New().String()
	if err := store.Create(context.Background(), orgID, domain.AgentRun{
		ID: id, TaskID: taskID, PromptID: agentRunTestPrompt(t), Status: status, Model: "m",
		BlueprintRunID: blueprintRunID,
	}); err != nil {
		t.Fatalf("Create %s under blueprint_run: %v", status, err)
	}
	return id
}

// agentRunTestPromptID is the prompt-row id the backend test files
// seed once per test factory call. Conformance subtests reference
// it by this constant when creating runs; the seeder doesn't surface
// it as a field because every call uses the same value within one
// subtest.
const agentRunTestPromptID = "p_agentrun_test"

func agentRunTestPrompt(_ *testing.T) string { return agentRunTestPromptID }
