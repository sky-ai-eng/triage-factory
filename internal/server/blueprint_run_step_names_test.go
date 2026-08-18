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

// TestBlueprintRunGet_StepNamesFromFrozenPlan covers both projections of the
// step list on GET /api/blueprint-runs/{id}: a run with a frozen plan carries
// each step's prompt name (the chain indicator's label), and the defensive
// no-plan fallback — which reads live blueprint_steps, where no name exists —
// still returns the steps and simply omits the field.
func TestBlueprintRunGet_StepNamesFromFrozenPlan(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	bpID, p0 := createWrappedBlueprint(t, s, "Reproduce")
	p1 := createBarePrompt(t, s, "Patch")
	if put := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bpID+"/steps", map[string]any{
		"steps": []map[string]any{{"step_prompt_id": p0}, {"step_prompt_id": p1, "brief": "land the fix"}},
	}); put.Code != http.StatusNoContent {
		t.Fatalf("set blueprint steps: %d: %s", put.Code, put.Body.String())
	}

	taskID := seedBlueprintRunTask(t, s, "owner/repo#names")

	// The plan a delegation freezes onto the run at mint — prompt names and all.
	plannedID := fixtureUUID("br-named")
	if _, err := sqlitestore.New(s.db).Blueprints.CreateRun(ctx, runmode.LocalDefaultOrgID, domain.BlueprintRun{
		ID:           plannedID,
		BlueprintID:  bpID,
		TaskID:       taskID,
		TriggerType:  domain.BlueprintTriggerManual,
		Status:       domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-named",
		StepPlan: []domain.BlueprintPlanStep{
			{StepIndex: 0, PromptID: p0, PromptName: "Reproduce", PromptBody: "do Reproduce", Source: "user"},
			{StepIndex: 1, PromptID: p1, PromptName: "Patch", PromptBody: "b", Source: "user", Brief: "land the fix"},
		},
	}); err != nil {
		t.Fatalf("CreateRun (planned): %v", err)
	}

	steps := getBlueprintRunSteps(t, s, plannedID)
	if len(steps) != 2 {
		t.Fatalf("planned run steps = %d, want 2", len(steps))
	}
	for i, want := range []string{"Reproduce", "Patch"} {
		if steps[i].Step.Name == nil {
			t.Errorf("step %d name omitted, want %q", i, want)
			continue
		}
		if *steps[i].Step.Name != want {
			t.Errorf("step %d name = %q, want %q", i, *steps[i].Step.Name, want)
		}
	}
	// The name is additive: the brief still rides along as the secondary line.
	if steps[1].Step.Brief != "land the fix" {
		t.Errorf("step 1 brief = %q, want %q", steps[1].Step.Brief, "land the fix")
	}
	if steps[0].Step.StepPromptID != p0 || steps[1].Step.StepPromptID != p1 {
		t.Errorf("step prompt ids = %q/%q, want %q/%q",
			steps[0].Step.StepPromptID, steps[1].Step.StepPromptID, p0, p1)
	}

	// The defensive branch: a run with no frozen plan falls back to the live
	// steps, which carry a prompt id and no name.
	unplannedID := fixtureUUID("br-unnamed")
	if _, err := sqlitestore.New(s.db).Blueprints.CreateRun(ctx, runmode.LocalDefaultOrgID, domain.BlueprintRun{
		ID:           unplannedID,
		BlueprintID:  bpID,
		TaskID:       taskID,
		TriggerType:  domain.BlueprintTriggerManual,
		Status:       domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-unnamed",
	}); err != nil {
		t.Fatalf("CreateRun (unplanned): %v", err)
	}

	fallback := getBlueprintRunSteps(t, s, unplannedID)
	if len(fallback) != 2 {
		t.Fatalf("unplanned run steps = %d, want 2 (the live blueprint's steps)", len(fallback))
	}
	// omitempty means the key is absent, not present-and-empty (a *string stays
	// nil on an absent key, but holds "" if an empty name leaks into the JSON).
	for i, st := range fallback {
		if st.Step.Name != nil {
			t.Errorf("fallback step %d name = %q, want omitted", i, *st.Step.Name)
		}
	}
}

// blueprintRunStepJSON is the decode target for the run projection's step list —
// only the fields these assertions read. Name is a *string so an omitted key is
// distinguishable from one present but empty: the fallback path's contract is
// that the key is absent, and a plain string would read "" either way.
type blueprintRunStepJSON struct {
	Step struct {
		StepIndex    int     `json:"step_index"`
		StepPromptID string  `json:"step_prompt_id"`
		Name         *string `json:"name"`
		Brief        string  `json:"brief"`
	} `json:"step"`
}

func getBlueprintRunSteps(t *testing.T, s *Server, blueprintRunID string) []blueprintRunStepJSON {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, "/api/blueprint-runs/"+blueprintRunID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get blueprint run %s: %d: %s", blueprintRunID, rec.Code, rec.Body.String())
	}
	var resp struct {
		Steps []blueprintRunStepJSON `json:"steps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode blueprint run %s: %v", blueprintRunID, err)
	}
	return resp.Steps
}

// seedBlueprintRunTask mints the entity/event/task a blueprint run hangs off
// (blueprint_runs.task_id is NOT NULL).
func seedBlueprintRunTask(t *testing.T, s *Server, sourceID string) string {
	t.Helper()
	ctx := context.Background()
	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", sourceID, "pr", sourceID, "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eid := entity.ID
	evtID, err := s.events.Record(ctx, runmode.LocalDefaultOrgID, domain.Event{
		EntityID: &eid, EventType: domain.EventGitHubPRCICheckFailed,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := s.tasks.FindOrCreate(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, "", evtID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task.ID
}
