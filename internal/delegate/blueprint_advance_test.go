package delegate

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// --- decideBlueprintStep: the advancement decision matrix ------------------

// TestDecideBlueprintStep pins the position-gated mapping from a completed
// step's runs.outcome to the orchestrator's next move — the logic that
// replaced the chain-verdict switch. It is the single place advancement /
// finish / abort flow through runs.outcome, so the whole matrix is covered
// here without spawning an agent.
func TestDecideBlueprintStep(t *testing.T) {
	cases := []struct {
		name       string
		outcome    string
		isFinal    bool
		want       blueprintStepOutcome
		wantReason string
	}{
		// continue advances mid-blueprint; on the last step there is no next
		// step, so it resolves to a structural finish.
		{"continue non-final advances", "continue", false, blueprintStepAdvance, ""},
		{"continue final finishes", "continue", true, blueprintStepFinish, ""},

		// finish always ends the blueprint completed (early exit when non-final).
		{"finish non-final finishes", "finish", false, blueprintStepFinish, ""},
		{"finish final finishes", "finish", true, blueprintStepFinish, ""},

		// abort always leaves the task open; the reason comes from
		// runs.outcome_reason at the call site (empty synthetic reason here).
		{"abort non-final aborts", "abort", false, blueprintStepAbort, ""},
		{"abort final aborts", "abort", true, blueprintStepAbort, ""},

		// A missing outcome (empty === NULL) is the floor after the
		// outcome gate exhausted its retries: ambiguous mid-blueprint →
		// abort no-outcome; unambiguous on the final step → finish.
		{"missing non-final aborts no-outcome", "", false, blueprintStepAbort, "no-outcome"},
		{"missing final finishes", "", true, blueprintStepFinish, ""},

		// An unrecognized/garbage outcome (a future or buggy value) must
		// never close a task on finish — it aborts regardless of position,
		// so we never act on a value we can't interpret.
		{"unknown non-final aborts", "bogus", false, blueprintStepAbort, "unknown-outcome: bogus"},
		{"unknown final aborts", "bogus", true, blueprintStepAbort, "unknown-outcome: bogus"},

		// yield is a valid RunOutcome but never reaches a completed step (it
		// parks the run in awaiting_input before completing). Pin that even if
		// a buggy write ever persisted it on a completed step, the default
		// branch refuses to act on it — abort, never finish — on either side.
		{"yield on completed step aborts (non-final)", "yield", false, blueprintStepAbort, "unknown-outcome: yield"},
		{"yield on completed step aborts (final)", "yield", true, blueprintStepAbort, "unknown-outcome: yield"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := decideBlueprintStep(tc.outcome, tc.isFinal)
			if got != tc.want {
				t.Errorf("decideBlueprintStep(%q, final=%v) decision = %v, want %v", tc.outcome, tc.isFinal, got, tc.want)
			}
			if reason != tc.wantReason {
				t.Errorf("decideBlueprintStep(%q, final=%v) reason = %q, want %q", tc.outcome, tc.isFinal, reason, tc.wantReason)
			}
		})
	}
}

// --- Multi-step external-action coercion -----------------------------------

// TestProcessCompletion_BlueprintStepExternalActionCoercesToFinish pins the
// run.go coercion: a non-final blueprint step that emits continue but queued a
// terminal external action (a PR awaiting human approval) is coerced
// continue→finish before flipping to pending_approval, so the post-approval
// resume terminates the blueprint completed. This is the successor to the
// synthetic --final verdict the chain path inserted.
func TestProcessCompletion_BlueprintStepExternalActionCoercesToFinish(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "bp-coerce")
	makeRunBlueprintStep(t, database, runID, taskID)
	// Queue a PR for human approval so the run has a pending external action.
	if err := s.pendingPRs.CreateSystem(context.Background(), runmode.LocalDefaultOrg, domain.PendingPR{
		ID: "pp-" + runID, RunID: runID, Owner: "o", Repo: "r",
		HeadBranch: "h", HeadSHA: "sha", BaseBranch: "main", Title: "queued PR",
	}); err != nil {
		t.Fatalf("seed pending PR: %v", err)
	}
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrg, runID, "bpr-"+runID, task,
		res(`{"outcome":"continue","summary":"opened a PR"}`), cwd, "", "claude-sonnet-4-6", "owner", "repo", "event", "", "")

	run := loadRun(t, s, runID)
	if run.Outcome != "finish" {
		t.Errorf("run.outcome = %q, want finish (a blueprint step that queued a PR is coerced continue→finish)", run.Outcome)
	}
	if run.Status != "pending_approval" {
		t.Errorf("run.status = %q, want pending_approval", run.Status)
	}
}

// TestProcessCompletion_BlueprintStepContinueNoPendingStaysContinue is the
// contrast: a blueprint step that emits continue without queuing any external
// action keeps continue (no coercion) and completes normally, leaving the
// orchestrator to advance to the next step.
func TestProcessCompletion_BlueprintStepContinueNoPendingStaysContinue(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "bp-continue")
	makeRunBlueprintStep(t, database, runID, taskID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrg, runID, "bpr-"+runID, task,
		res(`{"outcome":"continue","summary":"did step work"}`), cwd, "", "claude-sonnet-4-6", "owner", "repo", "event", "", "")

	run := loadRun(t, s, runID)
	if run.Outcome != "continue" {
		t.Errorf("run.outcome = %q, want continue (no pending action → no coercion)", run.Outcome)
	}
	if run.Status != "completed" {
		t.Errorf("run.status = %q, want completed", run.Status)
	}
	// The blueprint orchestrator owns task lifecycle — a mid-blueprint step
	// completion must not close the task.
	if got := readTaskStatus(t, database, taskID); got == "done" {
		t.Errorf("task.status = done; a blueprint step completion must not close the task")
	}
}

// TestProcessCompletion_BlueprintStepWritesNamespacedMemoryRow pins the write
// side of the namespacing: processCompletion ingests the agent's memory file
// from _scratch/entity-memory/<blueprint_run_id>/<run_id>.md (not the old
// top-level path) and stamps the run's blueprint_run_id onto the run_memory
// row, so the next step's materializer folders it correctly.
func TestProcessCompletion_BlueprintStepWritesNamespacedMemoryRow(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "bp-memrow")
	makeRunBlueprintStep(t, database, runID, taskID) // sets blueprint_run_id = "bpr-<runID>"
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	// Stage the agent's memory file at the namespaced path the contract
	// dictates. The blueprint_run_id is the namespace folder.
	blueprintRunID := "bpr-" + runID
	memDir := filepath.Join(cwd, "_scratch", "entity-memory", blueprintRunID)
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(memDir, runID+".md"), []byte("step did X; next step needs Y"), 0o644); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	// No session id → the gate can't (and needn't) retry; the staged file plus
	// a valid continue outcome already satisfy it.
	s.processCompletion(context.Background(), runmode.LocalDefaultOrg, runID, "bpr-"+runID, task,
		res(`{"outcome":"continue","summary":"did step work"}`), cwd, "", "claude-sonnet-4-6", "owner", "repo", "event", "", "")

	mem, err := sqlitestore.New(database).TaskMemory.GetRunMemory(context.Background(), runmode.LocalDefaultOrg, runID)
	if err != nil || mem == nil {
		t.Fatalf("GetRunMemory: mem=%v err=%v", mem, err)
	}
	if mem.Content != "step did X; next step needs Y" {
		t.Errorf("agent_content = %q; processCompletion should ingest the file from the namespaced path", mem.Content)
	}
	if mem.BlueprintRunID != blueprintRunID {
		t.Errorf("run_memory.blueprint_run_id = %q, want %q", mem.BlueprintRunID, blueprintRunID)
	}
}

// makeRunBlueprintStep turns the fixture's run into a blueprint step: it seeds
// a blueprint + blueprint_runs row and points the run at it so
// processCompletion's isBlueprintStep branch trips. Mirrors the setup the
// advance-task chain-step guard test uses.
func makeRunBlueprintStep(t *testing.T, database *sql.DB, runID, taskID string) {
	t.Helper()
	if err := sqlitestore.New(database).Blueprints.Create(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: "bp-" + runID, Name: "bp-" + runID, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, worktree_path)
		 VALUES (?, ?, ?, 'manual', ?)`,
		"bpr-"+runID, "bp-"+runID, taskID, "/tmp/wt-"+runID,
	); err != nil {
		t.Fatalf("seed blueprint_runs: %v", err)
	}
	if _, err := database.Exec(`UPDATE runs SET blueprint_run_id = ? WHERE id = ?`, "bpr-"+runID, runID); err != nil {
		t.Fatalf("set blueprint_run_id: %v", err)
	}
}
