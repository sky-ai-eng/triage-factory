package delegate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
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

func loadConversation(t *testing.T, s *Spawner, conversationID string) *domain.Conversation {
	t.Helper()
	r, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil || r == nil {
		t.Fatalf("load conversation %s: err=%v", conversationID, err)
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
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID), task,
		res(`{"outcome":"finish","summary":"shipped it"}`), cwd, nil, "", "event", "")

	conv := loadConversation(t, s, conversationID)
	if !conv.Concluded() {
		t.Errorf("conv = (status %q, completed_at %v), want concluded", conv.Status, conv.CompletedAt)
	}
	if conv.Outcome != "finish" {
		t.Errorf("conv.outcome = %q, want finish", conv.Outcome)
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
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	task := loadTask(t, s, taskID)
	before := readTaskStatus(t, database, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID), task,
		res(`{"outcome":"abort","summary":"investigated the failure","reason":"needs a human to rotate the token"}`),
		cwd, nil, "", "event", "")

	conv := loadConversation(t, s, conversationID)
	if !conv.Concluded() {
		t.Errorf("conv = (status %q, completed_at %v), want concluded (an abort is a verdict; the task stays open)", conv.Status, conv.CompletedAt)
	}
	if conv.Outcome != "abort" {
		t.Errorf("conv.outcome = %q, want abort", conv.Outcome)
	}
	if conv.OutcomeReason != "needs a human to rotate the token" {
		t.Errorf("conv.outcome_reason = %q", conv.OutcomeReason)
	}
	if conv.ResultSummary != "investigated the failure" {
		t.Errorf("conv.result_summary = %q; reason must stay distinct from summary", conv.ResultSummary)
	}
	if got := readTaskStatus(t, database, taskID); got == "done" {
		t.Errorf("task.status = %q (was %q); abort must leave the task open", got, before)
	}
}

// TestProcessCompletion_AbortMissingReasonParksOnTheInvalidEnvelope: abort is
// the agent's voluntary stop, so the reason is its required companion — an
// abort without one is an invalid envelope, not a valid conclusion. The live
// driver re-prompts it; a non-live path (resume / one-shot) that can't lands
// here, where it parks with no recorded outcome, never a clean NULL-outcome
// completion the orchestrator would read as finish.
func TestProcessCompletion_AbortMissingReasonParksOnTheInvalidEnvelope(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "abort-noreason")
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID), task,
		res(`{"outcome":"abort","summary":"stopped, couldn't proceed"}`), cwd, nil, "", "event", "")

	conv := loadConversation(t, s, conversationID)
	if !conv.ParkedOnInvalidEnvelope() {
		t.Errorf("conv = (status %q, park_reason %q), want open on invalid_envelope (an abort with no reason is an invalid envelope)", conv.Status, conv.ParkReason)
	}
	if conv.Outcome != "" {
		t.Errorf("conv.outcome = %q, want empty (an abort with no reason is not a valid conclusion)", conv.Outcome)
	}
}

// TestProcessCompletion_NoConclusionParksOpen: a turn that ends with no
// envelope at all (prose) is not a termination — the run is parked `open` (not
// recorded as a NULL-outcome completion, which would let the orchestrator close
// a final step). This is the disposition for a one-shot/resume turn that ends
// without concluding (the live driver consumes this in its loop instead).
func TestProcessCompletion_NoConclusionParksOpen(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "open-noconcl")
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	parked, _ := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID), task,
		res(`this is not a JSON envelope`), cwd, nil, "", "event", "")

	if !parked {
		t.Error("processCompletion(no-conclusion) = false; want true (open, not terminal)")
	}
	conv := loadConversation(t, s, conversationID)
	if conv.Status != "open" {
		t.Errorf("conv.status = %q, want open (a no-conclusion turn parks open, not completed)", conv.Status)
	}
	if conv.Outcome != "" {
		t.Errorf("conv.outcome = %q, want \"\" (no conclusion was recorded)", conv.Outcome)
	}
}

// TestProcessCompletion_InvalidEnvelopeParksOpenWithNoVerdict: an envelope
// attempt that never validated (here a `finish` with no summary) leaves the
// transcript whole, so it is not a failure: the conversation parks `open` with
// no verdict and the invalid_envelope reason the reactor aborts the blueprint
// on. Never a NULL-outcome conclusion, which the reactor would read as a clean
// finish on a final step. The live driver re-prompts this in place; when a
// backend can't or the bound is exhausted, the unfixed result lands here
// (TestDriveLiveConversation_InvalidRepromptsToBoundThenHandsBack).
func TestProcessCompletion_InvalidEnvelopeParksOpenWithNoVerdict(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "invalid-parks")
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	parked, fenced := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID), task,
		res(`{"outcome":"finish"}`), cwd, nil, "", "event", "")

	if parked || fenced {
		t.Errorf("processCompletion(invalid) = (parked %v, fenced %v), want (false, false): the step has answered and the blueprint acts on it", parked, fenced)
	}
	conv := loadConversation(t, s, conversationID)
	if conv.Status != domain.StatusOpen || conv.ParkReason != domain.ParkReasonInvalidEnvelope {
		t.Errorf("conv = (status %q, park_reason %q), want (open, invalid_envelope)", conv.Status, conv.ParkReason)
	}
	if conv.Concluded() || conv.Outcome != "" {
		t.Errorf("conv = (completed_at %v, outcome %q), want no verdict recorded", conv.CompletedAt, conv.Outcome)
	}
	if conv.FailureKind != "" {
		t.Errorf("conv.failure_kind = %q, want none: the runtime did not fail", conv.FailureKind)
	}
}

// TestInvalidEnvelope_AbortsTheBlueprintAndLeavesTheTaskOpen is the whole
// disposition: the park above, then the reactor reading it. The blueprint
// aborts with a reason naming the envelope, the task stays open (an abort
// never closes it), and the conversation keeps its park with no verdict.
func TestInvalidEnvelope_AbortsTheBlueprintAndLeavesTheTaskOpen(t *testing.T) {
	s, database, brID, taskID, conversationID := reactorFixture(t, "invalid-abort", 1, "running", "")
	org := runmode.LocalDefaultOrgID
	seedLocalBotAgent(t, database)
	stampBotClaim(t, database, taskID)

	s.processCompletion(context.Background(), org, conversationID, brID, holderClaimFor(t, s, org, conversationID), loadTask(t, s, taskID),
		res(`{"outcome":"frobnicate"}`), t.TempDir(), nil, "", "manual", runmode.LocalDefaultUserID)

	stepConversation := loadConversation(t, s, conversationID)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	br := mustGetRun(t, s, org, brID)
	if br.Status != domain.BlueprintRunStatusAborted || br.AbortReason != invalidEnvelopeAbortReason {
		t.Errorf("blueprint = (%q, reason %q), want (aborted, %q)", br.Status, br.AbortReason, invalidEnvelopeAbortReason)
	}
	if got := readTaskStatus(t, database, taskID); got == "done" || got == "dismissed" {
		t.Errorf("task.status = %q, want it left open for a person", got)
	}
	conv := loadConversation(t, s, conversationID)
	if !conv.ParkedOnInvalidEnvelope() || conv.Concluded() || conv.Outcome != "" {
		t.Errorf("conv = (status %q, park_reason %q, outcome %q, completed_at %v), want parked on invalid_envelope with no verdict",
			conv.Status, conv.ParkReason, conv.Outcome, conv.CompletedAt)
	}
}

// TestTerminateBlueprint_CompletedClosesTask pins the orchestrator-owned close:
// finalizing a blueprint_run as completed closes its task (done +
// close_reason=run_completed).
func TestTerminateBlueprint_CompletedClosesTask(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "terminate-done")
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

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
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "manual", runmode.LocalDefaultUserID,
		time.Now(), runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusAborted, "needs a human", nil, true)

	if got := readTaskStatus(t, database, taskID); got == "done" {
		t.Errorf("task.status = %q; abort must leave the task open", got)
	}
}

// parkOnInvalidEnvelope stages the row processCompletion leaves behind for an
// envelope that never validated: `open`, no verdict, the invalid_envelope
// reason.
func parkOnInvalidEnvelope(t *testing.T, database *sql.DB, conversationID string) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE conversations SET status = 'open', completed_at = NULL, outcome = NULL, park_reason = ? WHERE id = ?`,
		string(domain.ParkReasonInvalidEnvelope), conversationID); err != nil {
		t.Fatalf("park %s on the invalid envelope: %v", conversationID, err)
	}
}

// TestResumeBlueprintAfterResume_InvalidEnvelopeAbortsARunningBlueprint is the
// resume path's half of the disposition: a follow-up turn on a re-opened
// blueprint that again ends on an envelope that never validated aborts it, as
// the first engagement's did. On a finished blueprint the same park re-drives
// nothing: a follow-up there never changes the blueprint.
func TestResumeBlueprintAfterResume_InvalidEnvelopeAbortsARunningBlueprint(t *testing.T) {
	for _, tc := range []struct {
		name      string
		blueprint domain.BlueprintRunStatus
		want      domain.BlueprintRunStatus
		reason    string
	}{
		{"running", domain.BlueprintRunStatusRunning, domain.BlueprintRunStatusAborted, invalidEnvelopeAbortReason},
		{"finished", domain.BlueprintRunStatusCompleted, domain.BlueprintRunStatusCompleted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, database, brID, taskID, conversationID := reactorFixture(t, "resume-invalid-"+tc.name, 1, "open", "")
			org := runmode.LocalDefaultOrgID
			parkOnInvalidEnvelope(t, database, conversationID)
			if tc.blueprint != domain.BlueprintRunStatusRunning {
				completeBlueprintRun(t, database, brID)
			}
			taskBefore := readTaskStatus(t, database, taskID)

			s.ResumeBlueprintAfterResume(org, conversationID, runmode.LocalDefaultUserID)

			br := mustGetRun(t, s, org, brID)
			if br.Status != tc.want || br.AbortReason != tc.reason {
				t.Errorf("blueprint = (%q, reason %q), want (%q, reason %q)", br.Status, br.AbortReason, tc.want, tc.reason)
			}
			if got := readTaskStatus(t, database, taskID); got != taskBefore {
				t.Errorf("task.status = %q, want it unchanged at %q", got, taskBefore)
			}
		})
	}
}

// TestInvalidEnvelope_OnAFollowUpWithdrawsTheEarlierVerdict: a follow-up on a
// step whose blueprint will not re-open (it finished, or it aborted on a task
// since closed) wakes a row that still carries the verdict its first
// engagement recorded. When that follow-up ends on an envelope that never
// validated, the row parks with no verdict, like any other invalid envelope,
// rather than reading as concluded with the old outcome. The blueprint and
// the task are left as they were: a follow-up there changes neither.
func TestInvalidEnvelope_OnAFollowUpWithdrawsTheEarlierVerdict(t *testing.T) {
	for _, tc := range []struct {
		name      string
		outcome   string
		blueprint domain.BlueprintRunStatus
		closeTask bool
	}{
		{"finished blueprint", "finish", domain.BlueprintRunStatusCompleted, false},
		{"aborted blueprint, task closed", "abort", domain.BlueprintRunStatusAborted, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, database, brID, taskID, conversationID := reactorFixture(t, "followup-invalid-"+string(tc.blueprint), 1, dbtest.SeedConcluded, tc.outcome)
			org := runmode.LocalDefaultOrgID
			ctx := context.Background()
			if _, err := database.Exec(`UPDATE conversations SET outcome_reason = 'earlier reason' WHERE id = ?`, conversationID); err != nil {
				t.Fatalf("stage the earlier verdict's reason: %v", err)
			}
			finishBlueprint(t, database, brID, string(tc.blueprint), 0)
			if tc.closeTask {
				if _, err := s.tasks.CloseSystem(ctx, org, taskID, "user_done", ""); err != nil {
					t.Fatalf("close task: %v", err)
				}
			}
			taskBefore := readTaskStatus(t, database, taskID)

			if ok, err := s.conversations.MarkQueuedForResume(ctx, org, conversationID); err != nil || !ok {
				t.Fatalf("wake the concluded step = (%v, %v), want (true, nil)", ok, err)
			}
			s.processCompletion(ctx, org, conversationID, brID, holderClaimFor(t, s, org, conversationID), loadTask(t, s, taskID),
				res(`{"outcome":"frobnicate"}`), t.TempDir(), nil, "", "manual", runmode.LocalDefaultUserID)
			s.ResumeBlueprintAfterResume(org, conversationID, runmode.LocalDefaultUserID)

			conv := loadConversation(t, s, conversationID)
			if !conv.ParkedOnInvalidEnvelope() || conv.Concluded() {
				t.Errorf("conv = (status %q, park_reason %q, completed_at %v), want parked on invalid_envelope and not concluded",
					conv.Status, conv.ParkReason, conv.CompletedAt)
			}
			if conv.Outcome != "" || conv.OutcomeReason != "" {
				t.Errorf("verdict = (outcome %q, reason %q), want the earlier one withdrawn", conv.Outcome, conv.OutcomeReason)
			}
			if br := mustGetRun(t, s, org, brID); br.Status != tc.blueprint {
				t.Errorf("blueprint = %q, want it left %q", br.Status, tc.blueprint)
			}
			if got := readTaskStatus(t, database, taskID); got != taskBefore {
				t.Errorf("task.status = %q, want it unchanged at %q", got, taskBefore)
			}
		})
	}
}
