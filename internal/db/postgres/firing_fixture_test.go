package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// firePgStep stages one claimable step the way production stages one: through
// BlueprintStore.CreateRunWithFirstStepSystem, which commits the
// blueprint_run and its step-0 conversations row in a single transaction.
//
// step carries the step's own identity — ID (minted here when empty),
// PromptID, Model, CreatorUserID, ActorAgentID, PreferredExecutorID. The run
// it belongs to, its index and its task are filled in here. The firing is a
// manual one, so CreatorUserID is required: it is the run's creator as well
// as the step's.
//
// One running blueprint_run per task is a schema invariant
// (blueprint_runs_one_active_run_per_task), so the task's prior run is
// settled first — which is what a task moving on to its next engagement
// means. A fixture staging several conversations claimable at the SAME
// instant therefore needs a task apiece, which is the real firing model
// anyway: one delegation is one blueprint_run on one task.
func firePgStep(t *testing.T, h *pgtest.Harness, stores db.Stores, orgID, bpID, taskID string, step domain.Conversation) *domain.Conversation {
	t.Helper()
	if step.BlueprintStepIndex == nil {
		step0 := 0
		step.BlueprintStepIndex = &step0
	}
	conv, err := tryFirePgStep(t, h, stores, orgID, bpID, taskID, step)
	if err != nil {
		t.Fatalf("CreateRunWithFirstStepSystem: %v", err)
	}
	if conv == nil {
		t.Fatal("CreateRunWithFirstStepSystem returned no conversation for a fresh firing")
	}
	return conv
}

// tryFirePgStep is firePgStep for the subtests whose subject IS a refusal, so
// they need the error rather than a fatal. It fills in nothing the caller left
// deliberately empty below the run: a nil BlueprintStepIndex stays nil, which
// is what the unindexed-mint refusal is staged with.
func tryFirePgStep(t *testing.T, h *pgtest.Harness, stores db.Stores, orgID, bpID, taskID string, step domain.Conversation) (*domain.Conversation, error) {
	t.Helper()
	if _, err := h.AdminDB.Exec(
		`UPDATE blueprint_runs SET status = 'completed' WHERE org_id = $1 AND task_id = $2 AND status = 'running'`,
		orgID, taskID,
	); err != nil {
		t.Fatalf("settle the task's prior blueprint_run: %v", err)
	}
	brID := uuid.New().String()
	if step.ID == "" {
		step.ID = uuid.New().String()
	}
	step.TaskID = taskID
	step.BlueprintRunID = brID
	if step.TriggerType == "" {
		step.TriggerType = "manual"
	}
	if step.Model == "" {
		step.Model = "m"
	}
	_, _, conv, err := stores.Blueprints.CreateRunWithFirstStepSystem(context.Background(), orgID, domain.BlueprintRun{
		ID:           brID,
		BlueprintID:  bpID,
		TaskID:       taskID,
		TriggerType:  domain.BlueprintTriggerManual,
		WorktreePath: "/tmp/wt-" + brID,
	}, db.AgentClaimStamp{}, "", step)
	return conv, err
}

// advancePgStep mints the step after prev the way production mints one:
// through BlueprintStore.AdvanceRunToStepSystem, which moves the
// blueprint_run's current_step_index, ends the step it moves past and writes
// the next step's conversations row in one transaction. next inherits prev's
// run and task and lands one index on, so a fixture walking a sequence just
// hands back what the last call returned.
func advancePgStep(t *testing.T, stores db.Stores, orgID string, prev *domain.Conversation, next domain.Conversation) *domain.Conversation {
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
	advanced, _, conv, err := stores.Blueprints.AdvanceRunToStepSystem(context.Background(), orgID, from, prev.ID, next)
	if err != nil {
		t.Fatalf("AdvanceRunToStepSystem: %v", err)
	}
	if !advanced || conv == nil {
		t.Fatalf("AdvanceRunToStepSystem from step %d = (advanced=%v, conv=%v), want the next step", from, advanced, conv != nil)
	}
	return conv
}
