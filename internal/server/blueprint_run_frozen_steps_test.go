package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestBlueprintRunGet_ProjectsFrozenStepsNotLive is the regression test for
// TFAC-313: GET /api/blueprint-runs/{id} must render the step list the run was
// minted with (its frozen step_plan), not the live blueprint_steps. The board
// card and run-detail rail derive their step pips from this endpoint's `steps`
// length, so growing the blueprint after the run was minted — the user's repro:
// cancel a 1-step run, then append a prompt to the blueprint's tail — must not
// retroactively grow a settled run's rendered step count.
func TestBlueprintRunGet_ProjectsFrozenStepsNotLive(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	// A 1-step blueprint: the auto-wrapped prompt is its sole live step.
	bpID, p0 := createWrappedBlueprint(t, s, "Frozen")

	// A task for the run to hang off (blueprint_runs.task_id is NOT NULL).
	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "owner/repo#frozen", "pr", "Frozen", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eid := entity.ID
	evtID, err := s.events.Record(ctx, runmode.LocalDefaultOrg, domain.Event{
		EntityID: &eid, EventType: domain.EventGitHubPRCICheckFailed,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := s.tasks.FindOrCreate(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, "", evtID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Mint the run with a length-1 plan frozen onto it, exactly as delegate.go
	// does at mint — snapshot the single resolved step. Cancelled mirrors the
	// repro (the run was cancelled before the blueprint was edited).
	const brID = "br-frozen"
	if _, err := sqlitestore.New(s.db).Blueprints.CreateRun(ctx, runmode.LocalDefaultOrg, domain.BlueprintRun{
		ID:           brID,
		BlueprintID:  bpID,
		TaskID:       task.ID,
		TriggerType:  domain.BlueprintTriggerManual,
		Status:       domain.BlueprintRunStatusCancelled,
		WorktreePath: "/tmp/wt-frozen",
		StepPlan: []domain.BlueprintPlanStep{
			{StepIndex: 0, PromptID: p0, PromptName: "Frozen", PromptBody: "do Frozen", Source: "user"},
		},
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// The mid-flight edit: append a second step to the LIVE blueprint, growing it
	// to 2 steps. Re-including p0 (owned by this blueprint) keeps it as step 0.
	p1 := createBarePrompt(t, s, "Appended")
	if put := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bpID+"/steps", map[string]any{
		"steps": []map[string]any{{"step_prompt_id": p0}, {"step_prompt_id": p1}},
	}); put.Code != http.StatusNoContent {
		t.Fatalf("grow live blueprint: %d: %s", put.Code, put.Body.String())
	}

	// Precondition: the live blueprint now reports 2 steps.
	live := doJSON(t, s, http.MethodGet, "/api/blueprints/"+bpID+"/steps", nil)
	var liveSteps []map[string]any
	_ = json.Unmarshal(live.Body.Bytes(), &liveSteps)
	if len(liveSteps) != 2 {
		t.Fatalf("live blueprint steps = %d, want 2 (precondition for the regression)", len(liveSteps))
	}

	// The run still projects its frozen length-1 plan — not the grown live list.
	rec := doJSON(t, s, http.MethodGet, "/api/blueprint-runs/"+brID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get blueprint run: %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Steps []struct {
			Step struct {
				StepIndex    int     `json:"step_index"`
				StepPromptID string  `json:"step_prompt_id"`
				CreatedAt    *string `json:"created_at"`
			} `json:"step"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Steps) != 1 {
		t.Fatalf("blueprint run steps = %d, want 1 (the frozen plan, not the grown live blueprint)", len(resp.Steps))
	}
	if resp.Steps[0].Step.StepPromptID != p0 {
		t.Errorf("frozen step prompt = %q, want %q", resp.Steps[0].Step.StepPromptID, p0)
	}
	// A step reconstructed from the frozen plan has no created_at to report, so
	// the field is omitted rather than serialized as a zero "0001-01-01"
	// timestamp (a *string stays nil on an absent key, but holds the sentinel if
	// a zero time leaks back in).
	if resp.Steps[0].Step.CreatedAt != nil {
		t.Errorf("frozen step created_at = %q, want omitted", *resp.Steps[0].Step.CreatedAt)
	}
}
