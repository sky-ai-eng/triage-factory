package delegate

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// redelegate settles the fixture's blueprint_run and mints a second one on the
// same task — a re-delegation, the shape this file is about. It returns the new
// run, which carries no worktree_path of its own: the tree belongs to the task,
// and run B has not claimed anything yet.
func (f stepFixture) redelegate(t *testing.T, suffix string) *domain.BlueprintRun {
	t.Helper()
	finishBlueprint(t, f.database, f.brID, "completed", len(f.conversationIDs)-1)
	created, err := f.s.blueprints.CreateRun(context.Background(), runmode.LocalDefaultOrgID, domain.BlueprintRun{
		ID: "bpr-" + suffix + "-b", BlueprintID: blueprintIDOfRun(t, f.database, f.brID), TaskID: f.task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		StepPlan: []domain.BlueprintPlanStep{{StepIndex: 0, PromptID: "p", PromptName: "p", PromptBody: "b", Source: "user"}},
	})
	if err != nil {
		t.Fatalf("mint the second blueprint_run: %v", err)
	}
	return &created
}

func blueprintIDOfRun(t *testing.T, database *sql.DB, blueprintRunID string) string {
	t.Helper()
	var bpID string
	if err := database.QueryRow(`SELECT blueprint_id FROM blueprint_runs WHERE id = ?`, blueprintRunID).Scan(&bpID); err != nil {
		t.Fatalf("read blueprint_id of %s: %v", blueprintRunID, err)
	}
	return bpID
}

// enqueueStepZero mints run B's first step through the production mint, which
// is what stamps the task's inherited worktree_path onto the new row.
func (f stepFixture) enqueueStepZero(t *testing.T, br *domain.BlueprintRun) string {
	t.Helper()
	before := conversationIDsForTask(t, f.database, f.task.ID)
	if err := f.s.enqueueBlueprintStep(context.Background(), runmode.LocalDefaultOrgID, br.ID, f.task,
		domain.BlueprintStep{BlueprintID: br.BlueprintID, StepIndex: 0, StepPromptID: stepPromptID(t, f.database, f.conversationIDs[0])},
		"claude-sonnet-4-6", "manual", "", runmode.LocalDefaultUserID, ""); err != nil {
		t.Fatalf("enqueueBlueprintStep: %v", err)
	}
	for id := range conversationIDsForTask(t, f.database, f.task.ID) {
		if !before[id] {
			return id
		}
	}
	t.Fatal("enqueueBlueprintStep wrote no conversation row")
	return ""
}

// stepPromptID reads the prompt a seeded step ran, so the mint under test
// writes a conversations.prompt_id its FK accepts.
func stepPromptID(t *testing.T, database *sql.DB, conversationID string) string {
	t.Helper()
	var promptID sql.NullString
	if err := database.QueryRow(`SELECT prompt_id FROM conversations WHERE id = ?`, conversationID).Scan(&promptID); err != nil {
		t.Fatalf("read prompt_id of %s: %v", conversationID, err)
	}
	return promptID.String
}

func conversationIDsForTask(t *testing.T, database *sql.DB, taskID string) map[string]bool {
	t.Helper()
	rows, err := database.Query(`SELECT id FROM conversations WHERE task_id = ?`, taskID)
	if err != nil {
		t.Fatalf("list the task's conversations: %v", err)
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan conversation id: %v", err)
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the task's conversations: %v", err)
	}
	return ids
}

// TestEnqueueBlueprintStep_StampsTheTasksWorktreePath is the mint half of
// keying the workspace by the task: a conversation opens in the tree the task's
// previous one left, and the row says so before any executor claims it.
//
// Without the stamp the row arrives with an empty path, the claim's warm stat
// has nothing to stat, and a tree sitting on disk is rehydrated from a blob
// anyway — or, when no blob exists, cloned over.
func TestEnqueueBlueprintStep_StampsTheTasksWorktreePath(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	wt := t.TempDir()
	f := seedStepFixture(t, "github", "mint-stamp", 1, wt)

	next := f.enqueueStepZero(t, f.redelegate(t, "mint-stamp"))

	if got := storedWorktreePath(t, f.database, next); got != wt {
		t.Errorf("the minted conversation's worktree_path = %q, want the task's %q", got, wt)
	}
}

// TestEnqueueBlueprintStep_FirstConversationOnATaskInheritsNothing is the other
// half: "" is an answer, not a failure. A task nothing has ever run on has no
// tree to name, and the claim that picks this row up is the one that clones.
func TestEnqueueBlueprintStep_FirstConversationOnATaskInheritsNothing(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	f := seedStepFixture(t, "github", "mint-first", 1, "")
	if _, err := f.database.Exec(`UPDATE conversations SET worktree_path = NULL WHERE task_id = ?`, f.task.ID); err != nil {
		t.Fatalf("clear the fixture's paths: %v", err)
	}

	next := f.enqueueStepZero(t, f.redelegate(t, "mint-first"))

	if got := storedWorktreePath(t, f.database, next); got != "" {
		t.Errorf("worktree_path on a task with no tree = %q, want empty", got)
	}
}

// TestBuildStepConfig_RedelegationStartsInThePriorRunsTree is the claim half,
// warm: run B's first step opens in the directory run A was working in, and is
// told the tree is warm.
//
// This is what keying the workspace by the blueprint run could not do. Run B's
// blueprint_runs row carries no worktree_path — it has claimed nothing — so the
// old first-claim arm read that as "no workspace" and cloned a fresh checkout
// onto the base branch, beside the branch run A had already pushed.
func TestBuildStepConfig_RedelegationStartsInThePriorRunsTree(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	wt := t.TempDir()
	f := seedStepFixture(t, "jira", "redelegate-warm", 1, wt)

	runB := f.redelegate(t, "redelegate-warm")
	next := f.enqueueStepZero(t, runB)
	if runB.WorktreePath != "" {
		t.Fatalf("the second run carries worktree_path %q; the fixture is not staging a re-delegation", runB.WorktreePath)
	}

	cfg, err := f.s.buildStepConfig(context.Background(), runmode.LocalDefaultOrgID, runB, f.task,
		domain.Conversation{ID: next, TaskID: f.task.ID, WorktreePath: wt, BlueprintRunID: runB.ID}, nil, nil)
	if err != nil {
		t.Fatalf("buildStepConfig for run B step 0: %v", err)
	}
	if cfg.wtPath != wt {
		t.Errorf("run B step 0 resolved wtPath = %q, want run A's tree %q", cfg.wtPath, wt)
	}
	if cfg.workspace != domain.WorkspaceProvenanceWarm {
		t.Errorf("workspace provenance = %q, want warm — a tree still on disk is reused, not rebuilt", cfg.workspace)
	}
	// The run row is stamped even though it did not build the tree: the
	// blueprint's terminal cleanup and the cancel finalizers read it to find
	// the directory they must remove.
	if got := runWorktreePath(t, f.database, runB.ID); got != wt {
		t.Errorf("blueprint_runs.worktree_path after run B's first claim = %q, want %q", got, wt)
	}
}

// TestBuildStepConfig_RedelegationRehydratesRatherThanCloning is the same claim
// against a tree that is gone: the task's snapshot is the durable copy, so run
// B's first step rebuilds from it rather than starting over.
//
// The provenance is the assertion that matters. `rehydrated` is reachable only
// through the ladder's blob rung — neither the fresh-clone arm nor the ladder's
// own last rung (freshStepWorkspace) can produce it — so reading it back proves
// nothing cloned.
func TestBuildStepConfig_RedelegationRehydratesRatherThanCloning(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	f := seedStepFixture(t, "jira", "redelegate-cold", 1, "")

	// Run A's tree, captured and then lost — a blueprint terminal removed the
	// directory, or the host it lived on went away.
	rebuilt := worktree.RunRoot(f.task.ID)
	t.Cleanup(func() { _ = os.RemoveAll(rebuilt) })
	f.stampSharedWorktree(t, rebuilt)
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	f.s.SetStorage(blobs)
	src := filepath.Join(t.TempDir(), "workspace")
	writeFile(t, filepath.Join(src, "_tfac", "notes.txt"), "run A got this far")
	if err := f.s.snapshotWorkspace(ctx, org, f.conversationIDs[0], f.task.ID, "", src, "", domain.ConversationRuntimeSDK); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}
	if err := os.RemoveAll(rebuilt); err != nil {
		t.Fatalf("lose run A's tree: %v", err)
	}

	runB := f.redelegate(t, "redelegate-cold")
	next := f.enqueueStepZero(t, runB)

	cfg, err := f.s.buildStepConfig(ctx, org, runB, f.task,
		domain.Conversation{ID: next, TaskID: f.task.ID, WorktreePath: rebuilt, BlueprintRunID: runB.ID}, nil, nil)
	if err != nil {
		t.Fatalf("buildStepConfig for run B step 0: %v", err)
	}
	if cfg.workspace != domain.WorkspaceProvenanceRehydrated {
		t.Fatalf("workspace provenance = %q, want rehydrated — a re-delegation must not clone over the task's snapshot", cfg.workspace)
	}
	if cfg.wtPath != rebuilt {
		t.Errorf("rehydrated cwd = %q, want the task's run root %q", cfg.wtPath, rebuilt)
	}
	assertFileContains(t, filepath.Join(cfg.wtPath, "_tfac", "notes.txt"), "run A got this far")
}

// TestBuildStepConfig_FirstEverConversationOnATaskBuildsFresh is the negative
// the two above need: the fresh arm is still reachable, and it is reachable on
// exactly the condition that names it — a task with no tree, no run path, and
// no snapshot.
func TestBuildStepConfig_FirstEverConversationOnATaskBuildsFresh(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	f := seedStepFixture(t, "slack", "first-ever", 1, "")
	t.Cleanup(func() { worktree.RemoveRunRoot(f.task.ID) })
	if _, err := f.database.Exec(`UPDATE conversations SET worktree_path = NULL WHERE task_id = ?`, f.task.ID); err != nil {
		t.Fatalf("clear the fixture's paths: %v", err)
	}

	br := f.blueprintRun(t)
	cfg, err := f.s.buildStepConfig(context.Background(), runmode.LocalDefaultOrgID, br, f.task,
		domain.Conversation{ID: f.conversationIDs[0], TaskID: f.task.ID, BlueprintRunID: br.ID}, nil, nil)
	if err != nil {
		t.Fatalf("buildStepConfig for the first claim: %v", err)
	}
	if cfg.workspace != domain.WorkspaceProvenanceFresh {
		t.Errorf("workspace provenance = %q, want fresh — a task with no workspace anywhere has one built", cfg.workspace)
	}
	if cfg.wtPath != worktree.RunRoot(f.task.ID) {
		t.Errorf("fresh wtPath = %q, want the task's run root %q", cfg.wtPath, worktree.RunRoot(f.task.ID))
	}
}

// TestBuildStepConfig_RedelegationAfterAFailedRunWithNoBlobBuildsFresh is the
// shape the retention change creates and the one a stale path could strand: a
// task whose only run failed before it ever parked. The failure wrote no
// snapshot, and the blueprint's terminal swept the tree, so every record the
// task carries names a directory that is not there.
//
// The next delegation clones. It must not be handed the ladder's
// workspace-expired refusal instead — that answer is for a conversation whose
// continuity lived in a blob that is gone, and a first step of a new run has no
// continuity to lose. A recorded path is evidence of a workspace only while the
// directory it names exists.
func TestBuildStepConfig_RedelegationAfterAFailedRunWithNoBlobBuildsFresh(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	swept := filepath.Join(t.TempDir(), "gone")
	f := seedStepFixture(t, "slack", "redelegate-swept", 1, swept)
	t.Cleanup(func() { worktree.RemoveRunRoot(f.task.ID) })
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	f.s.SetStorage(blobs)

	runB := f.redelegate(t, "redelegate-swept")
	next := f.enqueueStepZero(t, runB)
	if got := storedWorktreePath(t, f.database, next); got != swept {
		t.Fatalf("the mint stamped %q; the fixture is not staging a swept tree (%q)", got, swept)
	}

	cfg, err := f.s.buildStepConfig(context.Background(), runmode.LocalDefaultOrgID, runB, f.task,
		domain.Conversation{ID: next, TaskID: f.task.ID, WorktreePath: swept, BlueprintRunID: runB.ID}, nil, nil)
	if err != nil {
		t.Fatalf("buildStepConfig for a re-delegation with nothing to rehydrate: %v", err)
	}
	if cfg.workspace != domain.WorkspaceProvenanceFresh {
		t.Errorf("workspace provenance = %q, want fresh", cfg.workspace)
	}
	if cfg.wtPath != worktree.RunRoot(f.task.ID) {
		t.Errorf("fresh wtPath = %q, want the task's run root %q", cfg.wtPath, worktree.RunRoot(f.task.ID))
	}
}

func runWorktreePath(t *testing.T, database *sql.DB, blueprintRunID string) string {
	t.Helper()
	var path sql.NullString
	if err := database.QueryRow(`SELECT worktree_path FROM blueprint_runs WHERE id = ?`, blueprintRunID).Scan(&path); err != nil {
		t.Fatalf("read blueprint_runs.worktree_path for %s: %v", blueprintRunID, err)
	}
	return path.String
}
