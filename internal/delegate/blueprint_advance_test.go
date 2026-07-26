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

// seedDraftPRArtifact records a draft pull_request artifact for runID, so the
// run looks (to processCompletion's park check) like it queued a PR for human
// approval — the GitHub-native successor to seeding a pending_prs row.
func seedDraftPRArtifact(t *testing.T, s *Spawner, runID string) {
	t.Helper()
	a := domain.NewPullRequestArtifact("o/r", 1, "node", "h", "main",
		"https://github.com/o/r/pull/1", "queued PR", "", true)
	a.ConversationID = runID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	if _, err := s.artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a); err != nil {
		t.Fatalf("seed draft PR artifact: %v", err)
	}
}

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

		// "yield" is no longer a recognized outcome, so a stray "yield"
		// persisted on a completed step is just an unknown token:
		// the default branch refuses to act on it — abort, never finish.
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

// --- Draft-PR sidecar never parks the step ----------------------

// TestProcessCompletion_BlueprintStepDraftPRDoesNotPark pins the post-park-removal
// behavior: a non-final blueprint step that emits continue AND queued a draft PR
// is NOT coerced and does NOT park — the artifact is an async sidecar, so the
// step completes with its real outcome (continue) and the orchestrator advances
// to the next step. (Previously this was coerced continue→finish and flipped to
// pending_approval, freezing the blueprint.)
func TestProcessCompletion_BlueprintStepDraftPRDoesNotPark(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "bp-nopark")
	makeRunBlueprintStep(t, database, runID, taskID)
	// Queue a draft PR — the legacy "pending external action" — and confirm it no
	// longer parks the step.
	seedDraftPRArtifact(t, s, runID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, runID, "bpr-"+runID, task,
		res(`{"outcome":"continue","summary":"opened a PR"}`), cwd, nil, "", "event", "")

	run := loadRun(t, s, runID)
	if run.Outcome != "continue" {
		t.Errorf("run.outcome = %q, want continue (a queued draft PR no longer coerces the outcome)", run.Outcome)
	}
	if run.Status != "completed" {
		t.Errorf("run.status = %q, want completed (a draft PR is a sidecar; the step never parks)", run.Status)
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

	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, runID, "bpr-"+runID, task,
		res(`{"outcome":"continue","summary":"did step work"}`), cwd, nil, "", "event", "")

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
// side: processCompletion ingests the agent's memory file from the one fixed
// path (_tfac/memory.md) and stamps the run's blueprint_run_id onto the
// conversation_memory row, so the next step's materializer files it as this
// workflow run's own handoff rather than as history.
func TestProcessCompletion_BlueprintStepWritesNamespacedMemoryRow(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "bp-memrow")
	makeRunBlueprintStep(t, database, runID, taskID) // sets blueprint_run_id = "bpr-<runID>"
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	// Stage the agent's memory file at the one path the contract names.
	blueprintRunID := "bpr-" + runID
	memDir := filepath.Join(cwd, "_tfac")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "memory.md"), []byte("step did X; next step needs Y"), 0o644); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	// No session id → the gate can't (and needn't) retry; the staged file plus
	// a valid continue outcome already satisfy it.
	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, runID, "bpr-"+runID, task,
		res(`{"outcome":"continue","summary":"did step work"}`), cwd, nil, "", "event", "")

	// Raw column read rather than a store method: this test is about
	// processCompletion's write side (file ingestion + blueprint_run_id
	// stamping), not the entity-scoped read path, and production's write
	// side (UpsertAgentMemorySystem) does not itself join an entity in
	// conversation_memory_entities — that lands with the run-end attach ticket.
	var agentContent sql.NullString
	var gotBlueprintRunID sql.NullString
	if err := database.QueryRow(
		`SELECT agent_content, blueprint_run_id FROM conversation_memory WHERE conversation_id = ?`, runID,
	).Scan(&agentContent, &gotBlueprintRunID); err != nil {
		t.Fatalf("scan conversation_memory: %v", err)
	}
	if agentContent.String != "step did X; next step needs Y" {
		t.Errorf("agent_content = %q; processCompletion should ingest the file from _tfac/memory.md", agentContent.String)
	}
	if gotBlueprintRunID.String != blueprintRunID {
		t.Errorf("conversation_memory.blueprint_run_id = %q, want %q", gotBlueprintRunID.String, blueprintRunID)
	}
}

// TestTerminateBlueprint_CompletedWithUnresolvedArtifactLeavesTaskOpen pins the
// terminal-on-last gate: a blueprint that completes while a draft PR / ready
// review is still unresolved does NOT close its task — it leaves it open and
// surfaced in the derived approval column (in_review). The last artifact
// resolution closes it.
func TestTerminateBlueprint_CompletedWithUnresolvedArtifactLeavesTaskOpen(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "term-unresolved")
	stampBotClaim(t, database, taskID)
	makeRunBlueprintStep(t, database, runID, taskID)
	blueprintRunID := "bpr-" + runID
	// The step completed and left a draft PR unresolved.
	setRunStatus(t, database, runID, "completed")
	seedDraftPRArtifact(t, s, runID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, blueprintRunID, taskID, "event", "",
		loadRun(t, s, runID).StartedAt, runConfig{orgID: runmode.LocalDefaultOrgID},
		domain.BlueprintRunStatusCompleted, "", nil, true)

	if got := readTaskStatus(t, database, taskID); got != "in_review" {
		t.Errorf("task.status = %q, want in_review (completed with an unresolved artifact leaves the task open in the approval column)", got)
	}
}

// TestTerminateBlueprint_CompletedWithNoUnresolvedArtifactClosesTask is the
// contrast: a blueprint that completes with no unresolved artifact closes the
// task as before.
func TestTerminateBlueprint_CompletedWithNoUnresolvedArtifactClosesTask(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "term-clean")
	stampBotClaim(t, database, taskID)
	makeRunBlueprintStep(t, database, runID, taskID)
	blueprintRunID := "bpr-" + runID
	setRunStatus(t, database, runID, "completed")

	s.terminateBlueprint(runmode.LocalDefaultOrgID, blueprintRunID, taskID, "event", "",
		loadRun(t, s, runID).StartedAt, runConfig{orgID: runmode.LocalDefaultOrgID},
		domain.BlueprintRunStatusCompleted, "", nil, true)

	if got := readTaskStatus(t, database, taskID); got != "done" {
		t.Errorf("task.status = %q, want done (a clean completion with no unresolved artifact closes the task)", got)
	}
}

// makeRunBlueprintStep turns the fixture's run into a blueprint step: it seeds
// a blueprint + blueprint_runs row and points the run at it so
// processCompletion's isBlueprintStep branch trips. Mirrors the setup the
// advance-task chain-step guard test uses.
func makeRunBlueprintStep(t *testing.T, database *sql.DB, runID, taskID string) {
	t.Helper()
	if err := sqlitestore.New(database).Blueprints.Create(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: "bp-" + runID, Name: "bp-" + runID, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, worktree_path, step_plan)
		 VALUES (?, ?, ?, 'manual', ?, '[]')`,
		"bpr-"+runID, "bp-"+runID, taskID, "/tmp/wt-"+runID,
	); err != nil {
		t.Fatalf("seed blueprint_runs: %v", err)
	}
	if _, err := database.Exec(`UPDATE conversations SET blueprint_run_id = ? WHERE id = ?`, "bpr-"+runID, runID); err != nil {
		t.Fatalf("set blueprint_run_id: %v", err)
	}
}
