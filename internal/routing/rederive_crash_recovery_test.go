package routing

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// TestReDeriveAfterScoring_CrashResidueStrandsDeferredTrigger is the
// blast radius the scorer's crash-recovery reset closes, stated as a
// test: a task the scorer claimed ('in_progress') and never scored is
// invisible to UnscoredTasks, so its autonomy_suitability stays NULL and
// the re-derive — the whole promise behind deferring a
// min_autonomy_suitability trigger at event time — returns early on it.
// Nothing errors; the trigger just never fires.
func TestReDeriveAfterScoring_CrashResidueStrandsDeferredTrigger(t *testing.T) {
	ctx := context.Background()
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)
	scores := sqlitestore.New(database).Scores

	// The crash: claimed before the LLM call, process killed before the
	// scores landed.
	if err := scores.MarkScoring(ctx, runmode.LocalDefaultOrgID, []string{taskID}); err != nil {
		t.Fatalf("MarkScoring: %v", err)
	}

	unscored, err := scores.UnscoredTasks(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("UnscoredTasks: %v", err)
	}
	if len(unscored) != 0 {
		t.Fatalf("UnscoredTasks returned %d tasks, want 0 — the fixture must actually be stranded", len(unscored))
	}

	stub := &stubDelegator{db: database}
	crashResidueRouter(database, stub).ReDeriveAfterScoring(ctx, runmode.LocalDefaultOrgID, []string{taskID})
	if stub.calls != 0 {
		t.Errorf("unscored task delegated (%d calls), want 0 — re-derive has no score to gate on", stub.calls)
	}
}

// TestReDeriveAfterScoring_RecoveredResidueFiresDeferredTrigger is the
// same fixture carried through the recovery: the next cycle's
// ResetStaleScoring hands the stranded task back to UnscoredTasks, it
// gets scored, and the deferred trigger fires on the re-derive that
// follows.
func TestReDeriveAfterScoring_RecoveredResidueFiresDeferredTrigger(t *testing.T) {
	ctx := context.Background()
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)
	scores := sqlitestore.New(database).Scores

	if err := scores.MarkScoring(ctx, runmode.LocalDefaultOrgID, []string{taskID}); err != nil {
		t.Fatalf("MarkScoring: %v", err)
	}

	// Next cycle starts: recovery first, then the cycle picks its work.
	recovered, err := scores.ResetStaleScoring(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ResetStaleScoring: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("ResetStaleScoring recovered %d tasks, want 1", recovered)
	}
	unscored, err := scores.UnscoredTasks(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("UnscoredTasks: %v", err)
	}
	if len(unscored) != 1 || unscored[0].ID != taskID {
		t.Fatalf("UnscoredTasks = %v, want the recovered task %s", unscored, taskID)
	}
	if err := updateScores(t, database, []domain.TaskScoreUpdate{{
		ID: taskID, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "test",
	}}); err != nil {
		t.Fatalf("update scores: %v", err)
	}

	task, err := testTaskStore(database).Get(ctx, runmode.LocalDefaultOrgID, taskID)
	if err != nil || task == nil {
		t.Fatalf("get task: %v", err)
	}
	if task.AutonomySuitability == nil {
		t.Fatal("autonomy_suitability is NULL after the recovering cycle scored the task")
	}

	stub := &stubDelegator{db: database}
	crashResidueRouter(database, stub).ReDeriveAfterScoring(ctx, runmode.LocalDefaultOrgID, []string{taskID})
	if stub.calls != 1 {
		t.Errorf("delegate calls = %d, want 1 — the deferred trigger must fire once the recovered task is scored", stub.calls)
	}
}

// TestReDeriveAfterScoring_OwedDrainFiresAfterMissedCallback is the window
// on the far side of the scores commit, stated as a test: the scores landed,
// the process died before the post-scoring callback ran, and the task is now
// 'scored' — so no future cycle re-picks it and its deferred trigger would
// never be evaluated. The debt the scores write recorded is the only thing
// that gets it evaluated, and draining it must fire the trigger exactly once.
func TestReDeriveAfterScoring_OwedDrainFiresAfterMissedCallback(t *testing.T) {
	ctx := context.Background()
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)
	scores := sqlitestore.New(database).Scores

	// The crash: scores committed, callback never ran. Written through the
	// store so the owed mark comes from the same statement production uses.
	if err := scores.UpdateTaskScores(ctx, runmode.LocalDefaultOrgID, []domain.TaskScoreUpdate{{
		ID: taskID, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "test",
	}}); err != nil {
		t.Fatalf("UpdateTaskScores: %v", err)
	}
	unscored, err := scores.UnscoredTasks(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("UnscoredTasks: %v", err)
	}
	if len(unscored) != 0 {
		t.Fatalf("UnscoredTasks returned %d tasks, want 0 — a scored task is never re-picked, which is what makes the missed callback permanent", len(unscored))
	}

	// The next cycle's drain.
	owed, err := scores.TasksOwedReDerive(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("TasksOwedReDerive: %v", err)
	}
	if len(owed) != 1 || owed[0] != taskID {
		t.Fatalf("TasksOwedReDerive = %v, want [%s] — the scores write must record the debt", owed, taskID)
	}

	stub := &stubDelegator{db: database}
	router := crashResidueRouter(database, stub)
	router.ReDeriveAfterScoring(ctx, runmode.LocalDefaultOrgID, owed)

	if stub.calls != 1 {
		t.Fatalf("delegate calls = %d, want 1 — the drain is the only thing left that can fire this task's deferred trigger", stub.calls)
	}

	// The debt is discharged, so the cycle after that drains nothing...
	owed, err = scores.TasksOwedReDerive(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("TasksOwedReDerive (after the pass): %v", err)
	}
	if len(owed) != 0 {
		t.Errorf("TasksOwedReDerive after an evaluated pass = %v, want none", owed)
	}
	// ...and even a redundant pass over the same task fires nothing more:
	// the trigger sits behind the replay fence and the task's own agent
	// claim, which is why the drain needs no dedup of its own.
	router.ReDeriveAfterScoring(ctx, runmode.LocalDefaultOrgID, []string{taskID})
	if stub.calls != 1 {
		t.Errorf("delegate calls = %d after a redundant re-derive, want 1", stub.calls)
	}
	if runs := countBlueprintRuns(t, database); runs != 1 {
		t.Errorf("blueprint_runs = %d, want 1 — the (triggering_event_id, trigger_id) fence must absorb the replay", runs)
	}
}

// TestReDeriveAfterScoring_ClearsOwedMarkWithNothingToFire is the negative
// space: the overwhelming majority of scored tasks have no deferred trigger
// to fire. The pass still evaluated them, so it still discharges their debt
// — otherwise the owed set would grow without bound and every cycle would
// re-derive the whole board.
func TestReDeriveAfterScoring_ClearsOwedMarkWithNothingToFire(t *testing.T) {
	ctx := context.Background()
	database := newTestDB(t)
	// Threshold above the score below: the trigger is evaluated and declines.
	taskID, _ := setupReDeriveScenario(t, database, 0.9)
	scores := sqlitestore.New(database).Scores

	if err := scores.UpdateTaskScores(ctx, runmode.LocalDefaultOrgID, []domain.TaskScoreUpdate{{
		ID: taskID, PriorityScore: 0.5, AutonomySuitability: 0.2, Summary: "test",
	}}); err != nil {
		t.Fatalf("UpdateTaskScores: %v", err)
	}

	stub := &stubDelegator{db: database}
	crashResidueRouter(database, stub).ReDeriveAfterScoring(ctx, runmode.LocalDefaultOrgID, []string{taskID})

	if stub.calls != 0 {
		t.Errorf("delegate calls = %d, want 0 — the task scored below the trigger's threshold", stub.calls)
	}
	owed, err := scores.TasksOwedReDerive(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("TasksOwedReDerive: %v", err)
	}
	if len(owed) != 0 {
		t.Errorf("TasksOwedReDerive = %v, want none — a task with nothing to fire was still evaluated", owed)
	}
}

// TestReDeriveAfterScoring_KeepsOwedMarkWhenEvaluationBails is the other
// half of the clear's contract. The pass reads several stores per task; when
// one of them fails, the task was never actually decided. Clearing there
// would be the same silent loss the column exists to prevent, just with a
// store error in place of a crash — so the mark survives and the next
// cycle's drain asks again.
func TestReDeriveAfterScoring_KeepsOwedMarkWhenEvaluationBails(t *testing.T) {
	ctx := context.Background()
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)
	store := sqlitestore.New(database)

	if err := store.Scores.UpdateTaskScores(ctx, runmode.LocalDefaultOrgID, []domain.TaskScoreUpdate{{
		ID: taskID, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "test",
	}}); err != nil {
		t.Fatalf("UpdateTaskScores: %v", err)
	}

	stub := &stubDelegator{db: database}
	handlers := failingHandlerStore{
		EventHandlerStore: testEventHandlerStore(database),
		err:               errors.New("simulated store failure"),
	}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), handlers, nil, nil, nil,
		testTaskStore(database), store.Conversations, store.Entities, store.PendingFirings,
		store.Events, store.Orgs, store.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())
	router.SetReDeriveLedger(store.Scores)
	router.ReDeriveAfterScoring(ctx, runmode.LocalDefaultOrgID, []string{taskID})

	if stub.calls != 0 {
		t.Errorf("delegate calls = %d, want 0 — the trigger read failed, so nothing was decided", stub.calls)
	}
	owed, err := store.Scores.TasksOwedReDerive(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("TasksOwedReDerive: %v", err)
	}
	if len(owed) != 1 || owed[0] != taskID {
		t.Fatalf("TasksOwedReDerive after a bailed pass = %v, want [%s]", owed, taskID)
	}

	// The retry the surviving mark buys: same task, a store that works.
	crashResidueRouter(database, stub).ReDeriveAfterScoring(ctx, runmode.LocalDefaultOrgID, owed)
	if stub.calls != 1 {
		t.Errorf("delegate calls = %d on the retry, want 1", stub.calls)
	}
}

// failingHandlerStore fails the trigger lookup the re-derive pass makes per
// task, leaving every other store on the path working — the shape of a
// transient DB blip mid-evaluation.
type failingHandlerStore struct {
	db.EventHandlerStore
	err error
}

func (s failingHandlerStore) GetEnabledForEventSystem(context.Context, string, string) ([]domain.EventHandler, error) {
	return nil, s.err
}

func countBlueprintRuns(t *testing.T, database *sql.DB) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM blueprint_runs`).Scan(&n); err != nil {
		t.Fatalf("count blueprint_runs: %v", err)
	}
	return n
}

func crashResidueRouter(database *sql.DB, stub *stubDelegator) *Router {
	store := sqlitestore.New(database)
	r := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), store.Conversations, store.Entities, store.PendingFirings,
		store.Events, store.Orgs, store.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())
	r.SetReDeriveLedger(store.Scores)
	return r
}
