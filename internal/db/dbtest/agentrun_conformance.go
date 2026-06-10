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
		if err := store.Complete(ctx, orgID, runID, "completed", 0.75, 2000, 5, "ok", "all done", "abort", "needs human"); err != nil {
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

	t.Run("MarkResuming_OnlyFromOpen", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		ctx := context.Background()
		runID := seedAgentRunForTest(t, store, orgID, seed, "running")
		// Running → refused.
		ok, err := store.MarkResuming(ctx, orgID, runID)
		if err != nil || ok {
			t.Errorf("Resuming on running: ok=%v err=%v, want false", ok, err)
		}
		// Open it, then resume → ok.
		if _, err := store.MarkOpen(ctx, orgID, runID); err != nil {
			t.Fatalf("open: %v", err)
		}
		ok, err = store.MarkResuming(ctx, orgID, runID)
		if err != nil || !ok {
			t.Fatalf("Resuming from open: ok=%v err=%v", ok, err)
		}
		// Second resume → refused (back to running).
		ok, _ = store.MarkResuming(ctx, orgID, runID)
		if ok {
			t.Errorf("double-resume succeeded")
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
		ok, err := store.MarkFailedIfActive(ctx, orgID, openRun)
		if err != nil || !ok {
			t.Fatalf("MarkFailedIfActive on open: ok=%v err=%v, want true (open is failable)", ok, err)
		}
		if got, _ := store.Get(ctx, orgID, openRun); got.Status != "failed" {
			t.Errorf("status = %q, want failed", got.Status)
		}
		// pending_approval is protected (it has a durable snapshot) → refused.
		paRun := seedAgentRunForTest(t, store, orgID, seed, "pending_approval")
		if ok, _ := store.MarkFailedIfActive(ctx, orgID, paRun); ok {
			t.Errorf("MarkFailedIfActive flipped a pending_approval run; want refused")
		}
		// Already terminal → refused.
		doneRun := seedAgentRunForTest(t, store, orgID, seed, "completed")
		if ok, _ := store.MarkFailedIfActive(ctx, orgID, doneRun); ok {
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
		if err := store.Complete(ctx, orgID, eventRunID, "completed", 0, 0, 0, "", "", "finish", ""); err != nil {
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

		// open + pending_approval WITH a worktree path → included.
		openRun := seedAgentRunForTest(t, store, orgID, seed, "open")
		if err := store.SetWorktreePath(ctx, orgID, openRun, "/tmp/triagefactory-runs/open"); err != nil {
			t.Fatalf("set worktree (open): %v", err)
		}
		pending := seedAgentRunForTest(t, store, orgID, seed, "pending_approval")
		if err := store.SetWorktreePath(ctx, orgID, pending, "/tmp/triagefactory-runs/pending"); err != nil {
			t.Fatalf("set worktree (pending): %v", err)
		}
		// open WITHOUT a worktree → excluded by the COALESCE filter.
		_ = seedAgentRunForTest(t, store, orgID, seed, "open")
		// running WITH a worktree → excluded by the status filter.
		running := seedAgentRunForTest(t, store, orgID, seed, "running")
		if err := store.SetWorktreePath(ctx, orgID, running, "/tmp/triagefactory-runs/running"); err != nil {
			t.Fatalf("set worktree (running): %v", err)
		}

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
		if !got["/tmp/triagefactory-runs/pending"] {
			t.Error("pending_approval worktree missing from ListParkedWorktreePaths")
		}
		if got["/tmp/triagefactory-runs/running"] {
			t.Error("running worktree leaked — status filter failed")
		}
		if len(got) != 2 {
			t.Errorf("got %d parked paths, want 2 (%v)", len(got), paths)
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

// agentRunTestPromptID is the prompt-row id the backend test files
// seed once per test factory call. Conformance subtests reference
// it by this constant when creating runs; the seeder doesn't surface
// it as a field because every call uses the same value within one
// subtest.
const agentRunTestPromptID = "p_agentrun_test"

func agentRunTestPrompt(_ *testing.T) string { return agentRunTestPromptID }
