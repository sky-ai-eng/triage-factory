package delegate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestMaterializePriorMemories_CreatesDirEvenWithNoPriors guards the
// invariant the prompt + completion-gate retry both depend on: the agent's
// initial cwd has a _scratch/entity-memory/<namespace>/ directory it can ls
// and write into, regardless of whether prior runs ever existed for this
// entity.
//
// Pre-fix, the function early-returned when len(memories)==0, so a
// first-run agent saw a missing directory and either (a) failed `ls` on its
// namespace folder outright or (b) failed the final memory write because the
// parent dir didn't exist.
func TestMaterializePriorMemories_CreatesDirEvenWithNoPriors(t *testing.T) {
	database := newDelegateTestDB(t)
	cwd := t.TempDir()

	stores := sqlitestore.New(database)
	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-100", "issue", "T", "https://x/100")
	if err != nil {
		t.Fatalf("entity: %v", err)
	}

	// Sanity: no memories for this entity yet.
	mems, err := stores.TaskMemory.GetMemoriesForEntity(context.Background(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil {
		t.Fatalf("GetMemoriesForEntity: %v", err)
	}
	if len(mems) != 0 {
		t.Fatalf("expected 0 priors for new entity, got %d", len(mems))
	}

	const namespace = "run-noprior"
	materializePriorMemories(stores.TaskMemory, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, cwd, entity.ID, namespace)

	// The current run's namespace folder is created even with no priors.
	nsDir := filepath.Join(cwd, "_scratch", "entity-memory", namespace)
	info, err := os.Stat(nsDir)
	if err != nil {
		t.Fatalf("namespace dir not created at %s: %v", nsDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("namespace path exists but is not a directory")
	}

	// And it's empty (no prior memory files materialized).
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		t.Fatalf("read namespace dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty namespace dir, found %d entries", len(entries))
	}
}

// TestMaterializePriorMemories_WritesPriors verifies the existing
// happy-path behavior survives the mkdir-first refactor.
func TestMaterializePriorMemories_WritesPriors(t *testing.T) {
	database := newDelegateTestDB(t)
	cwd := t.TempDir()

	stores := sqlitestore.New(database)
	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-200", "issue", "T", "https://x/200")
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	// Seed task + run + memory chain so GetMemoriesForEntity returns one row.
	evt, err := stores.Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventJiraIssueAssigned, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, "", evt, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	ensureTestPrompt(t, database, domain.Prompt{ID: "p1", Name: "T", Body: "x", Source: "user"})
	// Every run belongs to a blueprint_run now, so the prior memory is
	// namespaced by its blueprint_run_id. Seed that row (runs.blueprint_run_id is
	// NOT NULL and the run_memory FK needs it).
	const priorBlueprintRunID = "bpr-prior"
	if err := stores.Blueprints.Create(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: "bp-prior", Name: "BP", Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("blueprint: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, worktree_path, step_plan) VALUES (?, 'bp-prior', ?, 'manual', '/tmp/wt', '[]')`,
		priorBlueprintRunID, task.ID,
	); err != nil {
		t.Fatalf("blueprint_run: %v", err)
	}
	dbtest.SeedConversation(t, database, domain.AgentRun{
		ID: "prior-run", TaskID: task.ID, PromptID: "p1", Status: "completed", Model: "m", BlueprintRunID: priorBlueprintRunID,
	})
	if err := stores.TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, "prior-run", entity.ID, priorBlueprintRunID, "what i did last time"); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}
	// The primary join row a real run's completion will carry once the
	// run-end attach ticket (TFAC-625) lands — materializePriorMemories'
	// join-based read (TFAC-622) needs it to find anything.
	if err := stores.TaskMemory.RecordEntityTouchSystem(context.Background(), runmode.LocalDefaultOrgID, "prior-run", entity.ID, domain.MemoryRolePrimary); err != nil {
		t.Fatalf("seed join row: %v", err)
	}

	materializePriorMemories(stores.TaskMemory, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, cwd, entity.ID, "current-run")

	// The prior's namespace is its blueprint_run_id — the file lands under
	// <blueprint_run_id>/<run_id>.md, never at the top level.
	priorPath := filepath.Join(cwd, "_scratch", "entity-memory", priorBlueprintRunID, "prior-run.md")
	body, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatalf("expected materialized prior at %s: %v", priorPath, err)
	}
	if string(body) != "what i did last time" {
		t.Errorf("prior content = %q, want %q", string(body), "what i did last time")
	}
}

// TestMaterializePriorMemories_BlueprintSiblingsShareFolder is the acceptance
// test for the handoff mechanic: two steps of the same blueprint run write
// their memory under a shared <blueprint_run_id>/ folder, so when step 2
// materializes priors it finds step 1's memory in its own namespace folder —
// that's the handoff. Also pins the "no top-level .md files" invariant.
func TestMaterializePriorMemories_BlueprintSiblingsShareFolder(t *testing.T) {
	database := newDelegateTestDB(t)
	cwd := t.TempDir()
	stores := sqlitestore.New(database)
	ctx := context.Background()

	entity, _, err := stores.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "owner/repo#7", "pr", "T", "https://x/7")
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	evt, err := stores.Events.Record(ctx, runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventJiraIssueAssigned, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, "", evt, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	ensureTestPrompt(t, database, domain.Prompt{ID: "p1", Name: "T", Body: "x", Source: "user"})

	// Seed the blueprint + blueprint_run the two step runs share. The
	// run_memory.blueprint_run_id FK (ON DELETE SET NULL) needs this row.
	const blueprintRunID = "bpr-shared"
	if err := stores.Blueprints.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: "bp1", Name: "BP", Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("blueprint: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, worktree_path, step_plan) VALUES (?, 'bp1', ?, 'manual', '/tmp/wt', '[]')`,
		blueprintRunID, task.ID,
	); err != nil {
		t.Fatalf("blueprint_run: %v", err)
	}

	// Step 1 runs and writes its memory under the shared blueprint namespace.
	dbtest.SeedConversation(t, database, domain.AgentRun{
		ID: "step1-run", TaskID: task.ID, PromptID: "p1", Status: "completed", Model: "m", BlueprintRunID: blueprintRunID,
	})
	if err := stores.TaskMemory.UpsertAgentMemory(ctx, runmode.LocalDefaultOrgID, "step1-run", entity.ID, blueprintRunID, "step 1 findings"); err != nil {
		t.Fatalf("step1 memory: %v", err)
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, runmode.LocalDefaultOrgID, "step1-run", entity.ID, domain.MemoryRolePrimary); err != nil {
		t.Fatalf("seed join row: %v", err)
	}

	// Step 2 starts: materialize priors under step 2's own namespace (the same
	// shared blueprint_run_id). Step 1's memory must land in that folder.
	materializePriorMemories(stores.TaskMemory, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, cwd, entity.ID, blueprintRunID)

	step1Path := filepath.Join(cwd, "_scratch", "entity-memory", blueprintRunID, "step1-run.md")
	body, err := os.ReadFile(step1Path)
	if err != nil {
		t.Fatalf("step 2 should read step 1's memory from the shared %s/ folder: %v", blueprintRunID, err)
	}
	if string(body) != "step 1 findings" {
		t.Errorf("step1 memory = %q, want %q", string(body), "step 1 findings")
	}

	// No loose .md files at the top level — the tree is all <namespace>/ folders.
	entries, err := os.ReadDir(filepath.Join(cwd, "_scratch", "entity-memory"))
	if err != nil {
		t.Fatalf("read entity-memory: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("found top-level file %q under entity-memory/; the tree must be all namespace folders", e.Name())
		}
	}
}
