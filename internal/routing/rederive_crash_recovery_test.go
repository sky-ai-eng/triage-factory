package routing

import (
	"context"
	"database/sql"
	"testing"

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

func crashResidueRouter(database *sql.DB, stub *stubDelegator) *Router {
	store := sqlitestore.New(database)
	return NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), store.Conversations, store.Entities, store.PendingFirings,
		store.Events, store.Orgs, store.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())
}
