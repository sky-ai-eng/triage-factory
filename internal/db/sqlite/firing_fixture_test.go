package sqlite_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedSqliteFiringParents stages the rows one delegation needs before it can
// be fired: a task, a prompt, and a blueprint whose only step is that prompt.
// Every id it writes is keyed by prefix, so two fixtures in one database never
// collide.
func seedSqliteFiringParents(t *testing.T, conn *sql.DB, stores db.Stores, prefix string) (bpID, taskID, promptID string) {
	t.Helper()
	promptID = prefix + "-p0"
	bpID = prefix + "-bp"
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: promptID, Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, bpID, "Blueprint "+prefix)
	if _, err := stores.Blueprints.ReplaceSteps(context.Background(), runmode.LocalDefaultOrgID, bpID, []string{promptID}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	return bpID, seedEntityEventTask(t, conn, prefix).ID, promptID
}

// fireSqliteStep stages one claimable step the way production stages one:
// through BlueprintStore.CreateRunWithFirstStepSystem, which commits the
// blueprint_run and its step-0 conversations row in a single transaction.
//
// step carries the step's own identity — ID (minted here when empty),
// PromptID, Model, ActorAgentID, PreferredExecutorID. The run it belongs to,
// its index and its task are filled in here.
//
// One running blueprint_run per task is a schema invariant, so the task's
// prior run is settled first — which is what a task moving on to its next
// engagement means. A fixture staging several conversations claimable at the
// SAME instant therefore needs a task apiece, which is the real firing model
// anyway: one delegation is one blueprint_run on one task.
func fireSqliteStep(t *testing.T, conn *sql.DB, stores db.Stores, bpID, taskID string, step domain.Conversation) *domain.Conversation {
	t.Helper()
	if step.BlueprintStepIndex == nil {
		step0 := 0
		step.BlueprintStepIndex = &step0
	}
	conv, err := tryFireSqliteStep(t, conn, stores, bpID, taskID, step)
	if err != nil {
		t.Fatalf("CreateRunWithFirstStepSystem: %v", err)
	}
	if conv == nil {
		t.Fatal("CreateRunWithFirstStepSystem returned no conversation for a fresh firing")
	}
	return conv
}

// tryFireSqliteStep is fireSqliteStep for the subtests whose subject IS a
// refusal, so they need the error rather than a fatal. It fills in nothing the
// caller left deliberately empty below the run: a nil BlueprintStepIndex stays
// nil, which is what the unindexed-mint refusal is staged with.
func tryFireSqliteStep(t *testing.T, conn *sql.DB, stores db.Stores, bpID, taskID string, step domain.Conversation) (*domain.Conversation, error) {
	t.Helper()
	return tryFireSqliteRun(t, conn, stores, domain.BlueprintRun{BlueprintID: bpID, TaskID: taskID}, step)
}

// fireSqliteRun fires a run the fixture composed itself — the variant for
// tests that name the run's own columns (its id, its frozen plan, its actor)
// rather than only the step riding on it. Anything br leaves empty is
// defaulted; the step's run, task and index are filled in from br.
func fireSqliteRun(t *testing.T, conn *sql.DB, stores db.Stores, br domain.BlueprintRun, step domain.Conversation) *domain.Conversation {
	t.Helper()
	if step.BlueprintStepIndex == nil {
		step0 := 0
		step.BlueprintStepIndex = &step0
	}
	conv, err := tryFireSqliteRun(t, conn, stores, br, step)
	if err != nil {
		t.Fatalf("CreateRunWithFirstStepSystem: %v", err)
	}
	if conv == nil {
		t.Fatal("CreateRunWithFirstStepSystem returned no conversation for a fresh firing")
	}
	return conv
}

// tryFireSqliteRun is the one place this package's fixtures reach the firing
// door. Everything above it composes a run and a step and lands here.
func tryFireSqliteRun(t *testing.T, conn *sql.DB, stores db.Stores, br domain.BlueprintRun, step domain.Conversation) (*domain.Conversation, error) {
	t.Helper()
	if _, err := conn.Exec(`UPDATE blueprint_runs SET status = 'completed' WHERE task_id = ? AND status = 'running'`, br.TaskID); err != nil {
		t.Fatalf("settle the task's prior blueprint_run: %v", err)
	}
	if br.ID == "" {
		br.ID = uuid.New().String()
	}
	if br.TriggerType == "" {
		br.TriggerType = domain.BlueprintTriggerManual
	}
	if br.WorktreePath == "" {
		br.WorktreePath = "/tmp/wt-" + br.ID
	}
	if step.ID == "" {
		step.ID = uuid.New().String()
	}
	step.TaskID = br.TaskID
	step.BlueprintRunID = br.ID
	if step.TriggerType == "" {
		step.TriggerType = string(br.TriggerType)
	}
	if step.Model == "" {
		step.Model = "m"
	}
	_, _, conv, err := stores.Blueprints.CreateRunWithFirstStepSystem(
		context.Background(), runmode.LocalDefaultOrgID, br, db.AgentClaimStamp{}, "", step)
	return conv, err
}

// stageSqliteStep is the whole fixture in one call, for the many tests that
// just need one claimable step and do not name its parents.
func stageSqliteStep(t *testing.T, conn *sql.DB, stores db.Stores, prefix string) *domain.Conversation {
	t.Helper()
	bpID, taskID, promptID := seedSqliteFiringParents(t, conn, stores, prefix)
	return fireSqliteStep(t, conn, stores, bpID, taskID, domain.Conversation{PromptID: promptID})
}

// advanceSqliteStep mints the step after prev the way production mints one:
// through BlueprintStore.AdvanceRunToStepSystem, which moves the
// blueprint_run's current_step_index, ends the step it moves past and writes
// the next step's conversations row in one transaction. next inherits prev's
// run and task and lands one index on, so a fixture walking a sequence just
// hands back what the last call returned.
func advanceSqliteStep(t *testing.T, stores db.Stores, prev *domain.Conversation, next domain.Conversation) *domain.Conversation {
	t.Helper()
	if prev.BlueprintStepIndex == nil {
		t.Fatalf("advancing past conversation %s, which records no step index", prev.ID)
	}
	from := *prev.BlueprintStepIndex
	to := from + 1
	if next.ID == "" {
		next.ID = uuid.New().String()
	}
	next.TaskID = prev.TaskID
	next.BlueprintRunID = prev.BlueprintRunID
	next.BlueprintStepIndex = &to
	if next.TriggerType == "" {
		next.TriggerType = "manual"
	}
	if next.Model == "" {
		next.Model = "m"
	}
	advanced, _, conv, err := stores.Blueprints.AdvanceRunToStepSystem(
		context.Background(), runmode.LocalDefaultOrgID, from, prev.ID, next)
	if err != nil {
		t.Fatalf("AdvanceRunToStepSystem: %v", err)
	}
	if !advanced || conv == nil {
		t.Fatalf("AdvanceRunToStepSystem from step %d = (advanced=%v, conv=%v), want the next step", from, advanced, conv != nil)
	}
	return conv
}
