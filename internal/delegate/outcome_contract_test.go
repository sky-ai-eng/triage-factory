package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// res is a turn the model ended itself, with body as its final text.
func res(body string) *agentproc.Result {
	return &agentproc.Result{StopReason: "end_turn", Result: body}
}

// stopped is a turn stop_blueprint ended.
func stopped(outcome domain.ConversationOutcome, reason, summary string) *agentproc.Result {
	return &agentproc.Result{StopReason: "tool_use", Terminal: &agentproc.Terminal{Outcome: outcome, Reason: reason, Summary: summary}}
}

// --- Position-gated contract injection ------------------------------------

// nonterminalStepSysPrompt is the one position bit that reaches the agent:
// the handoff addendum is injected iff the step is not the last. The terminal
// step — and therefore the whole N=1 case — gets nothing.
func TestNonterminalStepSysPrompt_PositionGated(t *testing.T) {
	// N=1: the single step is also the terminal step → no fragment.
	if got := nonterminalStepSysPrompt(0, 1); got != "" {
		t.Errorf("N=1 step got a non-empty fragment; the single-prompt case hands off to nothing")
	}
	// N=3: steps 0 and 1 are non-terminal → fragment; step 2 is terminal → none.
	if got := nonterminalStepSysPrompt(0, 3); got == "" {
		t.Errorf("non-terminal step 0/3 got no fragment")
	}
	if got := nonterminalStepSysPrompt(1, 3); got == "" {
		t.Errorf("non-terminal step 1/3 got no fragment")
	}
	if got := nonterminalStepSysPrompt(2, 3); got != "" {
		t.Errorf("terminal step 2/3 got a fragment; the last step hands off to nothing")
	}
}

// --- Terminal disposition (processCompletion) -----------------------------
//
// These tests feed processCompletion a turn-end directly, as the live driver
// and the resume path hand it one, to pin how it records each.

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

// TestProcessCompletion_AFinishedTurnConcludesContinue: a turn the model
// ended itself concludes `continue`, its final text the summary and no reason
// recorded. processCompletion does not close the task — task disposition is
// the orchestrator's (terminateBlueprint), which resolves `continue` on a
// final step to a finish.
func TestProcessCompletion_AFinishedTurnConcludesContinue(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "finish")
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID), task,
		res("Shipped it."), cwd, nil, "", "event", "")

	conv := loadConversation(t, s, conversationID)
	if !conv.Concluded() {
		t.Errorf("conv = (status %q, completed_at %v), want concluded", conv.Status, conv.CompletedAt)
	}
	if conv.Outcome != "continue" || conv.ResultSummary != "Shipped it." || conv.OutcomeReason != "" {
		t.Errorf("conv = (outcome %q, summary %q, reason %q), want continue with the final text and no reason", conv.Outcome, conv.ResultSummary, conv.OutcomeReason)
	}
	if got := readTaskStatus(t, database, taskID); got == "done" {
		t.Errorf("task.status = %q; processCompletion must not close the task (terminateBlueprint does)", got)
	}
}

// TestProcessCompletion_AbortLeavesTaskOpen: a stop_blueprint abort persists
// outcome=abort + the reason in outcome_reason (distinct from result_summary)
// and never closes the task.
func TestProcessCompletion_AbortLeavesTaskOpen(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "abort")
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	task := loadTask(t, s, taskID)
	before := readTaskStatus(t, database, taskID)
	cwd := t.TempDir()

	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID), task,
		stopped(domain.ConversationOutcomeAbort, "needs a human to rotate the token", "investigated the failure"),
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

// TestProcessCompletion_StopBlueprintRecordsTheCall: a turn stop_blueprint
// ended records the call's outcome, summary and reason, a finish's reason
// included as the native runtime records it, and the text after the call,
// which alone would conclude `continue`, is not read as the verdict.
func TestProcessCompletion_StopBlueprintRecordsTheCall(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "stop-finish")
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	stop := stopped(domain.ConversationOutcomeFinish, "nothing to review", "checked the diff")
	stop.Result = "Ending the workflow here."
	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID), task,
		stop, cwd, nil, "", "event", "")

	conv := loadConversation(t, s, conversationID)
	if !conv.Concluded() || conv.Outcome != "finish" {
		t.Fatalf("conv = (status %q, outcome %q), want concluded on finish", conv.Status, conv.Outcome)
	}
	if conv.ResultSummary != "checked the diff" || conv.OutcomeReason != "nothing to review" {
		t.Errorf("conv = (summary %q, reason %q), want the call's summary and reason", conv.ResultSummary, conv.OutcomeReason)
	}
}

// TestProcessCompletion_AnUnfinishedTurnParksOpenWithANote: a turn the model
// did not finish (here a refusal) is not a termination — the run parks `open`
// with no verdict, never a conclusion the orchestrator would advance on, and
// the transcript carries a note naming the stop reason and what wakes it.
func TestProcessCompletion_AnUnfinishedTurnParksOpenWithANote(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "open-refusal")
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()

	parked, fenced := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID), task,
		&agentproc.Result{StopReason: "refusal", Result: "partial"}, cwd, nil, "", "event", "")

	if !parked || fenced {
		t.Errorf("processCompletion(refusal) = (parked %v, fenced %v), want (true, false): open, not terminal", parked, fenced)
	}
	conv := loadConversation(t, s, conversationID)
	if conv.Status != domain.StatusOpen || conv.Concluded() || conv.Outcome != "" {
		t.Errorf("conv = (status %q, outcome %q, completed_at %v), want open with no verdict", conv.Status, conv.Outcome, conv.CompletedAt)
	}
	if conv.ParkReason != domain.ParkReasonIdle {
		t.Errorf("conv.park_reason = %q, want %q", conv.ParkReason, domain.ParkReasonIdle)
	}
	var note string
	if err := database.QueryRow(`SELECT content FROM messages WHERE conversation_id = ? AND subtype = ? ORDER BY id DESC LIMIT 1`,
		conversationID, domain.MessageSubtypeStopNote).Scan(&note); err != nil {
		t.Fatalf("read the park note: %v", err)
	}
	if note != stopReasonParkNotice("refusal") {
		t.Errorf("park note = %q, want the refusal's notice", note)
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
