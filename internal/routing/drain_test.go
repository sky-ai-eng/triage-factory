package routing

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// stubDelegator records every Delegate call and creates a real run row
// each time so MarkPendingFiringFired's FK to runs(id) is satisfied. Used
// by the drain race test to count fire attempts under concurrency.
type stubDelegator struct {
	db    *sql.DB
	calls int64
}

func (s *stubDelegator) Delegate(task domain.Task, promptID, triggerType, triggerID string) (string, error) {
	atomic.AddInt64(&s.calls, 1)
	runID := fmt.Sprintf("stub-run-%d", time.Now().UnixNano())
	if err := db.CreateAgentRun(s.db, domain.AgentRun{
		ID:          runID,
		TaskID:      task.ID,
		PromptID:    promptID,
		Status:      "running",
		Model:       "stub",
		TriggerType: triggerType,
		TriggerID:   triggerID,
	}); err != nil {
		return "", err
	}
	return runID, nil
}

func (s *stubDelegator) Cancel(runID string) error { return nil }

// setupDrainScenario seeds entity + prompt + event + task + trigger so a
// pending firing can be enqueued and drained against a realistic FK graph.
func setupDrainScenario(t *testing.T, database *sql.DB) (entityID, taskID, triggerID, eventID string) {
	t.Helper()

	entity, _, err := db.FindOrCreateEntity(database, "github", "owner/repo#1", "pr",
		"Test PR", "https://github.com/owner/repo/pull/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	entityID = entity.ID

	if err := db.CreatePrompt(database, domain.Prompt{
		ID: "p-drain", Name: "P", Body: "x", Source: "user",
	}); err != nil {
		t.Fatalf("create prompt: %v", err)
	}

	eventID, err = db.RecordEvent(database, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entityID,
		DedupKey:     "build",
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}

	task, _, err := db.FindOrCreateTask(database, entityID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	taskID = task.ID

	trig := domain.PromptTrigger{
		ID:               "t-drain",
		PromptID:         "p-drain",
		TriggerType:      domain.TriggerTypeEvent,
		EventType:        domain.EventGitHubPRCICheckFailed,
		BreakerThreshold: 4,
		Enabled:          true,
	}
	if err := db.SavePromptTrigger(database, trig); err != nil {
		t.Fatalf("save trigger: %v", err)
	}
	triggerID = trig.ID
	return
}

// TestDrainEntity_ClosedTask verifies the drain re-validates task state at
// drain time. A firing whose task closed mid-pause must transition to
// skipped_stale with task_closed rather than firing into a dead task.
func TestDrainEntity_ClosedTask(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)

	if _, err := db.EnqueuePendingFiring(database, entityID, taskID, triggerID, eventID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Close the task between enqueue and drain — simulates an inline close
	// check or entity cascade resolving the task while a firing waits.
	if err := db.CloseTask(database, taskID, "test_close", ""); err != nil {
		t.Fatalf("close task: %v", err)
	}

	router := NewRouter(database, nil, noopScorer{}, websocket.NewHub())
	router.DrainEntity(entityID)

	rows, err := db.ListPendingFiringsForEntity(database, entityID)
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

// TestDrainEntity_DisabledTrigger verifies the drain respects current
// trigger state. A trigger disabled mid-pause must not fire its queued
// firings on resume.
func TestDrainEntity_DisabledTrigger(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)

	if _, err := db.EnqueuePendingFiring(database, entityID, taskID, triggerID, eventID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if err := db.SetTriggerEnabled(database, triggerID, false); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}

	router := NewRouter(database, nil, noopScorer{}, websocket.NewHub())
	router.DrainEntity(entityID)

	rows, err := db.ListPendingFiringsForEntity(database, entityID)
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
		task, _, err := db.FindOrCreateTask(database, entityID, domain.EventGitHubPRCICheckFailed, dedup, eventID, 0.5)
		if err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
		taskIDs = append(taskIDs, task.ID)

		promptID := []string{"p-1", "p-2", "p-3"}[i]
		if err := db.CreatePrompt(database, domain.Prompt{ID: promptID, Name: promptID, Body: "x", Source: "user"}); err != nil {
			t.Fatalf("create prompt %s: %v", promptID, err)
		}
		trigID := []string{"tr-1", "tr-2", "tr-3"}[i]
		if err := db.SavePromptTrigger(database, domain.PromptTrigger{
			ID: trigID, PromptID: promptID, TriggerType: domain.TriggerTypeEvent,
			EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: 4, Enabled: true,
		}); err != nil {
			t.Fatalf("save trigger %s: %v", trigID, err)
		}
		triggerIDs = append(triggerIDs, trigID)

		if _, err := db.EnqueuePendingFiring(database, entityID, task.ID, trigID, eventID); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// Close every task so all three drain as task_closed.
	for _, id := range taskIDs {
		if err := db.CloseTask(database, id, "test_close", ""); err != nil {
			t.Fatalf("close task %s: %v", id, err)
		}
	}

	router := NewRouter(database, nil, noopScorer{}, websocket.NewHub())
	router.DrainEntity(entityID)

	rows, err := db.ListPendingFiringsForEntity(database, entityID)
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

	router := NewRouter(database, nil, noopScorer{}, websocket.NewHub())
	router.DrainEntity(entityID) // must not panic or error visibly

	rows, err := db.ListPendingFiringsForEntity(database, entityID)
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

	if _, err := db.EnqueuePendingFiring(database, entityID, taskID, triggerID, eventID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	stub := &stubDelegator{db: database}
	router := NewRouter(database, stub, noopScorer{}, websocket.NewHub())

	const drainers = 5
	var wg sync.WaitGroup
	wg.Add(drainers)
	for i := 0; i < drainers; i++ {
		go func() {
			defer wg.Done()
			router.DrainEntity(entityID)
		}()
	}
	wg.Wait()

	calls := atomic.LoadInt64(&stub.calls)
	if calls != 1 {
		t.Errorf("expected exactly 1 Delegate call across %d concurrent drains, got %d",
			drainers, calls)
	}

	rows, err := db.ListPendingFiringsForEntity(database, entityID)
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

	if _, err := db.EnqueuePendingFiring(database, entityID, taskID, triggerID, eventID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	stub := &stubDelegator{db: database}
	router := NewRouter(database, stub, noopScorer{}, websocket.NewHub())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go router.RunDrainSweeper(ctx, 30*time.Millisecond)

	// Poll for completion: sweeper must drain within a generous window.
	// 1s gives ~30 ticks; if it hasn't fired by then something's wrong.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&stub.calls) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if calls := atomic.LoadInt64(&stub.calls); calls != 1 {
		t.Fatalf("expected sweeper to fire the stuck firing exactly once, got %d calls", calls)
	}

	rows, err := db.ListPendingFiringsForEntity(database, entityID)
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
	router := NewRouter(database, stub, noopScorer{}, websocket.NewHub())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go router.RunDrainSweeper(ctx, 20*time.Millisecond)

	// Let several ticks elapse with nothing pending.
	time.Sleep(120 * time.Millisecond)

	if calls := atomic.LoadInt64(&stub.calls); calls != 0 {
		t.Errorf("expected 0 Delegate calls with empty queue, got %d", calls)
	}
}
