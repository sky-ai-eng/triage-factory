package delegate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// slackTaskFixture seeds a slack-sourced entity/event/task and returns the
// spawner + task, for tests that call setupSlack directly.
func slackTaskFixture(t *testing.T, suffix string) (*Spawner, *sql.DB, domain.Task) {
	t.Helper()
	database := newDelegateTestDB(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	stores := sqlitestore.New(database)

	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "slack", "C1/1700000000.000100-"+suffix, "message", "T", "https://x/"+suffix)
	if err != nil {
		t.Fatalf("create slack entity: %v", err)
	}
	eventID, err := stores.Events.Record(ctx, org, domain.Event{
		EventType: domain.EventSlackMessage, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := stores.Tasks.FindOrCreate(ctx, org, runmode.LocalDefaultTeamID, entity.ID, domain.EventSlackMessage, suffix, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	return s, database, *task
}

// TestSetupSlack_WorktreeLessShape pins the shape TFAC-591 requires: a Slack
// run mirrors Jira's lazy worktree-less setup.
func TestSetupSlack_WorktreeLessShape(t *testing.T) {
	s, _, task := slackTaskFixture(t, "shape")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	conversationID := "slack-run-shape"
	rootKey := "slack-bp-shape" // run-root is blueprint-keyed, distinct from conversationID
	t.Cleanup(func() { worktree.RemoveRunRoot(rootKey) })

	cfg, err := s.setupSlack(ctx, org, conversationID, "", rootKey, runmode.LocalDefaultUserID, task, nil)
	if err != nil {
		t.Fatalf("setupSlack: %v", err)
	}
	if cfg.hasWT {
		t.Error("setupSlack must be worktree-less (hasWT=false)")
	}
	if cfg.owner != "" || cfg.repo != "" {
		t.Errorf("expected empty owner/repo, got %q/%q", cfg.owner, cfg.repo)
	}
	if cfg.wtPath == "" || cfg.wtPath != cfg.runRoot {
		t.Errorf("expected wtPath == runRoot (both the run-root), got wtPath=%q runRoot=%q", cfg.wtPath, cfg.runRoot)
	}
	// The run-root is blueprint-keyed (rootKey), NOT this step's run id — the
	// invariant a cold rehydrate (which rebuilds under the blueprint key) and
	// the launch-time worktree pin both rely on.
	if cfg.runRoot != worktree.RunRoot(rootKey) {
		t.Errorf("run-root = %q, want it keyed by the blueprint run id (RunRoot(%q)=%q), not conversationID %q", cfg.runRoot, rootKey, worktree.RunRoot(rootKey), conversationID)
	}
	wantScope := "Slack thread: " + task.EntitySourceID
	if cfg.scope != wantScope {
		t.Errorf("scope = %q, want %q", cfg.scope, wantScope)
	}
}

// TestSetupSlack_PersistsWorktreePath pins that the run-root is stamped
// onto conversations.worktree_path via SetWorktreePathSystem, same as
// setupJira — required for resume (`claude --resume` keys session storage
// off cwd).
func TestSetupSlack_PersistsWorktreePath(t *testing.T) {
	s, database, task := slackTaskFixture(t, "persist")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	// Seed a minimal conversations row for SetWorktreePathSystem's UPDATE to hit.
	ensureTestPrompt(t, database, domain.Prompt{ID: "persist-prompt", Name: "T", Body: "x", Source: "user"})
	brID := seedConversationBlueprint(t, database, "persist", task.ID)
	stepIdx := 0
	conversationID := "slack-run-persist"
	rootKey := brID // the run-root keys by the blueprint run id
	t.Cleanup(func() { worktree.RemoveRunRoot(rootKey) })
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID: conversationID, TaskID: task.ID, PromptID: "persist-prompt", Status: "running",
		Model: "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
	})

	cfg, err := s.setupSlack(ctx, org, conversationID, "", rootKey, runmode.LocalDefaultUserID, task, nil)
	if err != nil {
		t.Fatalf("setupSlack: %v", err)
	}

	var wtPath string
	if err := database.QueryRow(`SELECT COALESCE(worktree_path,'') FROM conversations WHERE id = ?`, conversationID).Scan(&wtPath); err != nil {
		t.Fatalf("read persisted worktree_path: %v", err)
	}
	if wtPath != cfg.wtPath {
		t.Errorf("persisted conversations.worktree_path = %q, want %q", wtPath, cfg.wtPath)
	}
}

// TestSetupSlack_ToolsRef_NoRegisteredReference pins that a Slack run works
// with base tools when no ee package has registered a "slack" tools
// reference — no error, and the GitHub verbs are still documented.
func TestSetupSlack_ToolsRef_NoRegisteredReference(t *testing.T) {
	agentprompt.ResetToolsReferences()
	s, _, task := slackTaskFixture(t, "notools")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	conversationID := "slack-run-notools"
	rootKey := "slack-bp-notools" // run-root is blueprint-keyed, distinct from conversationID
	t.Cleanup(func() { worktree.RemoveRunRoot(rootKey) })

	cfg, err := s.setupSlack(ctx, org, conversationID, "", rootKey, runmode.LocalDefaultUserID, task, nil)
	if err != nil {
		t.Fatalf("setupSlack: %v", err)
	}
	if !strings.Contains(cfg.toolsRef, "exec gh") {
		t.Errorf("toolsRef documents no github verbs: %q", cfg.toolsRef)
	}
	if strings.Contains(cfg.toolsRef, "SLACK VERB DOCS") {
		t.Errorf("toolsRef documents slack with nothing registered for it: %q", cfg.toolsRef)
	}
}

// TestSetupSlack_ToolsRef_ComposesRegisteredReference pins the composition:
// a registered "slack" tools reference is appended after the GH template
// (GH tools are needed too — the agent acquires repos on demand).
func TestSetupSlack_ToolsRef_ComposesRegisteredReference(t *testing.T) {
	t.Cleanup(agentprompt.ResetToolsReferences)
	agentprompt.RegisterToolsReference("slack", "SLACK VERB DOCS")

	s, _, task := slackTaskFixture(t, "withtools")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	conversationID := "slack-run-withtools"
	rootKey := "slack-bp-withtools" // run-root is blueprint-keyed, distinct from conversationID
	t.Cleanup(func() { worktree.RemoveRunRoot(rootKey) })

	cfg, err := s.setupSlack(ctx, org, conversationID, "", rootKey, runmode.LocalDefaultUserID, task, nil)
	if err != nil {
		t.Fatalf("setupSlack: %v", err)
	}
	// Composed by source rather than by exact bytes: both families present,
	// core first. The assembly is canonical, so a caller's order and repeats
	// cannot change what the agent reads.
	if !strings.Contains(cfg.toolsRef, "exec gh") || !strings.Contains(cfg.toolsRef, "SLACK VERB DOCS") {
		t.Errorf("toolsRef = %q, want both the github and the registered slack verbs", cfg.toolsRef)
	}
	if strings.Index(cfg.toolsRef, "exec gh") > strings.Index(cfg.toolsRef, "SLACK VERB DOCS") {
		t.Errorf("toolsRef = %q, want github before the registered source", cfg.toolsRef)
	}
}

// slackBlueprintFixture seeds a full 1-step blueprint + running blueprint_run
// on a slack-sourced task, for exercising buildStepConfig's switch dispatch
// end to end (both the first-claim and later-step branches).
func slackBlueprintFixture(t *testing.T, suffix string) (*Spawner, *sql.DB, string, domain.Task, domain.Conversation) {
	t.Helper()
	s, database, task := slackTaskFixture(t, suffix)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	stores := sqlitestore.New(database)

	bpID := "sbp-" + suffix
	if _, err := stores.Blueprints.Create(ctx, org, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: bpID, Name: bpID, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("create blueprint: %v", err)
	}
	promptID := suffix + "-p0"
	ensureTestPrompt(t, database, domain.Prompt{ID: promptID, Name: promptID, Body: "b", Source: "user"})
	if _, err := stores.Blueprints.ReplaceSteps(ctx, org, bpID, []string{promptID}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	plan := []domain.BlueprintPlanStep{{
		StepIndex: 0, PromptID: promptID, PromptName: promptID, PromptBody: "b", Source: "user",
	}}
	created, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "sbpr-" + suffix, BlueprintID: bpID, TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		StepPlan: plan,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	brID := created.ID

	conv := domain.Conversation{ID: "srun-" + suffix, TaskID: task.ID, BlueprintRunID: brID}
	return s, database, brID, task, conv
}

// TestBuildStepConfig_Slack_FirstClaim pins wire point 1 (dispatch.go's
// first-claim switch): a slack task no longer hits the "unsupported task
// source" error the enabled-by-default slack:message trigger was
// tripping on — buildStepConfig routes it through setupSlack and stamps the
// resolved worktree path onto the blueprint_run.
func TestBuildStepConfig_Slack_FirstClaim(t *testing.T) {
	s, database, brID, task, conv := slackBlueprintFixture(t, "first")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	t.Cleanup(func() { worktree.RemoveRunRoot(conv.ID) })

	br, err := s.blueprints.GetRunSystem(ctx, org, brID)
	if err != nil || br == nil {
		t.Fatalf("GetRunSystem: (%v, %v)", br, err)
	}

	cfg, err := s.buildStepConfig(ctx, org, br, task, conv, nil, nil)
	if err != nil {
		t.Fatalf("buildStepConfig: %v (slack must be a supported source)", err)
	}
	if cfg.hasWT {
		t.Error("expected hasWT=false for a slack step")
	}
	if cfg.wtPath == "" {
		t.Error("expected a non-empty worktree path")
	}

	var stampedPath string
	if err := database.QueryRow(`SELECT worktree_path FROM blueprint_runs WHERE id = ?`, brID).Scan(&stampedPath); err != nil {
		t.Fatalf("read blueprint_runs.worktree_path: %v", err)
	}
	if stampedPath != cfg.wtPath {
		t.Errorf("blueprint_runs.worktree_path = %q, want %q", stampedPath, cfg.wtPath)
	}
}

// TestBuildStepConfig_Slack_LaterStep pins wire point 2 (dispatch.go's
// later-step switch): a re-claim (or step advance) rehydrates the shared
// worktree-less run-root and recomposes scope/toolsRef identically to the
// first claim.
func TestBuildStepConfig_Slack_LaterStep(t *testing.T) {
	s, database, brID, task, conv := slackBlueprintFixture(t, "later")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	// Simulate a prior first claim: an existing on-disk run-root stamped onto
	// the blueprint_run, so ensureWorkspace warm-returns it without needing a
	// blob store.
	wtDir := t.TempDir()
	if _, err := database.Exec(`UPDATE blueprint_runs SET worktree_path = ? WHERE id = ?`, wtDir, brID); err != nil {
		t.Fatalf("stamp worktree_path: %v", err)
	}

	br, err := s.blueprints.GetRunSystem(ctx, org, brID)
	if err != nil || br == nil {
		t.Fatalf("GetRunSystem: (%v, %v)", br, err)
	}

	cfg, err := s.buildStepConfig(ctx, org, br, task, conv, nil, nil)
	if err != nil {
		t.Fatalf("buildStepConfig: %v", err)
	}
	if cfg.hasWT {
		t.Error("expected hasWT=false for a slack step")
	}
	if cfg.wtPath != wtDir {
		t.Errorf("wtPath = %q, want warm-returned %q", cfg.wtPath, wtDir)
	}
	wantScope := "Slack thread: " + task.EntitySourceID
	if cfg.scope != wantScope {
		t.Errorf("scope = %q, want %q", cfg.scope, wantScope)
	}
	if !strings.Contains(cfg.toolsRef, "exec gh") {
		t.Errorf("toolsRef documents no github verbs: %q", cfg.toolsRef)
	}
}
