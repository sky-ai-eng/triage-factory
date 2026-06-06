package delegate

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// reactorFixture stands up a spawner + an N-step blueprint with a running
// blueprint_run on a fresh task, and inserts the step-0 run as the just-finished
// step (status + outcome set by the caller). Returns the spawner, db,
// blueprint_run id, task id, and the step-0 run id.
func reactorFixture(t *testing.T, suffix string, nSteps int, step0Status, step0Outcome string) (*Spawner, *sql.DB, string, string, string) {
	t.Helper()
	database := newTakeoverTestDB(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg
	stores := sqlitestore.New(database)

	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "owner/repo#"+suffix, "pr", "T", "https://x/"+suffix)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	eventID, err := stores.Events.Record(ctx, org, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := stores.Tasks.FindOrCreate(ctx, org, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, suffix, eventID, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}

	bpID := "rbp-" + suffix
	if err := stores.Blueprints.Create(ctx, org, runmode.LocalDefaultTeamID, domain.Blueprint{ID: bpID, Name: bpID, Source: "user", TeamID: runmode.LocalDefaultTeamID}); err != nil {
		t.Fatalf("blueprint: %v", err)
	}
	promptIDs := make([]string, nSteps)
	for i := 0; i < nSteps; i++ {
		pid := fmt.Sprintf("%s-p%d", suffix, i)
		ensureTestPrompt(t, database, domain.Prompt{ID: pid, Name: pid, Body: "b", Source: "user"})
		promptIDs[i] = pid
	}
	if err := stores.Blueprints.ReplaceSteps(ctx, org, bpID, promptIDs, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	// Freeze the plan onto the run exactly as the mint path does — the reactor
	// now sequences off br.StepPlan, so an empty plan would fail the advance.
	plan := make([]domain.BlueprintPlanStep, nSteps)
	for i := 0; i < nSteps; i++ {
		plan[i] = domain.BlueprintPlanStep{
			StepIndex: i, PromptID: promptIDs[i], PromptName: promptIDs[i],
			PromptBody: "b", Source: "user",
		}
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rbpr-" + suffix, BlueprintID: bpID, TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-" + suffix, StepPlan: plan,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	step0 := 0
	run0 := "rrun0-" + suffix
	if err := stores.AgentRuns.Create(ctx, org, domain.AgentRun{
		ID: run0, TaskID: task.ID, PromptID: promptIDs[0], Status: step0Status,
		Model: "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("create step0 run: %v", err)
	}
	if _, err := database.Exec(`UPDATE runs SET status = ?, outcome = ? WHERE id = ?`,
		step0Status, sql.NullString{String: step0Outcome, Valid: step0Outcome != ""}, run0); err != nil {
		t.Fatalf("set step0 outcome: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")
	return s, database, brID, task.ID, run0
}

func queuedStepRuns(t *testing.T, database *sql.DB, brID string) []int {
	t.Helper()
	rows, err := database.Query(`SELECT blueprint_step_index FROM runs WHERE blueprint_run_id = ? AND status = 'queued' ORDER BY blueprint_step_index`, brID)
	if err != nil {
		t.Fatalf("query queued runs: %v", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, idx)
	}
	return out
}

// TestReactor_AdvanceEnqueuesNextStep: a non-final step that completes with
// 'continue' enqueues the next step and bumps current_step_index, leaving the
// blueprint running.
func TestReactor_AdvanceEnqueuesNextStep(t *testing.T) {
	s, database, brID, _, run0 := reactorFixture(t, "adv", 2, "completed", "continue")
	org := runmode.LocalDefaultOrg

	stepRun, _ := s.agentRuns.GetSystem(context.Background(), org, run0)
	stepRun.TriggerType = "manual"
	stepRun.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(org, mustGetRun(t, s, org, brID), *stepRun, runConfig{orgID: org}, time.Now())

	if q := queuedStepRuns(t, database, brID); len(q) != 1 || q[0] != 1 {
		t.Fatalf("queued step runs = %v, want [1]", q)
	}
	br := mustGetRun(t, s, org, brID)
	if br.Status != domain.BlueprintRunStatusRunning {
		t.Errorf("blueprint status = %q, want running", br.Status)
	}
	if br.CurrentStepIndex != 1 {
		t.Errorf("current_step_index = %d, want 1", br.CurrentStepIndex)
	}
}

// TestReactor_FinalStepFinishCompletes: the final step completing with 'finish'
// terminates the blueprint completed and closes the task.
func TestReactor_FinalStepFinishCompletes(t *testing.T) {
	s, database, brID, taskID, run0 := reactorFixture(t, "fin", 1, "completed", "finish")
	org := runmode.LocalDefaultOrg

	stepRun, _ := s.agentRuns.GetSystem(context.Background(), org, run0)
	stepRun.TriggerType = "manual"
	stepRun.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(org, mustGetRun(t, s, org, brID), *stepRun, runConfig{orgID: org}, time.Now())

	if q := queuedStepRuns(t, database, brID); len(q) != 0 {
		t.Fatalf("queued step runs = %v, want none", q)
	}
	br := mustGetRun(t, s, org, brID)
	if br.Status != domain.BlueprintRunStatusCompleted {
		t.Errorf("blueprint status = %q, want completed", br.Status)
	}
	if got := readTaskStatus(t, database, taskID); got != "done" {
		t.Errorf("task status = %q, want done", got)
	}
}

// TestReactor_CancelRequestedTerminates: a continue outcome on a cancel-requested
// blueprint does NOT advance — it finalizes the blueprint cancelled.
func TestReactor_CancelRequestedTerminates(t *testing.T) {
	s, database, brID, _, run0 := reactorFixture(t, "can", 2, "completed", "continue")
	org := runmode.LocalDefaultOrg
	if _, err := s.blueprints.RequestRunCancelSystem(context.Background(), org, brID); err != nil {
		t.Fatalf("RequestRunCancelSystem: %v", err)
	}

	stepRun, _ := s.agentRuns.GetSystem(context.Background(), org, run0)
	stepRun.TriggerType = "manual"
	stepRun.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(org, mustGetRun(t, s, org, brID), *stepRun, runConfig{orgID: org}, time.Now())

	if q := queuedStepRuns(t, database, brID); len(q) != 0 {
		t.Fatalf("queued step runs = %v, want none (cancel must not advance)", q)
	}
	br := mustGetRun(t, s, org, brID)
	if br.Status != domain.BlueprintRunStatusCancelled {
		t.Errorf("blueprint status = %q, want cancelled", br.Status)
	}
}

// TestReactor_ParkedStepLeavesRunning: a step parked in awaiting_input leaves the
// blueprint running and enqueues nothing (resume drives it later).
func TestReactor_ParkedStepLeavesRunning(t *testing.T) {
	s, database, brID, _, run0 := reactorFixture(t, "park", 2, "awaiting_input", "")
	org := runmode.LocalDefaultOrg

	stepRun, _ := s.agentRuns.GetSystem(context.Background(), org, run0)
	stepRun.TriggerType = "manual"
	stepRun.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(org, mustGetRun(t, s, org, brID), *stepRun, runConfig{orgID: org}, time.Now())

	if q := queuedStepRuns(t, database, brID); len(q) != 0 {
		t.Fatalf("queued step runs = %v, want none (parked step waits for resume)", q)
	}
	if br := mustGetRun(t, s, org, brID); br.Status != domain.BlueprintRunStatusRunning {
		t.Errorf("blueprint status = %q, want running (parked)", br.Status)
	}
}

func mustGetRun(t *testing.T, s *Spawner, org, brID string) *domain.BlueprintRun {
	t.Helper()
	br, err := s.blueprints.GetRunSystem(context.Background(), org, brID)
	if err != nil || br == nil {
		t.Fatalf("GetRunSystem: (%v, %v)", br, err)
	}
	return br
}
