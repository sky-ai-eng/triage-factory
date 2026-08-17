package delegate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func res(body string) *agentproc.Result { return &agentproc.Result{Result: body} }

// --- Position-gated contract injection ------------------------------------

// nonterminalStepSysPrompt is the one position bit that reaches the agent:
// the non-terminal fragment (offering `continue`) is injected iff the step is
// not the last. The terminal step — and therefore the whole N=1 case — gets
// nothing and completes via the default `finish`.
func TestNonterminalStepSysPrompt_PositionGated(t *testing.T) {
	// N=1: the single step is also the terminal step → no fragment.
	if got := nonterminalStepSysPrompt(0, 1); got != "" {
		t.Errorf("N=1 step got a non-empty fragment; the single-prompt case must see no `continue`")
	}
	// N=3: steps 0 and 1 are non-terminal → fragment; step 2 is terminal → none.
	if got := nonterminalStepSysPrompt(0, 3); got == "" {
		t.Errorf("non-terminal step 0/3 got no fragment")
	}
	if got := nonterminalStepSysPrompt(1, 3); got == "" {
		t.Errorf("non-terminal step 1/3 got no fragment")
	}
	if got := nonterminalStepSysPrompt(2, 3); got != "" {
		t.Errorf("terminal step 2/3 got a fragment; the last step must see no `continue`")
	}
}

// TestTerminalContractHasNoContinue pins the terminal/N=1 assembled prompt:
// the base completion envelope documents finish/abort and never offers
// `continue` (a non-terminal step's assembled prompt = this envelope PLUS the
// fragment below, which does). It also no longer mentions `yield` — that
// vocabulary was removed.
func TestTerminalContractHasNoContinue(t *testing.T) {
	env := agentprompt.Build(machinistSpec())
	if strings.Contains(env, "continue") {
		t.Errorf("terminal completion contract must not mention `continue`")
	}
	if strings.Contains(env, "yield") {
		t.Errorf("completion contract must not mention `yield` (removed)")
	}
	for _, want := range []string{`"finish"`, `"abort"`} {
		if !strings.Contains(env, want) {
			t.Errorf("terminal completion contract missing %s", want)
		}
	}
}

// TestNonterminalFragmentFramesContinueAsDefault pins the fragment: it adds
// `continue` framed as the default and `finish` as the explicit-criteria-only
// exception.
func TestNonterminalFragmentFramesContinueAsDefault(t *testing.T) {
	frag := agentprompt.NonTerminalCompletion(machinistSpec())
	if !strings.Contains(frag, "continue") {
		t.Fatal("non-terminal fragment must offer `continue`")
	}
	if !strings.Contains(frag, "DEFAULT") {
		t.Errorf("non-terminal fragment must frame `continue` as the default")
	}
	if !strings.Contains(frag, "finish") {
		t.Errorf("non-terminal fragment must describe the `finish` exception")
	}
	if strings.Contains(frag, "yield") {
		t.Errorf("non-terminal fragment must not mention `yield` (removed)")
	}
}

// --- Terminal disposition (processCompletion) -----------------------------
//
// By the time a result reaches processCompletion it is a conclusion (or an
// IsError result) — the live driver owns the concluded-vs-open classification
// and the invalid-envelope re-prompt. These tests feed processCompletion
// directly (the one-shot / resume shape) to pin how it records each outcome.

func loadRun(t *testing.T, s *Spawner, conversationID string) *domain.Conversation {
	t.Helper()
	r, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil || r == nil {
		t.Fatalf("load run %s: err=%v", conversationID, err)
	}
	return r
}

func loadTask(t *testing.T, s *Spawner, taskID string) domain.Task {
	t.Helper()
	tk, err := s.tasks.GetSystem(context.Background(), runmode.LocalDefaultOrgID, taskID)
	if err != nil || tk == nil {
		t.Fatalf("load task %s: err=%v", taskID, err)
	}
	return *tk
}

// TestProcessCompletion_FinishRecordsOutcome: a step emitting finish persists
// outcome=finish + status=completed. processCompletion does not close the task
// — task disposition is the orchestrator's (terminateBlueprint).
func TestProcessCompletion_FinishRecordsOutcome(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "finish")
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForRun(t, database, conversationID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, "", task,
		res(`{"outcome":"finish","summary":"shipped it"}`), cwd, nil, "", "event", "")

	conv := loadRun(t, s, conversationID)
	if conv.Status != "completed" {
		t.Errorf("run.status = %q, want completed", conv.Status)
	}
	if conv.Outcome != "finish" {
		t.Errorf("run.outcome = %q, want finish", conv.Outcome)
	}
	if got := readTaskStatus(t, database, taskID); got == "done" {
		t.Errorf("task.status = %q; processCompletion must not close the task (terminateBlueprint does)", got)
	}
}

// TestProcessCompletion_AbortLeavesTaskOpen: an abort persists outcome=abort +
// the reason in outcome_reason (distinct from result_summary) and never closes
// the task.
func TestProcessCompletion_AbortLeavesTaskOpen(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "abort")
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForRun(t, database, conversationID)
	task := loadTask(t, s, taskID)
	before := readTaskStatus(t, database, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, "", task,
		res(`{"outcome":"abort","summary":"investigated the failure","reason":"needs a human to rotate the token"}`),
		cwd, nil, "", "event", "")

	conv := loadRun(t, s, conversationID)
	if conv.Status != "completed" {
		t.Errorf("run.status = %q, want completed (abort is run-completed, task-open)", conv.Status)
	}
	if conv.Outcome != "abort" {
		t.Errorf("run.outcome = %q, want abort", conv.Outcome)
	}
	if conv.OutcomeReason != "needs a human to rotate the token" {
		t.Errorf("run.outcome_reason = %q", conv.OutcomeReason)
	}
	if conv.ResultSummary != "investigated the failure" {
		t.Errorf("run.result_summary = %q; reason must stay distinct from summary", conv.ResultSummary)
	}
	if got := readTaskStatus(t, database, taskID); got == "done" {
		t.Errorf("task.status = %q (was %q); abort must leave the task open", got, before)
	}
}

// TestProcessCompletion_AbortMissingReasonFails: abort is the agent's voluntary
// stop, so the reason is its required companion — an abort without one is an
// invalid envelope, not a valid conclusion. The live driver re-prompts it; a
// non-live path (resume / one-shot) that can't lands here, where it's a knowable
// error → failed (with no recorded outcome), never a clean NULL-outcome
// completion the orchestrator would read as finish.
func TestProcessCompletion_AbortMissingReasonFails(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "abort-noreason")
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForRun(t, database, conversationID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, "", task,
		res(`{"outcome":"abort","summary":"stopped, couldn't proceed"}`), cwd, nil, "", "event", "")

	conv := loadRun(t, s, conversationID)
	if conv.Status != "failed" {
		t.Errorf("run.status = %q, want failed (an abort with no reason is an invalid envelope)", conv.Status)
	}
	if conv.Outcome != "" {
		t.Errorf("run.outcome = %q, want empty (an abort with no reason is not a valid conclusion)", conv.Outcome)
	}
}

// TestProcessCompletion_NoConclusionParksOpen: a turn that ends with no
// envelope at all (prose) is not a termination — the run is parked `open` (not
// recorded as a NULL-outcome completion, which would let the orchestrator close
// a final step). This is the disposition for a one-shot/resume turn that ends
// without concluding (the live driver consumes this in its loop instead).
func TestProcessCompletion_NoConclusionParksOpen(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "open-noconcl")
	bpr := blueprintRunIDForRun(t, database, conversationID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	parked, _ := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, "", task,
		res(`this is not a JSON envelope`), cwd, nil, "", "event", "")

	if !parked {
		t.Error("processCompletion(no-conclusion) = false; want true (open, not terminal)")
	}
	conv := loadRun(t, s, conversationID)
	if conv.Status != "open" {
		t.Errorf("run.status = %q, want open (a no-conclusion turn parks open, not completed)", conv.Status)
	}
	if conv.Outcome != "" {
		t.Errorf("run.outcome = %q, want \"\" (no conclusion was recorded)", conv.Outcome)
	}
}

// TestProcessCompletion_InvalidEnvelopeFails: an envelope attempt that never
// validated (here a `finish` with no summary) is a knowable error — recorded
// failed, not a NULL-outcome completion. The live driver re-prompts this in
// place; when a backend can't or the bound is exhausted, the unfixed result
// lands here and fails (TestDriveLiveRun_InvalidRepromptsToBoundThenHandsBack).
func TestProcessCompletion_InvalidEnvelopeFails(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "invalid-fails")
	bpr := blueprintRunIDForRun(t, database, conversationID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	parked, _ := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, "", task,
		res(`{"outcome":"finish"}`), cwd, nil, "", "event", "")

	if parked {
		t.Error("processCompletion(invalid) = true; want false (terminal failure, not parked)")
	}
	conv := loadRun(t, s, conversationID)
	if conv.Status != "failed" {
		t.Errorf("run.status = %q, want failed (an invalid envelope is a knowable error)", conv.Status)
	}
	if conv.Outcome != "" {
		t.Errorf("run.outcome = %q, want \"\" (an invalid attempt records no outcome)", conv.Outcome)
	}
}

// TestProcessCompletion_FinishReturnsNotParked: a terminal finish returns
// parked=false so runAgent's cleanup defers tear the worktree down. A completed
// run never parks anymore — only an idle-hibernated `open` turn does,
// covered elsewhere.
func TestProcessCompletion_FinishReturnsNotParked(t *testing.T) {
	s, _, conversationID, taskID := setupAdvanceFixture(t, "pc-finish")
	if parked, _ := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, "", "",
		loadTask(t, s, taskID), res(`{"outcome":"finish","summary":"done"}`),
		t.TempDir(), nil, "", "event", ""); parked {
		t.Error("processCompletion(finish) = true; want false (terminal, not parked)")
	}
}

// TestTerminateBlueprint_CompletedClosesTask pins the orchestrator-owned close:
// finalizing a blueprint_run as completed closes its task (done +
// close_reason=run_completed).
func TestTerminateBlueprint_CompletedClosesTask(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "terminate-done")
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForRun(t, database, conversationID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "manual", runmode.LocalDefaultUserID,
		time.Now(), runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusCompleted, "", nil, true)

	if got := readTaskStatus(t, database, taskID); got != "done" {
		t.Errorf("task.status = %q, want done (terminateBlueprint closes on completed)", got)
	}
	var closeReason string
	if err := database.QueryRow(`SELECT COALESCE(close_reason, '') FROM tasks WHERE id = ?`, taskID).Scan(&closeReason); err != nil {
		t.Fatalf("scan close_reason: %v", err)
	}
	if closeReason != "run_completed" {
		t.Errorf("close_reason = %q, want run_completed", closeReason)
	}
}

// TestTerminateBlueprint_AbortLeavesTaskOpen: a non-completed terminal (abort)
// leaves the task open for human attention.
func TestTerminateBlueprint_AbortLeavesTaskOpen(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "terminate-abort")
	stampBotClaim(t, database, taskID)
	if _, err := database.Exec(`UPDATE tasks SET status = 'in_progress' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("park: %v", err)
	}
	bpr := blueprintRunIDForRun(t, database, conversationID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "manual", runmode.LocalDefaultUserID,
		time.Now(), runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusAborted, "needs a human", nil, true)

	if got := readTaskStatus(t, database, taskID); got == "done" {
		t.Errorf("task.status = %q; abort must leave the task open", got)
	}
}
