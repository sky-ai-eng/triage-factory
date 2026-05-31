package sqlite_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// insertPromptForChainTest seeds a prompt row directly. PromptStore.Create
// exists but takes the full create-shape; for FK-only seeding we want a
// minimal raw INSERT, matching the pattern other sqlite_test files use
// when seeding tables they don't own.
func insertPromptForChainTest(t *testing.T, conn *sql.DB, p domain.Prompt) {
	t.Helper()
	if p.Kind == "" {
		p.Kind = domain.PromptKindLeaf
	}
	now := time.Now().UTC()
	if _, err := conn.Exec(`
		INSERT INTO prompts (id, name, body, source, kind, allowed_tools, usage_count, team_id, creator_user_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, '[]', 0, ?, ?, ?, ?)
	`, p.ID, p.Name, p.Body, p.Source, string(p.Kind), runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, now, now); err != nil {
		t.Fatalf("seed prompt %s: %v", p.ID, err)
	}
}

// seedEntityEventTask builds the entity → event → task FK chain that
// every chain test needs. Returns the task so tests can reference its
// ID. suffix scopes the seeded source_id so subtests don't collide.
func seedEntityEventTask(t *testing.T, conn *sql.DB, suffix string) *domain.Task {
	t.Helper()
	entity, _, err := sqlitestore.New(conn).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github",
		"owner/repo#"+suffix, "pr", "Chain Test "+suffix, "https://example.com/"+suffix)
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

// TestChainStore_SQLite_RunsForChain_RoundTrip protects the 16-column
// SELECT/Scan pair in RunsForChain against silent column-order drift.
func TestChainStore_SQLite_RunsForChain_RoundTrip(t *testing.T) {
	conn := openSQLiteForTest(t)
	chains := sqlitestore.New(conn).Chains
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "chain-rt")
	insertPromptForChainTest(t, conn, domain.Prompt{ID: "step-prompt-1", Name: "Step 1", Body: "do step 1", Source: "user"})
	insertPromptForChainTest(t, conn, domain.Prompt{ID: "step-prompt-2", Name: "Step 2", Body: "do step 2", Source: "user"})
	insertPromptForChainTest(t, conn, domain.Prompt{ID: "chain-prompt", Name: "My Chain", Body: "chain", Source: "user", Kind: domain.PromptKindChain})

	if err := chains.ReplaceSteps(ctx, org, "chain-prompt", []string{"step-prompt-1", "step-prompt-2"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}

	chainRunID, err := chains.CreateRun(ctx, org, domain.ChainRun{
		ID:            "chain-run-rt",
		ChainPromptID: "chain-prompt",
		TaskID:        task.ID,
		TriggerType:   domain.ChainTriggerManual,
		Status:        domain.ChainRunStatusRunning,
		WorktreePath:  "/tmp/wt-chain-rt",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if chainRunID != "chain-run-rt" {
		t.Fatalf("unexpected chain run id: %s", chainRunID)
	}

	step0 := 0
	step1 := 1
	for _, run := range []domain.AgentRun{
		{ID: "chain-step-run-0", TaskID: task.ID, PromptID: "step-prompt-1", Status: "initializing", Model: "claude-sonnet-4-6", ChainRunID: "chain-run-rt", ChainStepIndex: &step0},
		{ID: "chain-step-run-1", TaskID: task.ID, PromptID: "step-prompt-2", Status: "initializing", Model: "claude-sonnet-4-6", ChainRunID: "chain-run-rt", ChainStepIndex: &step1},
	} {
		if err := sqlitestore.New(conn).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrg, run); err != nil {
			t.Fatalf("create agent run %s: %v", run.ID, err)
		}
	}

	runs, err := chains.RunsForChain(ctx, org, "chain-run-rt")
	if err != nil {
		t.Fatalf("RunsForChain: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].ID != "chain-step-run-0" || runs[1].ID != "chain-step-run-1" {
		t.Errorf("unexpected order: %v", []string{runs[0].ID, runs[1].ID})
	}
	if runs[0].ChainStepIndex == nil || *runs[0].ChainStepIndex != 0 {
		t.Errorf("run[0].ChainStepIndex = %v, want 0", runs[0].ChainStepIndex)
	}
	if runs[1].ChainStepIndex == nil || *runs[1].ChainStepIndex != 1 {
		t.Errorf("run[1].ChainStepIndex = %v, want 1", runs[1].ChainStepIndex)
	}
	if runs[0].PromptID != "step-prompt-1" {
		t.Errorf("run[0].PromptID = %q, want step-prompt-1", runs[0].PromptID)
	}
	if runs[0].Model != "claude-sonnet-4-6" {
		t.Errorf("run[0].Model = %q, want claude-sonnet-4-6", runs[0].Model)
	}
}

// TestChainStore_SQLite_LatestVerdictsForRuns verifies the per-run
// "latest wins" projection: two verdicts for one run, abort is latest.
func TestChainStore_SQLite_LatestVerdictsForRuns(t *testing.T) {
	conn := openSQLiteForTest(t)
	chains := sqlitestore.New(conn).Chains
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "chain-verdict")
	insertPromptForChainTest(t, conn, domain.Prompt{ID: "vp-step", Name: "VP Step", Body: "x", Source: "user"})
	insertPromptForChainTest(t, conn, domain.Prompt{ID: "vp-chain", Name: "VP Chain", Body: "chain", Source: "user", Kind: domain.PromptKindChain})
	if err := chains.ReplaceSteps(ctx, org, "vp-chain", []string{"vp-step"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	if _, err := chains.CreateRun(ctx, org, domain.ChainRun{
		ID: "vp-chain-run", ChainPromptID: "vp-chain", TaskID: task.ID,
		TriggerType: domain.ChainTriggerManual, Status: domain.ChainRunStatusRunning,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	step0 := 0
	if err := sqlitestore.New(conn).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrg, domain.AgentRun{
		ID: "vp-run", TaskID: task.ID, PromptID: "vp-step", Status: "initializing",
		Model: "claude-sonnet-4-6", ChainRunID: "vp-chain-run", ChainStepIndex: &step0,
	}); err != nil {
		t.Fatalf("create agent run: %v", err)
	}

	advanceJSON, _ := json.Marshal(domain.ChainVerdict{Outcome: domain.ChainVerdictAdvance, Reason: "looks good"})
	abortJSON, _ := json.Marshal(domain.ChainVerdict{Outcome: domain.ChainVerdictAbort, Reason: "something broke"})
	if err := chains.InsertVerdict(ctx, org, "vp-run", string(advanceJSON)); err != nil {
		t.Fatalf("insert advance verdict: %v", err)
	}
	if err := chains.InsertVerdict(ctx, org, "vp-run", string(abortJSON)); err != nil {
		t.Fatalf("insert abort verdict: %v", err)
	}

	result, err := chains.LatestVerdictsForRuns(ctx, org, []string{"vp-run", "vp-run-no-verdict"})
	if err != nil {
		t.Fatalf("LatestVerdictsForRuns: %v", err)
	}
	v, ok := result["vp-run"]
	if !ok {
		t.Fatal("vp-run missing from result map")
	}
	if v.Outcome != domain.ChainVerdictAbort {
		t.Errorf("vp-run verdict outcome = %q, want %q (abort should win as the later write)", v.Outcome, domain.ChainVerdictAbort)
	}
	if _, ok := result["vp-run-no-verdict"]; ok {
		t.Error("vp-run-no-verdict should be absent — no artifacts written")
	}

	// Singular variant agrees.
	latest, err := chains.GetLatestVerdict(ctx, org, "vp-run")
	if err != nil {
		t.Fatalf("GetLatestVerdict: %v", err)
	}
	if latest == nil || latest.Outcome != domain.ChainVerdictAbort {
		t.Errorf("GetLatestVerdict outcome = %+v, want abort", latest)
	}
}

// TestChainStore_SQLite_MarkRunStatus_Guarded pins the lost-update
// race guard: only non-terminal statuses accept a transition.
func TestChainStore_SQLite_MarkRunStatus_Guarded(t *testing.T) {
	conn := openSQLiteForTest(t)
	chains := sqlitestore.New(conn).Chains
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "chain-guard")
	insertPromptForChainTest(t, conn, domain.Prompt{ID: "guard-chain", Name: "Guard Chain", Body: "chain", Source: "user", Kind: domain.PromptKindChain})

	chainRunID, err := chains.CreateRun(ctx, org, domain.ChainRun{
		ID: "chain-run-guard", ChainPromptID: "guard-chain", TaskID: task.ID,
		TriggerType: domain.ChainTriggerManual, Status: domain.ChainRunStatusRunning,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	changed, err := chains.MarkRunStatus(ctx, org, chainRunID, domain.ChainRunStatusCompleted, "", nil)
	if err != nil {
		t.Fatalf("MarkRunStatus: %v", err)
	}
	if !changed {
		t.Error("expected changed=true for running → completed")
	}

	changed2, err := chains.MarkRunStatus(ctx, org, chainRunID, domain.ChainRunStatusAborted, "late abort", nil)
	if err != nil {
		t.Fatalf("MarkRunStatus second: %v", err)
	}
	if changed2 {
		t.Error("expected changed=false when chain run already terminal")
	}

	cr, err := chains.GetRun(ctx, org, chainRunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if cr.Status != domain.ChainRunStatusCompleted {
		t.Errorf("status = %q, want completed", cr.Status)
	}
}

// TestChainStore_SQLite_CreateRun_RequiresTriggerType verifies the
// upfront validation: empty TriggerType errors rather than silently
// defaulting.
func TestChainStore_SQLite_CreateRun_RequiresTriggerType(t *testing.T) {
	conn := openSQLiteForTest(t)
	chains := sqlitestore.New(conn).Chains
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "ttype")
	insertPromptForChainTest(t, conn, domain.Prompt{ID: "ttype-chain", Name: "T", Body: "chain", Source: "user", Kind: domain.PromptKindChain})

	if _, err := chains.CreateRun(ctx, org, domain.ChainRun{
		ID: "ttype-run", ChainPromptID: "ttype-chain", TaskID: task.ID,
		TriggerType: "", Status: domain.ChainRunStatusRunning,
	}); err == nil {
		t.Error("expected error for empty TriggerType, got nil")
	}
}

// TestChainStore_SQLite_AssertsLocalOrg pins the local-org guard: any
// orgID other than runmode.LocalDefaultOrg must fail loudly.
func TestChainStore_SQLite_AssertsLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	chains := sqlitestore.New(conn).Chains
	if _, err := chains.ListSteps(context.Background(), "some-real-uuid", "anything"); err == nil {
		t.Fatal("ListSteps accepted non-local orgID; should reject")
	}
}
