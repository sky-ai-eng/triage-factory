package delegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
)

// TestBlueprintRun_StepPlanFrozenAgainstMidFlightEdit is the acceptance test for
// freezing the step plan at mint: a blueprint edited between step 0 finishing
// and step 1 claiming must not change what step 1 runs. The run executes the
// plan snapshotted onto blueprint_runs at mint, so a prompt-body edit and a
// structural ReplaceSteps (here dropping step 1 entirely) are both forward-only
// — they govern the next firing, never this in-flight one.
//
// It asserts the freeze at all three points the dispatcher path touches: the
// stored plan (GetRun), advancement (reactToStepTerminal enqueues the original
// step from the frozen length, not the shortened live list), and execution (the
// SKILL.md the next claim would materialize carries the original prompt body).
func TestBlueprintRun_StepPlanFrozenAgainstMidFlightEdit(t *testing.T) {
	database := newDelegateTestDB(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	stores := sqlitestore.New(database)

	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "owner/repo#freeze", "pr", "T", "https://x/freeze")
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	eventID, err := stores.Events.Record(ctx, org, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := stores.Tasks.FindOrCreate(ctx, org, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, "freeze", eventID, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}

	// 2-step blueprint with distinct, recognizable prompt bodies.
	bpID := "freeze-bp"
	if err := stores.Blueprints.Create(ctx, org, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: bpID, Name: bpID, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("blueprint: %v", err)
	}
	ensureTestPrompt(t, database, domain.Prompt{ID: "freeze-p0", Name: "Step Zero", Body: "orig-0 body", Source: "user"})
	ensureTestPrompt(t, database, domain.Prompt{ID: "freeze-p1", Name: "Step One", Body: "orig-1 body", Source: "user"})
	if err := stores.Blueprints.ReplaceSteps(ctx, org, bpID, []string{"freeze-p0", "freeze-p1"}, []string{"brief-0", "brief-1"}); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}

	// Mint the run with the resolved plan frozen on it, exactly as delegate.go
	// does at mint (resolve each step's prompt once, snapshot its content).
	steps, err := stores.Blueprints.ListStepsSystem(ctx, org, bpID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	plan := make([]domain.BlueprintPlanStep, len(steps))
	for i, st := range steps {
		p, err := stores.Prompts.GetSystem(ctx, org, st.StepPromptID)
		if err != nil || p == nil {
			t.Fatalf("resolve step %d prompt: %v", i, err)
		}
		plan[i] = domain.BlueprintPlanStep{
			StepIndex: st.StepIndex, PromptID: p.ID, PromptName: p.Name, PromptBody: p.Body,
			Source: p.Source, AllowedTools: p.AllowedTools, Model: p.Model, Brief: st.Brief,
		}
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "freeze-bpr", BlueprintID: bpID, TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-freeze", StepPlan: plan,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Step 0 finished with continue → ready to advance to step 1.
	step0 := 0
	run0 := "freeze-run0"
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID: run0, TaskID: task.ID, PromptID: "freeze-p0", Status: "completed",
		Model: "claude-sonnet-4-6", Outcome: "continue",
		BlueprintRunID: brID, BlueprintStepIndex: &step0,
	})

	// --- The mid-flight edits: a prompt-body edit AND a structural ReplaceSteps
	// that drops step 1 entirely. If the run read its program live, either would
	// change (or erase) what step 1 executes.
	if _, err := database.Exec(`UPDATE prompts SET body = 'edited-1 body' WHERE id = 'freeze-p1'`); err != nil {
		t.Fatalf("edit prompt body: %v", err)
	}
	if err := stores.Blueprints.ReplaceSteps(ctx, org, bpID, []string{"freeze-p0"}, []string{"brief-0"}); err != nil {
		t.Fatalf("ReplaceSteps (drop step 1): %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	// (1) The stored plan is untouched: still two steps, original step-1 body.
	br := mustGetRun(t, s, org, brID)
	if len(br.StepPlan) != 2 {
		t.Fatalf("frozen plan length = %d, want 2 (structural edit must not shrink it)", len(br.StepPlan))
	}
	if got := br.StepPlan[1].PromptBody; got != "orig-1 body" {
		t.Fatalf("frozen step-1 body = %q, want %q (prompt-body edit leaked in)", got, "orig-1 body")
	}

	// (2) Advancement enqueues step 1 with the ORIGINAL prompt, even though the
	// live blueprint now has only one step (which would make step 0 final).
	stepRun, _ := s.agentRuns.GetSystem(ctx, org, run0)
	stepRun.TriggerType = "manual"
	stepRun.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(org, br, *stepRun, runConfig{orgID: org}, time.Now())

	if br2 := mustGetRun(t, s, org, brID); br2.Status != domain.BlueprintRunStatusRunning {
		t.Fatalf("blueprint status = %q, want running (must advance off the frozen plan, not finish on the shortened live list)", br2.Status)
	}
	var (
		nextPromptID string
		nextIdx      int
	)
	if err := database.QueryRow(
		`SELECT prompt_id, blueprint_step_index FROM conversations WHERE blueprint_run_id = ? AND status IS NULL`, brID,
	).Scan(&nextPromptID, &nextIdx); err != nil {
		t.Fatalf("read queued step-1 run: %v", err)
	}
	if nextIdx != 1 || nextPromptID != "freeze-p1" {
		t.Fatalf("enqueued step = (idx %d, prompt %q), want (1, freeze-p1) from the frozen plan", nextIdx, nextPromptID)
	}

	// (3) Execution: materialize step 1's skill from the frozen plan exactly as
	// the dispatcher will on the next claim. The SKILL.md must carry the ORIGINAL
	// body — this is the "step 1 executed the original prompt body" assertion.
	wt := t.TempDir()
	planStep := br.StepPlan[1]
	slug := skills.SlugForBlueprintStep(planStep.StepIndex, planStep.PromptName)
	if err := skills.MaterializeStepSkill(wt, slug, planStep.Prompt(), planStep.Brief); err != nil {
		t.Fatalf("MaterializeStepSkill: %v", err)
	}
	skillMD, err := os.ReadFile(filepath.Join(wt, ".claude", "skills", slug, "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	if !strings.Contains(string(skillMD), "orig-1 body") {
		t.Errorf("step 1 SKILL.md missing the original body; got:\n%s", skillMD)
	}
	if strings.Contains(string(skillMD), "edited-1 body") {
		t.Errorf("step 1 SKILL.md leaked the mid-flight prompt-body edit; got:\n%s", skillMD)
	}
}
