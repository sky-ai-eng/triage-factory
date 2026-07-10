package routing

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// stubDelegator records every Delegate call and creates a real run row
// each time so MarkPendingFiringFired's FK to runs(id) is satisfied. Used
// by the drain race test to count fire attempts under concurrency.
type stubDelegator struct {
	db    *sql.DB
	calls int64

	// mu guards lastTaskTeamID, the task's owner team_id observed at the
	// most recent Delegate call. Tests use it to assert the owner was
	// consolidated to the acting team before the run was created.
	mu             sync.Mutex
	lastTaskTeamID string
}

func (s *stubDelegator) Delegate(task domain.Task, opts delegate.DelegateOpts) (string, error) {
	atomic.AddInt64(&s.calls, 1)
	s.mu.Lock()
	s.lastTaskTeamID = teamIDValue(&task)
	s.mu.Unlock()
	return stubDelegateRun(s.db, task, opts)
}

// stubDelegateRun mirrors the production Delegate path for the router tests: it
// mints a blueprint_run (fenced on (triggering_event_id, trigger_id) for event
// triggers — the relocated replay fence) and a linked run row (runs.blueprint_run_id
// is NOT NULL). Returns the blueprint_run id, or delegate.ErrAlreadyFired when an
// event replay trips the fence. No worktree/agent is stood up — only the DB rows
// the router's ErrAlreadyFired + run-count assertions read.
func stubDelegateRun(database *sql.DB, task domain.Task, opts delegate.DelegateOpts) (string, error) {
	store := sqlitestore.New(database)
	// opts.ExplicitBlueprintID is a blueprint id; the run row's prompt_id FK
	// needs a real prompt, so resolve the blueprint's first step prompt.
	promptID := opts.ExplicitBlueprintID
	if promptID != "" {
		if steps, err := store.Blueprints.ListSteps(context.Background(), runmode.LocalDefaultOrgID, opts.ExplicitBlueprintID); err == nil && len(steps) > 0 {
			promptID = steps[0].StepPromptID
		}
	}
	brID := uuid.New().String()
	br := domain.BlueprintRun{
		ID:                brID,
		BlueprintID:       opts.ExplicitBlueprintID,
		TaskID:            task.ID,
		TriggerType:       domain.BlueprintTriggerType(opts.TriggerType),
		TriggerID:         opts.TriggerID,
		TriggeringEventID: opts.TriggeringEventID,
		// Mirror production: freeze the caller-resolved actor onto the
		// blueprint_run so a router test can assert it matches the task claim.
		ActorAgentID: opts.ActorAgentID,
		Status:       domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-" + brID,
	}
	if opts.TriggerType == "event" {
		inserted, err := store.Blueprints.CreateRunIfNotFiredSystem(context.Background(), runmode.LocalDefaultOrgID, br)
		if err != nil {
			return "", err
		}
		if !inserted {
			return "", delegate.ErrAlreadyFired
		}
	} else if _, err := store.Blueprints.CreateRun(context.Background(), runmode.LocalDefaultOrgID, br); err != nil {
		return "", err
	}
	stepIdx := 0
	if err := store.AgentRuns.Create(context.Background(), runmode.LocalDefaultOrgID, domain.AgentRun{
		ID:                 uuid.New().String(),
		TaskID:             task.ID,
		PromptID:           promptID,
		Status:             "running",
		Model:              "stub",
		TriggerType:        opts.TriggerType,
		TriggerID:          opts.TriggerID,
		CreatorUserID:      opts.CreatorUserID,
		BlueprintRunID:     brID,
		BlueprintStepIndex: &stepIdx,
	}); err != nil {
		return "", err
	}
	return brID, nil
}

func (s *stubDelegator) Cancel(orgID, runID, userID string) error { return nil }

func (s *stubDelegator) StageOrDeliverAdditiveEvent(ctx context.Context, orgID, runID, producer, body string, firing delegate.AdditiveFiringRef) delegate.InjectOutcome {
	return delegate.InjectNotDelivered
}

// setupDrainScenario seeds entity + prompt + event + task + trigger so a
// pending firing can be enqueued and drained against a realistic FK graph.
func setupDrainScenario(t *testing.T, database *sql.DB) (entityID, taskID, triggerID, eventID string) {
	t.Helper()

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#1", "pr",
		"Test PR", "https://github.com/owner/repo/pull/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	entityID = entity.ID

	createTestPrompt(t, database, domain.Prompt{
		ID: "p-drain", Name: "P", Body: "x", Source: "user",
	})

	eventID, err = sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entityID,
		DedupKey:     "build",
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}

	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entityID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	taskID = task.ID

	// Drain checks task.ClaimedByAgentID before firing.
	// In production the enqueue path (tryAutoDelegate) stamps the
	// claim when a firing lands in pending_firings; the test setup
	// here bypasses that by inserting the firing directly, so stamp
	// the claim explicitly with the local agent sentinel.
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := testTaskStore(database).SetClaimedByAgent(t.Context(), runmode.LocalDefaultOrgID, taskID, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("stamp claim: %v", err)
	}

	trig := domain.EventHandler{
		ID:                     "t-drain",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "p-drain",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRCICheckFailed,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(0),
		Enabled:                true,
	}
	createTriggerForTestRouting(t, database, trig)
	triggerID = trig.ID
	return
}

// TestDrainEntity_ClosedTask verifies the drain re-validates task state at
// drain time. A firing whose task closed mid-pause must transition to
// skipped_stale with task_closed rather than firing into a dead task.
func TestDrainEntity_ClosedTask(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)

	if _, err := sqlitestore.New(database).PendingFirings.Enqueue(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, entityID, taskID, triggerID, eventID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Close the task between enqueue and drain — simulates an inline close
	// check or entity cascade resolving the task while a firing waits.
	if err := testTaskStore(database).Close(t.Context(), runmode.LocalDefaultOrgID, taskID, "test_close", ""); err != nil {
		t.Fatalf("close task: %v", err)
	}

	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	router.DrainEntity(runmode.LocalDefaultOrgID, entityID)

	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	if rows[0].Status != domain.PendingFiringStatusSkippedStale {
		t.Errorf("status = %q, want %q", rows[0].Status, domain.PendingFiringStatusSkippedStale)
	}
	if rows[0].SkipReason != domain.PendingFiringSkipTaskClosed {
		t.Errorf("skip_reason = %q, want %q", rows[0].SkipReason, domain.PendingFiringSkipTaskClosed)
	}
}

// TestRevertTaskStatus_PreservesClaim pins the contract that
// revertTaskStatus only touches the lifecycle axis. Its sole caller
// (the mark-fired-failure rollback in DrainEntity) leaves the
// pending_firings row in 'pending' so the next drain retries; the
// retry's attemptDrainOne gate requires the bot
// claim to still be set or it skips with claim_changed. Clearing
// the claim cols here would silently drop the queued intent — the
// guard would fire and the retry never would. This test pins that
// the bot claim survives the revert.
func TestRevertTaskStatus_PreservesClaim(t *testing.T) {
	database := newTestDB(t)
	_, taskID, _, _ := setupDrainScenario(t, database)

	// Move the task off 'queued' to simulate mid-flight state the
	// rollback path would observe. (Pre-B+ fireDelegate flipped to
	// 'delegated'; post-B+ the status stays 'queued' on commit, but
	// we're testing revert independently of the caller path, so set
	// status to something visibly distinct so the assertion catches
	// a regression where SetTaskStatus isn't called either.)
	if err := testTaskStore(database).SetStatus(t.Context(), runmode.LocalDefaultOrgID, taskID, "snoozed"); err != nil {
		t.Fatalf("pre-stage status: %v", err)
	}

	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	router.revertTaskStatus(runmode.LocalDefaultOrgID, taskID, "queued")

	task, err := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if err != nil || task == nil {
		t.Fatalf("read task: task=%v err=%v", task, err)
	}
	if task.Status != "queued" {
		t.Errorf("Status = %q, want queued (lifecycle revert must fire)", task.Status)
	}
	if task.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("ClaimedByAgentID = %q, want %q (revert must NOT clear claim — retry needs it)",
			task.ClaimedByAgentID, runmode.LocalDefaultAgentID)
	}
	if task.ClaimedByUserID != "" {
		t.Errorf("ClaimedByUserID = %q, want empty", task.ClaimedByUserID)
	}
}

// TestDrainEntity_SnoozedTask pins the semantic that snooze is
// a lifecycle-axis "do not act" signal that's orthogonal to claim. A
// pending firing for a bot-claimed task that gets snoozed (e.g., user
// said "wait until Tuesday" while the firing was queued behind a busy
// entity) must not fire when the entity slot opens. The drain
// classifies snoozed alongside done/dismissed under task_closed —
// all three mean "task is not currently drain-eligible." A snooze
// wake-on-bump creates a fresh event → new firing if the trigger
// still matches; the deferred firing is the wrong wake path.
func TestDrainEntity_SnoozedTask(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)

	if _, err := sqlitestore.New(database).PendingFirings.Enqueue(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, entityID, taskID, triggerID, eventID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Bot-claim the task (drain would otherwise short-circuit on
	// claim_changed before the lifecycle check) AND snooze it.
	if err := testTaskStore(database).SetClaimedByAgent(t.Context(), runmode.LocalDefaultOrgID, taskID, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("stamp claim: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE tasks SET status = 'snoozed', snooze_until = '2099-01-01 00:00:00' WHERE id = ?`,
		taskID,
	); err != nil {
		t.Fatalf("snooze task: %v", err)
	}

	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	router.DrainEntity(runmode.LocalDefaultOrgID, entityID)

	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	if rows[0].Status != domain.PendingFiringStatusSkippedStale {
		t.Errorf("status = %q, want %q (snoozed task should skip drain, not fire)", rows[0].Status, domain.PendingFiringStatusSkippedStale)
	}
	if rows[0].SkipReason != domain.PendingFiringSkipTaskClosed {
		t.Errorf("skip_reason = %q, want %q (snoozed grouped under task_closed on the lifecycle-skip axis)",
			rows[0].SkipReason, domain.PendingFiringSkipTaskClosed)
	}
}

// TestDrainEntity_DisabledTrigger verifies the drain respects current
// trigger state. A trigger disabled mid-pause must not fire its queued
// firings on resume.
func TestDrainEntity_DisabledTrigger(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)

	if _, err := sqlitestore.New(database).PendingFirings.Enqueue(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, entityID, taskID, triggerID, eventID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	setTriggerEnabledForTestRouting(t, database, triggerID, false)

	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	router.DrainEntity(runmode.LocalDefaultOrgID, entityID)

	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	if rows[0].Status != domain.PendingFiringStatusSkippedStale {
		t.Errorf("status = %q, want %q", rows[0].Status, domain.PendingFiringStatusSkippedStale)
	}
	if rows[0].SkipReason != domain.PendingFiringSkipTriggerDisabled {
		t.Errorf("skip_reason = %q, want %q", rows[0].SkipReason, domain.PendingFiringSkipTriggerDisabled)
	}
}

// TestDrainEntity_MultipleStaleFirings verifies the drain loop walks past
// stale firings rather than stopping at the first one. Three queued
// firings, all with closed tasks → all three must end up marked
// skipped_stale (none left in pending).
func TestDrainEntity_MultipleStaleFirings(t *testing.T) {
	database := newTestDB(t)
	entityID, _, _, eventID := setupDrainScenario(t, database)

	// Three distinct (task, trigger) pairs so the dedup index lets all
	// three coexist as pending. Distinct prompts because prompt_triggers
	// has a unique index on (prompt_id, event_type, trigger_type).
	taskIDs := []string{}
	triggerIDs := []string{}
	for i := 0; i < 3; i++ {
		dedup := []string{"checkA", "checkB", "checkC"}[i]
		task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entityID, domain.EventGitHubPRCICheckFailed, dedup, eventID, 0.5)
		if err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
		taskIDs = append(taskIDs, task.ID)

		promptID := []string{"p-1", "p-2", "p-3"}[i]
		createTestPrompt(t, database, domain.Prompt{ID: promptID, Name: promptID, Body: "x", Source: "user"})
		trigID := []string{"tr-1", "tr-2", "tr-3"}[i]
		createTriggerForTestRouting(t, database, domain.EventHandler{
			ID: trigID, Kind: domain.EventHandlerKindTrigger,
			BlueprintID: promptID, TriggerType: domain.TriggerTypeEvent,
			EventType:        domain.EventGitHubPRCICheckFailed,
			BreakerThreshold: intPtr(4), MinAutonomySuitability: floatPtr(0),
			Enabled: true,
		})
		triggerIDs = append(triggerIDs, trigID)

		if _, err := sqlitestore.New(database).PendingFirings.Enqueue(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, entityID, task.ID, trigID, eventID); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// Close every task so all three drain as task_closed.
	for _, id := range taskIDs {
		if err := testTaskStore(database).Close(t.Context(), runmode.LocalDefaultOrgID, id, "test_close", ""); err != nil {
			t.Fatalf("close task %s: %v", id, err)
		}
	}

	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	router.DrainEntity(runmode.LocalDefaultOrgID, entityID)

	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 firing rows, got %d", len(rows))
	}
	for i, r := range rows {
		if r.Status != domain.PendingFiringStatusSkippedStale {
			t.Errorf("row %d: status = %q, want skipped_stale", i, r.Status)
		}
		if r.SkipReason != domain.PendingFiringSkipTaskClosed {
			t.Errorf("row %d: skip_reason = %q, want task_closed", i, r.SkipReason)
		}
	}
	_ = triggerIDs // referenced for clarity in the loop above
}

// TestDrainEntity_EmptyQueue verifies a drain on an entity with no pending
// firings is a clean no-op — no error, no side effects, no panic.
func TestDrainEntity_EmptyQueue(t *testing.T) {
	database := newTestDB(t)
	entityID, _, _, _ := setupDrainScenario(t, database)

	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	router.DrainEntity(runmode.LocalDefaultOrgID, entityID) // must not panic or error visibly

	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected empty queue, got %d rows", len(rows))
	}
}

// TestDrainEntity_ConcurrentDrainsDoNotDoubleFire is the regression test
// for the pop-fire-mark race: without per-entity serialization, a fast-
// terminating run fired by drainer A could trigger drainer B before A
// reached MarkPendingFiringFired, and B would pop the same still-pending
// row and call Delegate again. With the per-entity mutex, the second
// drainer blocks until the first marks the firing terminal, then sees
// nothing pending and returns clean.
//
// We model the race directly by spawning N drainers concurrently against
// a single pending firing and asserting Delegate fires exactly once.
// Without the mutex this test is reliably racy in practice (every drainer
// passes the validation guard, every drainer calls Delegate); with it,
// only one drainer ever reaches Delegate.
func TestDrainEntity_ConcurrentDrainsDoNotDoubleFire(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)

	if _, err := sqlitestore.New(database).PendingFirings.Enqueue(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, entityID, taskID, triggerID, eventID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())

	const drainers = 5
	var wg sync.WaitGroup
	wg.Add(drainers)
	for i := 0; i < drainers; i++ {
		go func() {
			defer wg.Done()
			router.DrainEntity(runmode.LocalDefaultOrgID, entityID)
		}()
	}
	wg.Wait()

	calls := atomic.LoadInt64(&stub.calls)
	if calls != 1 {
		t.Errorf("expected exactly 1 Delegate call across %d concurrent drains, got %d",
			drainers, calls)
	}

	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	if rows[0].Status != domain.PendingFiringStatusFired {
		t.Errorf("status = %q, want fired", rows[0].Status)
	}
	if rows[0].FiredRunID == nil {
		t.Error("fired_run_id should be set on the winning drain's mark")
	}
}

// TestRunDrainSweeper_PicksUpStuckFiring verifies that the periodic
// sweeper drains pending firings even when no notifyDrainer call ever
// fires (the stuck-queue case: queue has pending entries, no active auto
// runs, no incoming events). Models the safety-net scenario the sweeper
// exists for.
//
// Setup: enqueue a firing without ever calling DrainEntity manually.
// Without the sweeper this firing would sit in 'pending' forever. Start
// the sweeper at 30ms cadence; within a few ticks it should pick up the
// firing, validate, and fire.
func TestRunDrainSweeper_PicksUpStuckFiring(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)

	if _, err := sqlitestore.New(database).PendingFirings.Enqueue(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, entityID, taskID, triggerID, eventID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go router.RunDrainSweeper(ctx, 30*time.Millisecond)

	// Poll for completion: sweeper must drain within a generous window.
	// Wait for the firing's final status rather than just stub.calls,
	// because the sweeper increments calls inside Delegate and only
	// then runs MarkPendingFiringFired — observing calls==1 alone
	// doesn't tell us the row has been transitioned. Under -race the
	// gap between the two becomes large enough to flake. 1s gives
	// ~100 ticks; if status hasn't reached 'fired' by then something
	// is genuinely wrong.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
		if err == nil && len(rows) == 1 && rows[0].Status == domain.PendingFiringStatusFired {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if calls := atomic.LoadInt64(&stub.calls); calls != 1 {
		t.Fatalf("expected sweeper to fire the stuck firing exactly once, got %d calls", calls)
	}

	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	if rows[0].Status != domain.PendingFiringStatusFired {
		t.Errorf("status = %q, want fired", rows[0].Status)
	}
}

// TestRunDrainSweeper_NoOpWhenIdle verifies the sweeper doesn't
// gratuitously fire when there are no pending firings to drain. Stops
// "every 30s the binary spuriously creates runs" regression.
func TestRunDrainSweeper_NoOpWhenIdle(t *testing.T) {
	database := newTestDB(t)
	entityID, _, _, _ := setupDrainScenario(t, database)
	_ = entityID

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go router.RunDrainSweeper(ctx, 20*time.Millisecond)

	// Let several ticks elapse with nothing pending.
	time.Sleep(120 * time.Millisecond)

	if calls := atomic.LoadInt64(&stub.calls); calls != 0 {
		t.Errorf("expected 0 Delegate calls with empty queue, got %d", calls)
	}
}
