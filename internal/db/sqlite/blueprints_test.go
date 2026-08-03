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
		return stores.Blueprints, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, seedPrompt
	})
}

// TestBlueprintStore_SQLite_DuplicationConformance runs the shared
// DuplicatePrompts deep-copy suite against the SQLite impl. User prompts seed
// via the store; system prompts (source='system', creator NULL, a system_slug)
// go through a raw INSERT since PromptStore no longer carries a shipped seeder.
func TestBlueprintStore_SQLite_DuplicationConformance(t *testing.T) {
	dbtest.RunBlueprintDuplicationConformance(t, func(t *testing.T) (db.BlueprintStore, string, string, dbtest.DuplicationPromptSeeder, dbtest.DuplicationPromptGetter) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		ctx := context.Background()
		org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
		seed := func(t *testing.T, p domain.Prompt) string {
			t.Helper()
			if p.Source == "system" {
				return insertSystemPromptForTest(t, conn, p)
			}
			if p.ID == "" {
				p.ID = uuid.New().String()
			}
			if err := stores.Prompts.Create(ctx, org, team, p); err != nil {
				t.Fatalf("create prompt: %v", err)
			}
			return p.ID
		}
		getPrompt := func(t *testing.T, id string) domain.Prompt {
			t.Helper()
			pr, err := stores.Prompts.GetSystem(ctx, org, id)
			if err != nil || pr == nil {
				t.Fatalf("getPrompt %s: (%v, %v)", id, pr, err)
			}
			return *pr
		}
		return stores.Blueprints, org, team, seed, getPrompt
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

// insertSystemPromptForTest seeds a shipped-shape prompt row directly:
// source='system', creator_user_id NULL, and the given system_slug — the shape
// PromptStore.Create can't produce (it forces a non-NULL creator). Returns the id.
func insertSystemPromptForTest(t *testing.T, conn *sql.DB, p domain.Prompt) string {
	t.Helper()
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	if _, err := conn.Exec(`
		INSERT INTO prompts (id, name, body, source, allowed_tools, model, usage_count, user_modified, team_id, system_slug, creator_user_id, created_at, updated_at)
		VALUES (?, ?, ?, 'system', ?, ?, 0, 0, ?, ?, NULL, ?, ?)
	`, p.ID, p.Name, p.Body, p.AllowedTools, p.Model, runmode.LocalDefaultTeamID, p.SystemSlug, now, now); err != nil {
		t.Fatalf("seed system prompt %s: %v", p.ID, err)
	}
	return p.ID
}

// insertBlueprintForTest creates a user-source blueprint row owned by the
// local default team so blueprint_runs / blueprint_steps FKs resolve. The
// blueprint id is the supplied id (tests reference it directly).
func insertBlueprintForTest(t *testing.T, conn *sql.DB, id, name string) {
	t.Helper()
	if err := sqlitestore.New(conn).Blueprints.Create(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
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
	eventID, err := sqlitestore.New(conn).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := sqlitestore.New(conn).Tasks.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID,
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
	org := runmode.LocalDefaultOrgID

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
	for _, run := range []domain.Conversation{
		{ID: "blueprint-step-run-0", TaskID: task.ID, PromptID: "step-prompt-1", Status: "running", Model: "claude-sonnet-4-6", BlueprintRunID: "blueprint-run-rt", BlueprintStepIndex: &step0},
		{ID: "blueprint-step-run-1", TaskID: task.ID, PromptID: "step-prompt-2", Status: "running", Model: "claude-sonnet-4-6", BlueprintRunID: "blueprint-run-rt", BlueprintStepIndex: &step1},
	} {
		insertConversationForTest(t, conn, run)
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

// TestBlueprintStore_SQLite_StepPlanRoundTrip pins the frozen step plan column:
// the plan serialized at CreateRun deserializes field-faithfully on GetRun
// (full prompt content, not just ids), and an empty plan round-trips as empty
// rather than tripping the NOT NULL column.
func TestBlueprintStore_SQLite_StepPlanRoundTrip(t *testing.T) {
	conn := openSQLiteForTest(t)
	blueprints := sqlitestore.New(conn).Blueprints
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "stepplan-rt")
	insertBlueprintForTest(t, conn, "sp-bp", "Plan BP")

	plan := []domain.BlueprintPlanStep{
		{StepIndex: 0, PromptID: "sp-p0", PromptName: "Map", PromptBody: "map the surface", Source: "user", AllowedTools: "Bash,Read", Model: "opus", Brief: "brief-0"},
		{StepIndex: 1, PromptID: "sp-p1", PromptName: "Write", PromptBody: "write the review", Source: "imported", Brief: "brief-1"},
	}
	if _, err := blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "sp-bpr", BlueprintID: "sp-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-sp", StepPlan: plan,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got, err := blueprints.GetRun(ctx, org, "sp-bpr")
	if err != nil || got == nil {
		t.Fatalf("GetRun = (%v, %v)", got, err)
	}
	if len(got.StepPlan) != len(plan) {
		t.Fatalf("round-tripped plan length = %d, want %d", len(got.StepPlan), len(plan))
	}
	for i := range plan {
		if got.StepPlan[i] != plan[i] {
			t.Errorf("step %d round-trip = %+v, want %+v", i, got.StepPlan[i], plan[i])
		}
	}

	// An empty plan must satisfy NOT NULL and read back empty, not error.
	if _, err := blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "sp-bpr-empty", BlueprintID: "sp-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-sp-empty",
	}); err != nil {
		t.Fatalf("CreateRun (empty plan): %v", err)
	}
	empty, err := blueprints.GetRun(ctx, org, "sp-bpr-empty")
	if err != nil || empty == nil {
		t.Fatalf("GetRun (empty) = (%v, %v)", empty, err)
	}
	if len(empty.StepPlan) != 0 {
		t.Errorf("empty plan round-trip = %+v, want empty", empty.StepPlan)
	}
}

// TestBlueprintStore_SQLite_ActorAgentRoundTrip pins that the executing-bot
// actor freezes on the blueprint_run at CreateRun and reads back on GetRun (the
// reactor relies on this to inherit it onto each step run). The fenced event
// insert carries it too, and an empty actor round-trips as empty.
func TestBlueprintStore_SQLite_ActorAgentRoundTrip(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	agentID, err := stores.Agents.Create(ctx, org, domain.Agent{DisplayName: "Bot"})
	if err != nil {
		t.Fatalf("Agents.Create: %v", err)
	}
	task := seedEntityEventTask(t, conn, "actor-rt")
	insertBlueprintForTest(t, conn, "actor-bp", "Actor BP")

	// Manual CreateRun freezes + reads back the actor.
	if _, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "actor-bpr", BlueprintID: "actor-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-actor", ActorAgentID: agentID,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got, err := stores.Blueprints.GetRun(ctx, org, "actor-bpr")
	if err != nil || got == nil {
		t.Fatalf("GetRun = (%v, %v)", got, err)
	}
	if got.ActorAgentID != agentID {
		t.Errorf("manual actor round-trip = %q, want %q", got.ActorAgentID, agentID)
	}

	// Fenced event insert (CreateRunIfNotFiredSystem — the auto-fire hot path)
	// carries the actor too. Needs a real triggering_event_id (the task's event)
	// and trigger_id (an event_handler) for the fence FKs.
	var eventID string
	if err := conn.QueryRow(`SELECT primary_event_id FROM tasks WHERE id = ?`, task.ID).Scan(&eventID); err != nil {
		t.Fatalf("read task event id: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO event_handlers (id, kind, event_type, blueprint_id, breaker_threshold, min_autonomy_suitability, enabled, source, creator_user_id, team_id)
		VALUES ('actor-trig', 'trigger', ?, 'actor-bp', 4, 0, 1, 'user', ?, ?)
	`, domain.EventGitHubPRCICheckFailed, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed trigger: %v", err)
	}
	if inserted, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, org, domain.BlueprintRun{
		ID: "actor-bpr-ev", BlueprintID: "actor-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerEvent, TriggerID: "actor-trig", TriggeringEventID: eventID,
		Status: domain.BlueprintRunStatusRunning, WorktreePath: "/tmp/wt-actor-ev", ActorAgentID: agentID,
	}); err != nil || !inserted {
		t.Fatalf("CreateRunIfNotFiredSystem = (%v, %v), want (true, nil)", inserted, err)
	}
	ev, err := stores.Blueprints.GetRun(ctx, org, "actor-bpr-ev")
	if err != nil || ev == nil {
		t.Fatalf("GetRun (event) = (%v, %v)", ev, err)
	}
	if ev.ActorAgentID != agentID {
		t.Errorf("event (fenced) actor round-trip = %q, want %q", ev.ActorAgentID, agentID)
	}

	// No actor → empty, not an error.
	if _, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "actor-bpr-none", BlueprintID: "actor-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-actor-none",
	}); err != nil {
		t.Fatalf("CreateRun (no actor): %v", err)
	}
	none, err := stores.Blueprints.GetRun(ctx, org, "actor-bpr-none")
	if err != nil || none == nil {
		t.Fatalf("GetRun (no actor) = (%v, %v)", none, err)
	}
	if none.ActorAgentID != "" {
		t.Errorf("no-actor round-trip = %q, want empty", none.ActorAgentID)
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
	org := runmode.LocalDefaultOrgID

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
	insertConversationForTest(t, conn, domain.Conversation{
		ID: "op-run", TaskID: task.ID, PromptID: "op-step", Status: "running",
		Model: "claude-sonnet-4-6", BlueprintRunID: "op-blueprint-run", BlueprintStepIndex: &step0,
	})
	// Persist a terminal outcome the way processCompletion does.
	if err := stores.Conversations.CompleteSystem(ctx, org, "op-run", "completed", 0, 0, 0, "", "did the thing", "continue", "", ""); err != nil {
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

// TestBlueprintStore_SQLite_StepPlanLengths pins the batched plan-length read
// the run projection uses to say whether a step is its chain's last one. It
// counts in SQL over the frozen plan, and a blueprint run it cannot resolve is
// absent from the map rather than reported as a zero-step plan.
func TestBlueprintStore_SQLite_StepPlanLengths(t *testing.T) {
	conn := openSQLiteForTest(t)
	blueprints := sqlitestore.New(conn).Blueprints
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "blueprint-plan-len")
	insertBlueprintForTest(t, conn, "len-blueprint", "Len Blueprint")

	plan := []domain.BlueprintPlanStep{
		{StepIndex: 0, PromptID: "p0", PromptName: "One", PromptBody: "body one"},
		{StepIndex: 1, PromptID: "p1", PromptName: "Two", PromptBody: "body two"},
		{StepIndex: 2, PromptID: "p2", PromptName: "Three", PromptBody: "body three"},
	}
	if _, err := blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "len-bpr-three", BlueprintID: "len-blueprint", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		StepPlan: plan,
	}); err != nil {
		t.Fatalf("CreateRun (three steps): %v", err)
	}
	if _, err := blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "len-bpr-one", BlueprintID: "len-blueprint", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		StepPlan: plan[:1],
	}); err != nil {
		t.Fatalf("CreateRun (one step): %v", err)
	}

	got, err := blueprints.StepPlanLengths(ctx, org, []string{"len-bpr-three", "len-bpr-one", "len-bpr-missing"})
	if err != nil {
		t.Fatalf("StepPlanLengths: %v", err)
	}
	if got["len-bpr-three"] != 3 {
		t.Errorf("three-step plan length = %d, want 3", got["len-bpr-three"])
	}
	if got["len-bpr-one"] != 1 {
		t.Errorf("one-step plan length = %d, want 1", got["len-bpr-one"])
	}
	if _, ok := got["len-bpr-missing"]; ok {
		t.Error("an unresolvable blueprint run must be absent from the map, not reported as a zero-step plan")
	}

	empty, err := blueprints.StepPlanLengths(ctx, org, nil)
	if err != nil {
		t.Fatalf("StepPlanLengths(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("StepPlanLengths(nil) = %v, want an empty map", empty)
	}
}

// TestBlueprintStore_SQLite_MarkRunStatus_Guarded pins the lost-update
// race guard: only non-terminal statuses accept a transition.
func TestBlueprintStore_SQLite_MarkRunStatus_Guarded(t *testing.T) {
	conn := openSQLiteForTest(t)
	blueprints := sqlitestore.New(conn).Blueprints
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

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

// TestBlueprintStore_SQLite_ReopenRunForResume pins the resume re-open CAS: an
// aborted blueprint_run flips back to running (clearing its abort metadata) so a
// resume of its completed+abort step can re-finalize; a non-aborted row is a
// guarded no-op.
func TestBlueprintStore_SQLite_ReopenRunForResume(t *testing.T) {
	conn := openSQLiteForTest(t)
	blueprints := sqlitestore.New(conn).Blueprints
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "reopen")
	insertBlueprintForTest(t, conn, "reopen-blueprint", "Reopen Blueprint")
	brID, err := blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "reopen-run", BlueprintID: "reopen-blueprint", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// A running blueprint is not re-openable (the CAS guards on status='aborted').
	if reopened, err := blueprints.ReopenRunForResume(ctx, org, brID); err != nil || reopened {
		t.Errorf("ReopenRunForResume on running: reopened=%v err=%v, want false", reopened, err)
	}

	// Abort it (recording reason + step), then re-open: flips back to running and
	// clears the abort metadata so the resumed step re-finalizes cleanly.
	step := 2
	if _, err := blueprints.MarkRunStatus(ctx, org, brID, domain.BlueprintRunStatusAborted, "stopped for review", &step); err != nil {
		t.Fatalf("MarkRunStatus(aborted): %v", err)
	}
	reopened, err := blueprints.ReopenRunForResume(ctx, org, brID)
	if err != nil || !reopened {
		t.Fatalf("ReopenRunForResume on aborted: reopened=%v err=%v, want true", reopened, err)
	}
	cr, err := blueprints.GetRun(ctx, org, brID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if cr.Status != domain.BlueprintRunStatusRunning {
		t.Errorf("status = %q, want running", cr.Status)
	}
	if cr.AbortReason != "" {
		t.Errorf("abort_reason = %q, want cleared on re-open", cr.AbortReason)
	}
	if cr.AbortedAtStep != nil {
		t.Errorf("aborted_at_step = %v, want cleared on re-open", *cr.AbortedAtStep)
	}

	// Idempotent: a second re-open on the now-running row is a guarded no-op.
	if reopened, _ := blueprints.ReopenRunForResume(ctx, org, brID); reopened {
		t.Error("second ReopenRunForResume on running succeeded; want no-op false")
	}
}

// TestBlueprintStore_SQLite_CreateRun_RequiresTriggerType verifies the
// upfront validation: empty TriggerType errors rather than silently
// defaulting.
func TestBlueprintStore_SQLite_CreateRun_RequiresTriggerType(t *testing.T) {
	conn := openSQLiteForTest(t)
	blueprints := sqlitestore.New(conn).Blueprints
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "ttype")
	insertBlueprintForTest(t, conn, "ttype-blueprint", "T")

	if _, err := blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "ttype-run", BlueprintID: "ttype-blueprint", TaskID: task.ID,
		TriggerType: "", Status: domain.BlueprintRunStatusRunning,
	}); err == nil {
		t.Error("expected error for empty TriggerType, got nil")
	}
}

// TestBlueprintStore_SQLite_AssertsLocalOrg pins the local-org guard: any
// orgID other than runmode.LocalDefaultOrgID must fail loudly.
func TestBlueprintStore_SQLite_AssertsLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	blueprints := sqlitestore.New(conn).Blueprints
	if _, err := blueprints.ListSteps(context.Background(), "some-real-uuid", "anything"); err == nil {
		t.Fatal("ListSteps accepted non-local orgID; should reject")
	}
}
