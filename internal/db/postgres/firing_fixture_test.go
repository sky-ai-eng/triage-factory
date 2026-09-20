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
	return tryFirePgRun(t, h, stores, orgID, domain.BlueprintRun{BlueprintID: bpID, TaskID: taskID}, step)
}

// firePgRun fires a run the fixture composed itself — the variant for tests
// that name the run's own columns (its id, its frozen plan, its actor) rather
// than only the step riding on it. Anything br leaves empty is defaulted; the
// step's run, task and index are filled in from br.
func firePgRun(t *testing.T, h *pgtest.Harness, stores db.Stores, orgID string, br domain.BlueprintRun, step domain.Conversation) *domain.Conversation {
	t.Helper()
	if step.BlueprintStepIndex == nil {
		step0 := 0
		step.BlueprintStepIndex = &step0
	}
	conv, err := tryFirePgRun(t, h, stores, orgID, br, step)
	if err != nil {
		t.Fatalf("CreateRunWithFirstStepSystem: %v", err)
	}
	if conv == nil {
		t.Fatal("CreateRunWithFirstStepSystem returned no conversation for a fresh firing")
	}
	return conv
}

// tryFirePgRun is the one place this package's fixtures reach the firing door.
// Everything above it composes a run and a step and lands here.
func tryFirePgRun(t *testing.T, h *pgtest.Harness, stores db.Stores, orgID string, br domain.BlueprintRun, step domain.Conversation) (*domain.Conversation, error) {
	t.Helper()
	if _, err := h.AdminDB.Exec(
		`UPDATE blueprint_runs SET status = 'completed' WHERE org_id = $1 AND task_id = $2 AND status = 'running'`,
		orgID, br.TaskID,
	); err != nil {
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
		context.Background(), orgID, br, db.AgentClaimStamp{}, "", step)
	return conv, err
}

// insertPgBlueprintRun inserts a blueprint_runs row directly — the fixture
// stand-in for the mint inside a BlueprintStore door, staging a run in
// arbitrary shape without staging a whole firing for each. Production writes
// this table through CreateRunWithFirstStepSystem alone, which commits a first
// step with it; a fixture whose subject is the run row itself, or one that
// stages its step conversations by hand beside it, wants neither that step nor
// the doorbell the door rings for it.
//
// creatorUserID pairs with the trigger type the way
// blueprint_runs_creator_matches_trigger_type requires: an event-fired run has
// no human author and takes NULL whatever is passed, a manual one must name
// one. Fields honored: ID (minted when empty), BlueprintID, TaskID,
// TriggerType (default manual), TriggerID, ActorAgentID, Status (default
// running), StepPlan, WorktreePath. Returns the run id.
//
// One running blueprint_run per task is a schema invariant, so the task's
// prior run is settled first — which is what a task moving on to its next
// engagement means.
func insertPgBlueprintRun(t *testing.T, h *pgtest.Harness, orgID, creatorUserID string, br domain.BlueprintRun) string {
	t.Helper()
	if br.ID == "" {
		br.ID = uuid.New().String()
	}
	if br.TriggerType == "" {
		br.TriggerType = domain.BlueprintTriggerManual
	}
	if br.Status == "" {
		br.Status = domain.BlueprintRunStatusRunning
	}
	var creator any
	if br.TriggerType != domain.BlueprintTriggerEvent && creatorUserID != "" {
		creator = creatorUserID
	}
	stepPlan, err := domain.MarshalStepPlan(br.StepPlan)
	if err != nil {
		t.Fatalf("marshal step plan: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`UPDATE blueprint_runs SET status = 'completed' WHERE org_id = $1 AND task_id = $2 AND status = 'running'`,
		orgID, br.TaskID,
	); err != nil {
		t.Fatalf("settle the task's prior blueprint_run: %v", err)
	}
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type,
		                            trigger_id, actor_agent_id, status, step_plan, worktree_path, started_at)
		VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, $8::uuid, $9, $10, $11, now())
	`, br.ID, orgID, creator, br.BlueprintID, br.TaskID, br.TriggerType,
		nullIfEmptyPgTest(br.TriggerID), nullIfEmptyPgTest(br.ActorAgentID), br.Status, stepPlan, br.WorktreePath)
	return br.ID
}

// nullIfEmptyPgTest binds "" as SQL NULL, which is what a nullable column
// holding "no value" carries.
func nullIfEmptyPgTest(s string) any {
	if s == "" {
		return nil
	}
	return s
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
