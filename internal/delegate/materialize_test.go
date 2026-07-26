package delegate

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestMaterializePriorMemories_CreatesDirsEvenWithNoPriors guards the invariant
// the prompt depends on: the agent's initial cwd has both memory folders it is
// told to look in, regardless of whether prior runs ever existed for this
// entity. Without them the instructed `ls` fails noisily on a first run.
func TestMaterializePriorMemories_CreatesDirsEvenWithNoPriors(t *testing.T) {
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

	materializePriorMemories(stores.TaskMemory, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, cwd, entity.ID, "bpr-noprior", nil)

	for _, name := range []string{currentRunDirName, priorRunsDirName} {
		dir := filepath.Join(cwd, "_tfac", "entity-memory", name)
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s/ not created at %s: %v", name, dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s exists but is not a directory", dir)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		if len(entries) != 0 {
			t.Errorf("expected empty %s/, found %d entries", name, len(entries))
		}
	}
}

// TestMaterializePriorMemories_ReadableLayout is the acceptance test for the
// materialized layout: two earlier steps of the CURRENT workflow run land in
// this-run/ numbered in step order and named after their prompts, and a prior
// separate run on the same entity lands in history/ named by its date.
func TestMaterializePriorMemories_ReadableLayout(t *testing.T) {
	database := newDelegateTestDB(t)
	cwd := t.TempDir()
	stores := sqlitestore.New(database)
	ctx := context.Background()

	entity, task := seedMemoryFixture(t, database, stores, "owner/repo#7")

	const currentBlueprintRunID = "bpr-current"
	const priorBlueprintRunID = "bpr-prior"
	seedBlueprintRun(t, database, stores, "bp-current", currentBlueprintRunID, task.ID)
	seedBlueprintRun(t, database, stores, "bp-prior", priorBlueprintRunID, task.ID)

	ensureTestPrompt(t, database, domain.Prompt{ID: "p-triage", Name: "Triage", Body: "x", Source: "user"})
	ensureTestPrompt(t, database, domain.Prompt{ID: "p-implement", Name: "Implement the fix", Body: "x", Source: "user"})
	ensureTestPrompt(t, database, domain.Prompt{ID: "p-cifix", Name: "CI Fix", Body: "x", Source: "user"})

	// The current workflow run's two earlier steps, seeded newest-first so the
	// materializer is provably ordering on step index and not on arrival order.
	seedMemory(t, ctx, stores, database, memoryFixture{
		runID: "step2-run", promptID: "p-implement", stepIndex: 1,
		blueprintRunID: currentBlueprintRunID, entityID: entity.ID, taskID: task.ID,
		content: "step 2 findings",
	})
	seedMemory(t, ctx, stores, database, memoryFixture{
		runID: "step1-run", promptID: "p-triage", stepIndex: 0,
		blueprintRunID: currentBlueprintRunID, entityID: entity.ID, taskID: task.ID,
		content: "step 1 findings",
	})
	// A prior, separate run on the same entity.
	seedMemory(t, ctx, stores, database, memoryFixture{
		runID: "prior-run", promptID: "p-cifix", stepIndex: 0,
		blueprintRunID: priorBlueprintRunID, entityID: entity.ID, taskID: task.ID,
		content: "what i did last time", createdAt: "2026-07-20 09:30:00+00:00",
	})

	materializePriorMemories(stores.TaskMemory, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, cwd, entity.ID, currentBlueprintRunID, nil)

	root := filepath.Join(cwd, "_tfac", "entity-memory")
	assertMemoryFile(t, filepath.Join(root, "this-run", "01-triage.md"), "step 1 findings")
	assertMemoryFile(t, filepath.Join(root, "this-run", "02-implement-the-fix.md"), "step 2 findings")
	assertMemoryFile(t, filepath.Join(root, "history", "2026-07-20-ci-fix.md"), "what i did last time")

	// Nothing else: no id-named leftovers, no loose files at the top level.
	assertDirNames(t, root, []string{"history", "this-run"})
	assertDirNames(t, filepath.Join(root, "this-run"), []string{"01-triage.md", "02-implement-the-fix.md"})
	assertDirNames(t, filepath.Join(root, "history"), []string{"2026-07-20-ci-fix.md"})
}

// TestMaterializePriorMemories_HistoryNameCollision pins the disambiguation: two
// prior runs of the same prompt on the same day must both survive materializing,
// rather than the second silently overwriting the first.
func TestMaterializePriorMemories_HistoryNameCollision(t *testing.T) {
	database := newDelegateTestDB(t)
	cwd := t.TempDir()
	stores := sqlitestore.New(database)
	ctx := context.Background()

	entity, task := seedMemoryFixture(t, database, stores, "owner/repo#9")
	seedBlueprintRun(t, database, stores, "bp-a", "bpr-a", task.ID)
	seedBlueprintRun(t, database, stores, "bp-b", "bpr-b", task.ID)
	ensureTestPrompt(t, database, domain.Prompt{ID: "p-cifix", Name: "CI Fix", Body: "x", Source: "user"})

	seedMemory(t, ctx, stores, database, memoryFixture{
		runID: "run-a", promptID: "p-cifix", stepIndex: 0, blueprintRunID: "bpr-a",
		entityID: entity.ID, taskID: task.ID, content: "first attempt", createdAt: "2026-07-20 09:00:00+00:00",
	})
	seedMemory(t, ctx, stores, database, memoryFixture{
		runID: "run-b", promptID: "p-cifix", stepIndex: 0, blueprintRunID: "bpr-b",
		entityID: entity.ID, taskID: task.ID, content: "second attempt", createdAt: "2026-07-20 17:00:00+00:00",
	})

	materializePriorMemories(stores.TaskMemory, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, cwd, entity.ID, "bpr-current", nil)

	historyDir := filepath.Join(cwd, "_tfac", "entity-memory", "history")
	assertMemoryFile(t, filepath.Join(historyDir, "2026-07-20-ci-fix.md"), "first attempt")
	assertMemoryFile(t, filepath.Join(historyDir, "2026-07-20-ci-fix-2.md"), "second attempt")
}

// TestBlueprintHandoff_MemorySurvivesTheFixedPathClear is the round trip the
// shared worktree depends on. Step 1 writes the one path it is told about;
// termination ingests it into conversation_memory under the workflow run; step
// 2 starting in the SAME tree renders it back as a numbered file under
// this-run/ and only then clears the path for its own write. The clear must
// never be the reason a later step loses its predecessor's handoff.
func TestBlueprintHandoff_MemorySurvivesTheFixedPathClear(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "handoff")
	makeRunBlueprintStep(t, database, runID, taskID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()
	blueprintRunID := "bpr-" + runID

	// Step 1 writes the fixed path and terminates.
	writeAgentMemory(t, cwd, "step 1 chose approach X because Y")
	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, runID, blueprintRunID, task,
		res(`{"outcome":"continue","summary":"did step work"}`), cwd, "", "event", "")

	// Step 2's run start, in the order runAgent performs it.
	materializePriorMemories(s.taskMemory, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, cwd, task.EntityID, blueprintRunID, nil)
	clearAgentMemoryFile(cwd, nil)

	// Step 1's narrative is where the prompt tells step 2 to look — named for
	// the step and the prompt that produced it, not for either id.
	assertMemoryFile(t, filepath.Join(cwd, "_tfac", "entity-memory", "this-run", "01-t.md"), "step 1 chose approach X because Y")
	// And the write path is free, so step 2's own memory can't be confused for
	// step 1's if step 2 writes nothing.
	if _, state := readAgentMemoryFile(cwd); state != memoryFileMissing {
		t.Errorf("state = %v, want memoryFileMissing after the clear", state)
	}
}

// TestClearAgentMemoryFile pins the shared-worktree guard: the steps of one
// blueprint run all write the same filename, so a fresh step must not inherit
// its predecessor's file and have it ingested as its own memory.
func TestClearAgentMemoryFile(t *testing.T) {
	cwd := t.TempDir()

	// Absent file: a no-op, not an error path anyone notices.
	clearAgentMemoryFile(cwd, nil)
	if _, state := readAgentMemoryFile(cwd); state != memoryFileMissing {
		t.Errorf("state = %v, want memoryFileMissing", state)
	}

	writeAgentMemory(t, cwd, "the previous step's memory")
	clearAgentMemoryFile(cwd, nil)
	content, state := readAgentMemoryFile(cwd)
	if state != memoryFileMissing {
		t.Errorf("state = %v (content %q), want memoryFileMissing", state, content)
	}
}

// TestScanRepoFiles_GuardsEveryWriteIntoATrackedScratchDir is the
// repo-content guard, end to end. A GitHub PR run's tree IS the repo checkout,
// and .git/info/exclude does nothing for a path the repo already tracks — so a
// repo that happens to keep files under the scratch dir would have them deleted
// or overwritten by TF's own materialization and the change swept into the
// agent's next commit. Neither the delete nor a materialized write may touch
// one, and the repo must come out of run start exactly as clean as it went in.
func TestScanRepoFiles_GuardsEveryWriteIntoATrackedScratchDir(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	initTestRepo(t, cwd)

	// A repo that tracks BOTH the agent's write path and a name the history
	// materializer would otherwise choose for itself.
	writeAgentMemory(t, cwd, "repo-authored memory")
	historyName := filepath.Join(cwd, "_tfac", "entity-memory", "history", "2026-07-20-ci-fix.md")
	if err := os.MkdirAll(filepath.Dir(historyName), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFileT(t, historyName, "repo-authored history")
	runTestGit(t, cwd, "add", "-A")
	runTestGit(t, cwd, "commit", "-qm", "track files under the scratch dir")
	// The managed exclude a real worktree carries — present, and beside the
	// point for an already-tracked path. That is the whole hazard.
	writeFileT(t, filepath.Join(cwd, ".git", "info", "exclude"), "_tfac/\n")

	owned := scanRepoFiles(ctx, cwd)
	if !owned.owns("memory.md") || !owned.owns("entity-memory", "history", "2026-07-20-ci-fix.md") {
		t.Fatalf("scanRepoFiles missed a tracked path: %v", owned)
	}

	// Run start: the clear, plus a materialization whose history name collides
	// with the tracked one.
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	entity, task := seedMemoryFixture(t, database, stores, "owner/repo#42")
	seedBlueprintRun(t, database, stores, "bp-guard", "bpr-guard", task.ID)
	ensureTestPrompt(t, database, domain.Prompt{ID: "p-cifix", Name: "CI Fix", Body: "x", Source: "user"})
	seedMemory(t, ctx, stores, database, memoryFixture{
		runID: "prior-run", promptID: "p-cifix", stepIndex: 0, blueprintRunID: "bpr-guard",
		entityID: entity.ID, taskID: task.ID, content: "TF memory that must not land",
		createdAt: "2026-07-20 09:30:00+00:00",
	})

	clearAgentMemoryFile(cwd, owned)
	materializePriorMemories(stores.TaskMemory, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, cwd, entity.ID, "bpr-current", owned)

	assertMemoryFile(t, filepath.Join(cwd, "_tfac", "memory.md"), "repo-authored memory")
	assertMemoryFile(t, historyName, "repo-authored history")
	if out := runTestGit(t, cwd, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("run start dirtied the repo: git status --porcelain = %q", out)
	}
}

func writeAgentMemory(t *testing.T, cwd, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cwd, "_tfac"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFileT(t, filepath.Join(cwd, "_tfac", "memory.md"), content)
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// initTestRepo makes cwd a git working tree, skipping the test when git isn't
// installed (the same posture the worktree package's git-backed tests take).
func initTestRepo(t *testing.T, cwd string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	runTestGit(t, cwd, "init", "-q", ".")
	runTestGit(t, cwd, "config", "user.email", "test@example.com")
	runTestGit(t, cwd, "config", "user.name", "Test")
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestMemorySlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Triage", "triage"},
		{"CI Fix", "ci-fix"},
		{"Implement the fix", "implement-the-fix"},
		{"  Fix — review feedback! ", "fix-review-feedback"},
		{"", ""},
		{"///", ""},
		{"A very long prompt name that runs well past the cap", "a-very-long-prompt-name-that-run"},
		// A word boundary one byte under the cap: the next character costs its
		// separator too, so neither is taken. Charging that pair only after
		// writing it puts the slug two bytes over the bound.
		{strings.Repeat("a", memorySlugMaxLen-1) + " de", strings.Repeat("a", memorySlugMaxLen-1)},
		// Non-ASCII never contributes bytes of its own — it reads as a
		// separator, so the cap accounting stays a byte count.
		{"ünïcödé wörds hére", "n-c-d-w-rds-h-re"},
	}
	for _, c := range cases {
		got := memorySlug(c.in)
		if got != c.want {
			t.Errorf("memorySlug(%q) = %q, want %q", c.in, got, c.want)
		}
		if len(got) > memorySlugMaxLen {
			t.Errorf("memorySlug(%q) = %q (%d bytes), over the %d cap", c.in, got, len(got), memorySlugMaxLen)
		}
	}
}

// --- fixtures -------------------------------------------------------------

// seedMemoryFixture stages the entity + event + task chain a conversation_memory
// row hangs off, and returns both.
func seedMemoryFixture(t *testing.T, database *sql.DB, stores db.Stores, sourceID string) (domain.Entity, domain.Task) {
	t.Helper()
	ctx := context.Background()
	entity, _, err := stores.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", sourceID, "pr", "T", "https://x/"+sourceID)
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
	return *entity, *task
}

// seedBlueprintRun stages the blueprint + blueprint_run pair a step
// conversation (and its conversation_memory row's blueprint_run_id) FKs to.
func seedBlueprintRun(t *testing.T, database *sql.DB, stores db.Stores, blueprintID, blueprintRunID, taskID string) {
	t.Helper()
	if err := stores.Blueprints.Create(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: blueprintID, Name: "BP", Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("blueprint %s: %v", blueprintID, err)
	}
	if _, err := database.Exec(
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, worktree_path, step_plan) VALUES (?, ?, ?, 'manual', '/tmp/wt', '[]')`,
		blueprintRunID, blueprintID, taskID,
	); err != nil {
		t.Fatalf("blueprint_run %s: %v", blueprintRunID, err)
	}
}

// memoryFixture describes one prior run's memory: the conversation that
// produced it (prompt + step index + workflow run) and the row's content.
type memoryFixture struct {
	runID          string
	promptID       string
	stepIndex      int
	blueprintRunID string
	entityID       string
	taskID         string
	content        string
	createdAt      string // conversation_memory.created_at override; "" keeps now()
}

func seedMemory(t *testing.T, ctx context.Context, stores db.Stores, database *sql.DB, f memoryFixture) {
	t.Helper()
	stepIndex := f.stepIndex
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID: f.runID, TaskID: f.taskID, PromptID: f.promptID, Status: "completed", Model: "m",
		BlueprintRunID: f.blueprintRunID, BlueprintStepIndex: &stepIndex,
	})
	if err := stores.TaskMemory.UpsertAgentMemory(ctx, runmode.LocalDefaultOrgID, f.runID, f.entityID, f.blueprintRunID, f.content); err != nil {
		t.Fatalf("upsert memory %s: %v", f.runID, err)
	}
	// The primary join row a real run's completion writes — the entity-scoped
	// read is join-based, so without it the materializer sees nothing.
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, runmode.LocalDefaultOrgID, f.runID, f.entityID, domain.MemoryRolePrimary); err != nil {
		t.Fatalf("seed join row %s: %v", f.runID, err)
	}
	if f.createdAt != "" {
		if _, err := database.Exec(`UPDATE conversation_memory SET created_at = ? WHERE conversation_id = ?`, f.createdAt, f.runID); err != nil {
			t.Fatalf("stamp created_at %s: %v", f.runID, err)
		}
	}
}

func assertMemoryFile(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected materialized memory at %s: %v", path, err)
	}
	if string(body) != want {
		t.Errorf("%s = %q, want %q", path, string(body), want)
	}
}

func assertDirNames(t *testing.T, dir string, want []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != len(want) {
		t.Fatalf("%s contains %v, want %v", dir, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s contains %v, want %v", dir, got, want)
			return
		}
	}
}
