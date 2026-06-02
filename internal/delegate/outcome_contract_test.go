package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

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
// the base completion envelope documents finish/abort/yield and never offers
// `continue`. (A non-terminal step's assembled prompt = this envelope PLUS the
// fragment below, which does.)
func TestTerminalContractHasNoContinue(t *testing.T) {
	env := ai.EnvelopeTemplate
	if strings.Contains(env, "continue") {
		t.Errorf("terminal completion contract must not mention `continue`")
	}
	for _, want := range []string{`"finish"`, `"abort"`, `"yield"`} {
		if !strings.Contains(env, want) {
			t.Errorf("terminal completion contract missing %s", want)
		}
	}
}

// TestNonterminalFragmentFramesContinueAsDefault pins the fragment: it adds
// `continue` framed as the default and `finish` as the explicit-criteria-only
// exception.
func TestNonterminalFragmentFramesContinueAsDefault(t *testing.T) {
	frag := blueprintStepNonterminalPrompt
	if !strings.Contains(frag, "continue") {
		t.Fatal("non-terminal fragment must offer `continue`")
	}
	if !strings.Contains(frag, "DEFAULT") {
		t.Errorf("non-terminal fragment must frame `continue` as the default")
	}
	if !strings.Contains(frag, "finish") {
		t.Errorf("non-terminal fragment must describe the `finish` exception")
	}
}

// --- Outcome-validity retry loop ------------------------------------------

func res(body string) *agentproc.Result { return &agentproc.Result{Result: body} }

// TestOutcomeRetryLoop_ValidInitialSkipsRetry: a completion that already
// carries a valid outcome is returned untouched and never resumes.
func TestOutcomeRetryLoop_ValidInitialSkipsRetry(t *testing.T) {
	calls := 0
	resume := func() (*agentproc.Result, error) { calls++; return res(`{"outcome":"finish","summary":"ok"}`), nil }
	got := runOutcomeRetryLoop("r1", res(`{"outcome":"finish","summary":"done"}`), resume, maxOutcomeRetries)
	if calls != 0 {
		t.Errorf("resume called %d times for a valid initial envelope; want 0", calls)
	}
	if !completionHasValidOutcome(got) {
		t.Errorf("valid initial envelope should pass through")
	}
}

// TestOutcomeRetryLoop_AcceptsValidOnRetry2 pins the acceptance: an
// unparseable envelope re-prompts, and a valid outcome on the second attempt
// is accepted (no further retries).
func TestOutcomeRetryLoop_AcceptsValidOnRetry2(t *testing.T) {
	calls := 0
	resume := func() (*agentproc.Result, error) {
		calls++
		if calls < 2 {
			return res(`not json`), nil // attempt 1 still invalid
		}
		return res(`{"outcome":"finish","summary":"recovered"}`), nil // attempt 2 valid
	}
	got := runOutcomeRetryLoop("r2", res(`{"summary":"no outcome"}`), resume, maxOutcomeRetries)
	if calls != 2 {
		t.Errorf("resume called %d times; want exactly 2 (valid on retry-2, then stop)", calls)
	}
	if !completionHasValidOutcome(got) {
		t.Errorf("valid outcome on retry-2 should be accepted")
	}
	if parsed := parseAgentResult(got.Result); parsed == nil || parsed.Outcome != "finish" {
		t.Errorf("merged result should carry the retry's finish outcome, got %+v", parsed)
	}
}

// TestOutcomeRetryLoop_ExhaustsThreeRetries: when no valid outcome ever
// arrives, the loop resumes exactly maxOutcomeRetries (3) times before giving
// up to the caller's fallback.
func TestOutcomeRetryLoop_ExhaustsThreeRetries(t *testing.T) {
	calls := 0
	resume := func() (*agentproc.Result, error) { calls++; return res(`still not valid`), nil }
	got := runOutcomeRetryLoop("r3", res(`{"summary":"no outcome"}`), resume, maxOutcomeRetries)
	if calls != 3 {
		t.Errorf("resume called %d times; want 3 (maxOutcomeRetries)", calls)
	}
	if completionHasValidOutcome(got) {
		t.Errorf("loop should give up with no valid outcome after exhausting retries")
	}
}

// TestOutcomeRetryLoop_ErrorHaltsRetries: a resume error stops the loop and
// preserves the accumulated completion rather than crashing.
func TestOutcomeRetryLoop_ErrorHaltsRetries(t *testing.T) {
	calls := 0
	resume := func() (*agentproc.Result, error) { calls++; return nil, context.DeadlineExceeded }
	got := runOutcomeRetryLoop("r4", res(`{"summary":"no outcome"}`), resume, maxOutcomeRetries)
	if calls != 1 {
		t.Errorf("resume called %d times; a resume error should halt after the first attempt", calls)
	}
	if got == nil {
		t.Errorf("loop must return the accumulated completion even on resume error")
	}
}

// --- Single/terminal disposition parity -----------------------------------

func loadRun(t *testing.T, s *Spawner, runID string) *domain.AgentRun {
	t.Helper()
	r, err := s.agentRuns.GetSystem(context.Background(), runmode.LocalDefaultOrg, runID)
	if err != nil || r == nil {
		t.Fatalf("load run %s: err=%v", runID, err)
	}
	return r
}

func loadTask(t *testing.T, s *Spawner, taskID string) domain.Task {
	t.Helper()
	tk, err := s.tasks.GetSystem(context.Background(), runmode.LocalDefaultOrg, taskID)
	if err != nil || tk == nil {
		t.Fatalf("load task %s: err=%v", taskID, err)
	}
	return *tk
}

// TestProcessCompletion_FinishClosesTask: a single/terminal run emitting
// finish persists outcome=finish and closes the task — today's single-prompt
// behavior, now keyed on the outcome.
func TestProcessCompletion_FinishClosesTask(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "finish")
	// Bot-claim the task so the close flows through BOTH the inline
	// disposition and advanceTaskFromRunStatus (the auto-progression path
	// an event-triggered run takes in production).
	stampBotClaim(t, database, taskID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrg, runID, task,
		res(`{"outcome":"finish","summary":"shipped it"}`), cwd, "", "claude-sonnet-4-6", "owner", "repo", "event", "", "")

	run := loadRun(t, s, runID)
	if run.Status != "completed" {
		t.Errorf("run.status = %q, want completed", run.Status)
	}
	if run.Outcome != "finish" {
		t.Errorf("run.outcome = %q, want finish", run.Outcome)
	}
	if got := readTaskStatus(t, database, taskID); got != "done" {
		t.Errorf("task.status = %q, want done (finish closes the task)", got)
	}
}

// TestProcessCompletion_AbortLeavesTaskOpen: an abort persists
// outcome=abort + the reason in outcome_reason (distinct from result_summary)
// and leaves the task open.
func TestProcessCompletion_AbortLeavesTaskOpen(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "abort")
	// Bot-claim the task: without the abort guard in
	// advanceTaskFromRunStatus, a bot-claimed task would auto-close on the
	// 'completed' status even though the inline path left it open. This
	// claim is what makes the regression real.
	stampBotClaim(t, database, taskID)
	task := loadTask(t, s, taskID)
	before := readTaskStatus(t, database, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrg, runID, task,
		res(`{"outcome":"abort","summary":"investigated the failure","reason":"needs a human to rotate the token"}`),
		cwd, "", "claude-sonnet-4-6", "owner", "repo", "event", "", "")

	run := loadRun(t, s, runID)
	if run.Status != "completed" {
		t.Errorf("run.status = %q, want completed (abort is run-completed, task-open)", run.Status)
	}
	if run.Outcome != "abort" {
		t.Errorf("run.outcome = %q, want abort", run.Outcome)
	}
	if run.OutcomeReason != "needs a human to rotate the token" {
		t.Errorf("run.outcome_reason = %q", run.OutcomeReason)
	}
	if run.ResultSummary != "investigated the failure" {
		t.Errorf("run.result_summary = %q; reason must stay distinct from summary", run.ResultSummary)
	}
	if got := readTaskStatus(t, database, taskID); got == "done" {
		t.Errorf("task.status = %q (was %q); abort must leave the task open", got, before)
	}
}

// TestProcessCompletion_YieldParks: a yield envelope parks the run in
// awaiting_input rather than completing.
func TestProcessCompletion_YieldParks(t *testing.T) {
	s, _, runID, taskID := setupAdvanceFixture(t, "yield")
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrg, runID, task,
		res(`{"outcome":"yield","summary":"need a decision","yield":{"type":"confirmation","message":"Proceed?"}}`),
		cwd, "sess-yield", "claude-sonnet-4-6", "owner", "repo", "event", "", "")

	run := loadRun(t, s, runID)
	if run.Status != "awaiting_input" {
		t.Errorf("run.status = %q, want awaiting_input", run.Status)
	}
}

// TestProcessCompletion_UnparseableFallsBackToFinish: a single/terminal run
// whose envelope never carries a valid outcome (and can't be gate-retried
// because no session id) falls back to finish — parity with today's
// "completed on unparseable".
func TestProcessCompletion_UnparseableFallsBackToFinish(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "fallback")
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrg, runID, task,
		res(`this is not a JSON envelope`), cwd, "", "claude-sonnet-4-6", "owner", "repo", "event", "", "")

	run := loadRun(t, s, runID)
	if run.Status != "completed" {
		t.Errorf("run.status = %q, want completed", run.Status)
	}
	if run.Outcome != "finish" {
		t.Errorf("run.outcome = %q, want finish (single/terminal fallback)", run.Outcome)
	}
	if got := readTaskStatus(t, database, taskID); got != "done" {
		t.Errorf("task.status = %q, want done", got)
	}
}
