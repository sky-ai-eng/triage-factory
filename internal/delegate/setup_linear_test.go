package delegate

import (
	"context"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestSetupLinear_RunRootAndTools: a Linear run starts at a plain run root
// keyed by its task, like a Jira run, and its <tools> section carries the
// Linear verbs beside GitHub's whatever else the org has.
func TestSetupLinear_RunRootAndTools(t *testing.T) {
	database := newDelegateTestDB(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	stores := sqlitestore.New(database)

	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "linear", "ws-1", "ENG-7", "uuid-7", "issue", "Fix it", "https://linear.app/acme/issue/ENG-7")
	if err != nil {
		t.Fatalf("create linear entity: %v", err)
	}
	eventID, err := stores.Events.Record(ctx, org, domain.Event{
		EventType: domain.EventLinearIssueAssigned, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := stores.Tasks.FindOrCreate(ctx, org, runmode.LocalDefaultTeamID, entity.ID, domain.EventLinearIssueAssigned, "", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	rootKey := workspaceKey(task.ID)
	t.Cleanup(func() { worktree.RemoveRunRoot(rootKey) })
	cfg, err := s.setupLinear(ctx, org, "linear-run", "", rootKey, runmode.LocalDefaultUserID, *task)
	if err != nil {
		t.Fatalf("setupLinear: %v", err)
	}
	if cfg.owner != "" || cfg.repo != "" || cfg.prCheckout != "" {
		t.Errorf("expected no repo and no PR checkout, got %q/%q at %q", cfg.owner, cfg.repo, cfg.prCheckout)
	}
	if cfg.runRoot != worktree.RunRoot(rootKey) {
		t.Errorf("run-root = %q, want it keyed by the task: %q", cfg.runRoot, worktree.RunRoot(rootKey))
	}
	if want := "Linear issue: ENG-7"; cfg.scope != want {
		t.Errorf("scope = %q, want %q", cfg.scope, want)
	}
	for _, want := range []string{"triagefactory exec linear issue view", "triagefactory exec gh"} {
		if !strings.Contains(cfg.toolsRef, want) {
			t.Errorf("tools section is missing %q", want)
		}
	}
}

func TestBuildTaskContext_Linear(t *testing.T) {
	task := domain.Task{
		Title:          "ENG-7 assigned",
		EventType:      domain.EventLinearIssueAssigned,
		EntitySource:   "linear",
		EntitySourceID: "ENG-7",
	}
	got := BuildTaskContext(task, "", "", nil)
	if !strings.Contains(got, "- Issue: ENG-7") {
		t.Errorf("expected the issue line;\n%s", got)
	}
	for _, absent := range []string{"- Repository:", "- Pull request:", "- Project:"} {
		if strings.Contains(got, absent) {
			t.Errorf("Linear block must not contain %q;\n%s", absent, got)
		}
	}
}

// TestRenderBranchTemplate_LinearIdentifierIsTheTicketID: a Linear issue's
// identifier is the ticket id a branch convention names, as a Jira key is.
func TestRenderBranchTemplate_LinearIdentifierIsTheTicketID(t *testing.T) {
	task := domain.Task{EntitySource: "linear", EntitySourceID: "ENG-7"}
	if got := renderBranchTemplate("aa/<ticket-id>", task); got != "aa/ENG-7" {
		t.Errorf("renderBranchTemplate = %q, want aa/ENG-7", got)
	}
}
