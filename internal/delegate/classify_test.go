package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestClassifyTurn_AFinishedTurnConcludesContinue pins the implicit
// conclusion: a turn the model ended itself concludes `continue` with its
// final text as the summary, whatever that text looks like. Text shaped like
// a JSON verdict is a summary like any other, and an empty stop reason reads
// as an end of turn.
func TestClassifyTurn_AFinishedTurnConcludesContinue(t *testing.T) {
	cases := []struct {
		name       string
		stopReason string
		text       string
		want       string
	}{
		{"end of turn", "end_turn", "  Opened the pull request.\n", "Opened the pull request."},
		{"no reason reported", "", "Done.", "Done."},
		{"text shaped like a verdict", "end_turn", `{"outcome":"abort","reason":"blocked"}`, `{"outcome":"abort","reason":"blocked"}`},
		{"no text", "end_turn", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, verdict := classifyTurn(&agentproc.Result{StopReason: tc.stopReason, Result: tc.text})
			if class != turnConcluded {
				t.Fatalf("class = %v, want turnConcluded", class)
			}
			want := turnVerdict{Outcome: domain.ConversationOutcomeContinue, Summary: tc.want}
			if verdict != want {
				t.Errorf("verdict = %+v, want %+v", verdict, want)
			}
		})
	}
}

// TestClassifyTurn_AnUnfinishedTurnParks pins the allowlist: a turn whose
// stop reason is anything but an end of turn has no verdict, so a refusal or
// a cut-off reply never advances a blueprint.
func TestClassifyTurn_AnUnfinishedTurnParks(t *testing.T) {
	for _, reason := range []string{"refusal", "max_tokens", "model_context_window_exceeded", "pause_turn", "tool_use", "something_new"} {
		t.Run(reason, func(t *testing.T) {
			class, verdict := classifyTurn(&agentproc.Result{StopReason: reason, Result: "partial"})
			if class != turnParked {
				t.Fatalf("class = %v, want turnParked", class)
			}
			if verdict != (turnVerdict{}) {
				t.Errorf("verdict = %+v, want none", verdict)
			}
		})
	}
}

// TestClassifyTurn_AStopIsTheVerdict: a turn stop_blueprint ended concludes on
// the call's outcome, reason and summary, whatever the stop reason and the
// text after it.
func TestClassifyTurn_AStopIsTheVerdict(t *testing.T) {
	class, verdict := classifyTurn(&agentproc.Result{
		StopReason: "tool_use",
		Result:     "Stopping here.",
		Terminal:   &agentproc.Terminal{Outcome: domain.ConversationOutcomeAbort, Reason: "blocked", Summary: "tried"},
	})
	want := turnVerdict{Outcome: domain.ConversationOutcomeAbort, Reason: "blocked", Summary: "tried"}
	if class != turnConcluded || verdict != want {
		t.Errorf("classifyTurn = (%v, %+v), want (turnConcluded, %+v)", class, verdict, want)
	}
}
