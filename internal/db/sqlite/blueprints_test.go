package sqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestBlueprintStore_SQLite_Conformance runs the shared blueprint
// seed-idempotency + step round-trip suite against the SQLite impl.
func TestBlueprintStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunBlueprintStoreConformance(t, func(t *testing.T) (db.BlueprintStore, string, string, dbtest.PromptSeederForBlueprints) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seedPrompt := func(t *testing.T, idHint string) string {
			t.Helper()
			id := idHint + "-" + uuid.New().String()[:8]
			insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: id, Name: id, Body: "x", Source: "user"})
			return id
		}
		return stores.Blueprints, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, seedPrompt
	})
}

// insertPromptForBlueprintTest seeds a prompt row directly. PromptStore.Create
// exists but takes the full create-shape; for FK-only seeding we want a
// minimal raw INSERT, matching the pattern other sqlite_test files use
// when seeding tables they don't own.
func insertPromptForBlueprintTest(t *testing.T, conn *sql.DB, p domain.Prompt) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := conn.Exec(`
		INSERT INTO prompts (id, name, body, source, allowed_tools, usage_count, team_id, creator_user_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, '[]', 0, ?, ?, ?, ?)
	`, p.ID, p.Name, p.Body, p.Source, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, now, now); err != nil {
		t.Fatalf("seed prompt %s: %v", p.ID, err)
	}
}

// insertBlueprintForTest creates a user-source blueprint row owned by the
// local default team so blueprint_runs / blueprint_steps FKs resolve. The
// blueprint id is the supplied id (tests reference it directly).
func insertBlueprintForTest(t *testing.T, conn *sql.DB, id, name string) {
	t.Helper()
	if err := sqlitestore.New(conn).Blueprints.Create(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: id, Name: name, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint %s: %v", id, err)
	}
}

// seedEntityEventTask builds the entity → event → task FK chain that
// every blueprint test needs. Returns the task so tests can reference its
// ID. suffix scopes the seeded source_id so subtests don't collide.
func seedEntityEventTask(t *testing.T, conn *sql.DB, suffix string) *domain.Task {
	t.Helper()
	entity, _, err := sqlitestore.New(conn).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github",
		"owner/repo#"+suffix, "pr", "Blueprint Test "+suffix, "https://example.com/"+suffix)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := sqlitestore.New(conn).Events.Record(context.Background(), runmode.LocalDefaultOrg, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := sqlitestore.New(conn).Tasks.FindOrCreate(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, entity.ID,
		domain.EventGitHubPRCICheckFailed, suffix, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// TestBlueprintStore_SQLite_RunsForBlueprint_RoundTrip protects the 16-column
// SELECT/Scan pair in RunsForBlueprint against silent column-order drift.
func TestBlueprintStore_SQLite_RunsForBlueprint_RoundTrip(t *testing.T) {
	conn := openSQLiteForTest(t)
	blueprints := sqlitestore.New(conn).Blueprints
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "blueprint-rt")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "step-prompt-1", Name: "Step 1", Body: "do step 1", Source: "user"})
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "step-prompt-2", Name: "Step 2", Body: "do step 2", Source: "user"})
	insertBlueprintForTest(t, conn, "blueprint-1", "My Blueprint")

	if err := blueprints.ReplaceSteps(ctx, org, "blueprint-1", []string{"step-prompt-1", "step-prompt-2"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}

	blueprintRunID, err := blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID:           "blueprint-run-rt",
		BlueprintID:  "blueprint-1",
		TaskID:       task.ID,
		TriggerType:  domain.BlueprintTriggerManual,
		Status:       domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-blueprint-rt",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if blueprintRunID != "blueprint-run-rt" {
		t.Fatalf("unexpected blueprint run id: %s", blueprintRunID)
	}

	step0 := 0
	step1 := 1
	for _, run := range []domain.AgentRun{
		{ID: "blueprint-step-run-0", TaskID: task.ID, PromptID: "step-prompt-1", Status: "initializing", Model: "claude-sonnet-4-6", BlueprintRunID: "blueprint-run-rt", BlueprintStepIndex: &step0},
		{ID: "blueprint-step-run-1", TaskID: task.ID, PromptID: "step-prompt-2", Status: "initializing", Model: "claude-sonnet-4-6", BlueprintRunID: "blueprint-run-rt", BlueprintStepIndex: &step1},
	} {
		if err := sqlitestore.New(conn).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrg, run); err != nil {
			t.Fatalf("create agent run %s: %v", run.ID, err)
		}
	}

	runs, err := blueprints.RunsForBlueprint(ctx, org, "blueprint-run-rt")
	if err != nil {
		t.Fatalf("RunsForBlueprint: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].ID != "blueprint-step-run-0" || runs[1].ID != "blueprint-step-run-1" {
		t.Errorf("unexpected order: %v", []string{runs[0].ID, runs[1].ID})
	}
	if runs[0].BlueprintStepIndex == nil || *runs[0].BlueprintStepIndex != 0 {
		t.Errorf("run[0].BlueprintStepIndex = %v, want 0", runs[0].BlueprintStepIndex)
	}
	if runs[1].BlueprintStepIndex == nil || *runs[1].BlueprintStepIndex != 1 {
		t.Errorf("run[1].BlueprintStepIndex = %v, want 1", runs[1].BlueprintStepIndex)
	}
	if runs[0].PromptID != "step-prompt-1" {
		t.Errorf("run[0].PromptID = %q, want step-prompt-1", runs[0].PromptID)
	}
	if runs[0].Model != "claude-sonnet-4-6" {
		t.Errorf("run[0].Model = %q, want claude-sonnet-4-6", runs[0].Model)
	}
}

// TestBlueprintStore_SQLite_RunsForBlueprint_SurfacesOutcome pins the channel
// that replaced the old per-step verdict: a step run's terminal runs.outcome
// (and outcome_reason) round-trips through RunsForBlueprint, which is what the
// run-detail handler renders and what the orchestrator advances on.
func TestBlueprintStore_SQLite_RunsForBlueprint_SurfacesOutcome(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	blueprints := stores.Blueprints
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "blueprint-outcome")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "op-step", Name: "OP Step", Body: "x", Source: "user"})
	insertBlueprintForTest(t, conn, "op-blueprint", "OP Blueprint")
	if err := blueprints.ReplaceSteps(ctx, org, "op-blueprint", []string{"op-step"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	if _, err := blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "op-blueprint-run", BlueprintID: "op-blueprint", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	step0 := 0
	if err := stores.AgentRuns.Create(ctx, org, domain.AgentRun{
		ID: "op-run", TaskID: task.ID, PromptID: "op-step", Status: "initializing",
		Model: "claude-sonnet-4-6", BlueprintRunID: "op-blueprint-run", BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("create agent run: %v", err)
	}
	// Persist a terminal outcome the way processCompletion does.
	if err := stores.AgentRuns.CompleteSystem(ctx, org, "op-run", "completed", 0, 0, 0, "", "did the thing", "continue", ""); err != nil {
		t.Fatalf("complete step run: %v", err)
	}

	runs, err := blueprints.RunsForBlueprint(ctx, org, "op-blueprint-run")
	if err != nil {
		t.Fatalf("RunsForBlueprint: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("RunsForBlueprint returned %d runs, want 1", len(runs))
	}
	if runs[0].Outcome != "continue" {
		t.Errorf("step run outcome = %q, want continue (the orchestrator advances on this)", runs[0].Outcome)
	}
}

// TestBlueprintStore_SQLite_MarkRunStatus_Guarded pins the lost-update
// race guard: only non-terminal statuses accept a transition.
func TestBlueprintStore_SQLite_MarkRunStatus_Guarded(t *testing.T) {
	conn := openSQLiteForTest(t)
	blueprints := sqlitestore.New(conn).Blueprints
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "blueprint-guard")
	insertBlueprintForTest(t, conn, "guard-blueprint", "Guard Blueprint")

	blueprintRunID, err := blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "blueprint-run-guard", BlueprintID: "guard-blueprint", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	changed, err := blueprints.MarkRunStatus(ctx, org, blueprintRunID, domain.BlueprintRunStatusCompleted, "", nil)
	if err != nil {
		t.Fatalf("MarkRunStatus: %v", err)
	}
	if !changed {
		t.Error("expected changed=true for running → completed")
	}

	changed2, err := blueprints.MarkRunStatus(ctx, org, blueprintRunID, domain.BlueprintRunStatusAborted, "late abort", nil)
	if err != nil {
		t.Fatalf("MarkRunStatus second: %v", err)
	}
	if changed2 {
		t.Error("expected changed=false when blueprint run already terminal")
	}

	cr, err := blueprints.GetRun(ctx, org, blueprintRunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if cr.Status != domain.BlueprintRunStatusCompleted {
		t.Errorf("status = %q, want completed", cr.Status)
	}
}

// TestBlueprintStore_SQLite_CreateRun_RequiresTriggerType verifies the
// upfront validation: empty TriggerType errors rather than silently
// defaulting.
func TestBlueprintStore_SQLite_CreateRun_RequiresTriggerType(t *testing.T) {
	conn := openSQLiteForTest(t)
	blueprints := sqlitestore.New(conn).Blueprints
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "ttype")
	insertBlueprintForTest(t, conn, "ttype-blueprint", "T")

	if _, err := blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "ttype-run", BlueprintID: "ttype-blueprint", TaskID: task.ID,
		TriggerType: "", Status: domain.BlueprintRunStatusRunning,
	}); err == nil {
		t.Error("expected error for empty TriggerType, got nil")
	}
}

// TestBlueprintStore_SQLite_Delete_WithRunHistory_SoftDeletes pins the fix for
// the delete-with-runs case: a blueprint that has already run must still be
// deletable. Delete is soft (stamps deleted_at) so the run history — the
// durable audit trail, including externally-visible artifacts — survives.
func TestBlueprintStore_SQLite_Delete_WithRunHistory_SoftDeletes(t *testing.T) {
	conn := openSQLiteForTest(t)
	blueprints := sqlitestore.New(conn).Blueprints
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "del-runs")
	insertBlueprintForTest(t, conn, "del-bp", "Doomed")
	if _, err := blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "del-bp-run", BlueprintID: "del-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// The whole point: this must NOT error despite the blueprint_runs RESTRICT FK.
	if err := blueprints.Delete(ctx, org, "del-bp"); err != nil {
		t.Fatalf("Delete with run history errored (%v); it should soft-delete", err)
	}

	// Request-facing Get filters it; GetSystem still sees it (in-flight runs);
	// the run row itself is untouched (audit trail preserved).
	if got, err := blueprints.Get(ctx, org, "del-bp"); err != nil || got != nil {
		t.Errorf("Get after soft-delete = (%+v, %v); want (nil, nil)", got, err)
	}
	if sys, err := blueprints.GetSystem(ctx, org, "del-bp"); err != nil || sys == nil {
		t.Errorf("GetSystem after soft-delete = (%+v, %v); want the row preserved", sys, err)
	}
	if run, err := blueprints.GetRun(ctx, org, "del-bp-run"); err != nil || run == nil {
		t.Errorf("blueprint_run after blueprint delete = (%+v, %v); audit history must survive", run, err)
	}
}

// TestBlueprintStore_SQLite_AssertsLocalOrg pins the local-org guard: any
// orgID other than runmode.LocalDefaultOrg must fail loudly.
func TestBlueprintStore_SQLite_AssertsLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	blueprints := sqlitestore.New(conn).Blueprints
	if _, err := blueprints.ListSteps(context.Background(), "some-real-uuid", "anything"); err == nil {
		t.Fatal("ListSteps accepted non-local orgID; should reject")
	}
}
