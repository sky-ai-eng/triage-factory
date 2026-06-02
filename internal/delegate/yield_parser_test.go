package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestParseAgentResult_AcceptsYieldEnvelope(t *testing.T) {
	cases := []struct {
		name string
		text string
		typ  string
	}{
		{
			name: "confirmation",
			text: `{"outcome":"yield","yield":{"type":"confirmation","message":"go?","accept_label":"Yes","reject_label":"No"}}`,
			typ:  domain.YieldTypeConfirmation,
		},
		{
			name: "choice",
			text: `{"outcome":"yield","yield":{"type":"choice","message":"which?","options":[{"id":"a","label":"A"}],"multi":false}}`,
			typ:  domain.YieldTypeChoice,
		},
		{
			name: "prompt",
			text: `{"outcome":"yield","yield":{"type":"prompt","message":"name?","placeholder":"x"}}`,
			typ:  domain.YieldTypePrompt,
		},
		{
			name: "with surrounding markdown fences",
			text: "```json\n{\"outcome\":\"yield\",\"yield\":{\"type\":\"confirmation\",\"message\":\"ok?\"}}\n```",
			typ:  domain.YieldTypeConfirmation,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAgentResult(tc.text)
			if got == nil {
				t.Fatalf("nil parse for %q", tc.text)
			}
			if got.Outcome != "yield" {
				t.Errorf("outcome = %q, want yield", got.Outcome)
			}
			if got.Yield == nil || got.Yield.Type != tc.typ {
				t.Errorf("yield.type = %v, want %q", got.Yield, tc.typ)
			}
		})
	}
}

func TestParseAgentResult_RejectsBadYield(t *testing.T) {
	bad := []string{
		// outcome:yield but no yield payload
		`{"outcome":"yield"}`,
		// yield with unknown type
		`{"outcome":"yield","yield":{"type":"plan_steps","message":"…"}}`,
		// yield with empty type
		`{"outcome":"yield","yield":{"message":"…"}}`,
		// confirmation with empty message
		`{"outcome":"yield","yield":{"type":"confirmation","message":""}}`,
		// prompt with whitespace-only message
		`{"outcome":"yield","yield":{"type":"prompt","message":"   "}}`,
		// choice with no options
		`{"outcome":"yield","yield":{"type":"choice","message":"pick","options":[]}}`,
		// choice with empty option id
		`{"outcome":"yield","yield":{"type":"choice","message":"pick","options":[{"id":"","label":"A"}]}}`,
		// choice with empty option label
		`{"outcome":"yield","yield":{"type":"choice","message":"pick","options":[{"id":"a","label":""}]}}`,
		// choice with duplicate option ids
		`{"outcome":"yield","yield":{"type":"choice","message":"pick","options":[{"id":"a","label":"A"},{"id":"a","label":"Also A"}]}}`,
	}
	for _, text := range bad {
		if got := parseAgentResult(text); got != nil {
			t.Errorf("expected nil for %q, got %+v", text, got)
		}
	}
}

func TestParseAgentResult_AcceptsFinishCompletion(t *testing.T) {
	got := parseAgentResult(`{"outcome":"finish","summary":"done"}`)
	if got == nil {
		t.Fatal("finish completion rejected")
	}
	if got.Summary != "done" {
		t.Errorf("summary = %q, want done", got.Summary)
	}
	if !got.hasValidOutcome() {
		t.Errorf("finish should be a valid outcome")
	}
}

// TestParseAgentResult_AbortCarriesReason confirms the abort envelope keeps
// reason distinct from summary.
func TestParseAgentResult_AbortCarriesReason(t *testing.T) {
	got := parseAgentResult(`{"outcome":"abort","summary":"looked into it","reason":"needs a human to approve the migration"}`)
	if got == nil {
		t.Fatal("abort completion rejected")
	}
	if got.Summary != "looked into it" {
		t.Errorf("summary = %q", got.Summary)
	}
	if got.Reason != "needs a human to approve the migration" {
		t.Errorf("reason = %q", got.Reason)
	}
}

// TestAbortRequiresReason: abort is the agent's voluntary stop decision, so
// the reason is required at the gate. An abort with a summary but no reason
// parses (isValid via summary) but is NOT a valid outcome — the outcome gate
// re-prompts for the why. An abort with a reason is valid.
func TestAbortRequiresReason(t *testing.T) {
	noReason := parseAgentResult(`{"outcome":"abort","summary":"stopped"}`)
	if noReason == nil {
		t.Fatal("abort envelope should still parse on its summary")
	}
	if noReason.hasValidOutcome() {
		t.Errorf("abort without a reason must not satisfy the outcome gate")
	}

	withReason := parseAgentResult(`{"outcome":"abort","summary":"stopped","reason":"needs a human"}`)
	if withReason == nil || !withReason.hasValidOutcome() {
		t.Errorf("abort with a reason should be a valid outcome: got=%+v", withReason)
	}
}

// TestAgentResult_SummaryOnlyHasNoValidOutcome confirms the outcome gate's
// invariant: a summary-only envelope parses (isValid) but has no recognized
// outcome, so the gate would re-prompt.
func TestAgentResult_SummaryOnlyHasNoValidOutcome(t *testing.T) {
	got := parseAgentResult(`{"summary":"did stuff"}`)
	if got == nil {
		t.Fatal("summary-only envelope should still parse")
	}
	if got.hasValidOutcome() {
		t.Errorf("summary-only envelope must not report a valid outcome")
	}
}

// TestYieldWithoutPayloadIsNotValid pins the fix for the malformed-yield
// hole: outcome="yield" with a non-empty summary but no (or an invalid)
// yield payload must NOT count as a well-formed yield or a valid outcome.
// Otherwise it would parse on the summary, slip past the outcome gate, and
// then be treated as a normal completion that closes the task.
func TestYieldWithoutPayloadIsNotValid(t *testing.T) {
	// outcome=yield, summary present, payload entirely missing.
	got := parseAgentResult(`{"outcome":"yield","summary":"I want to pause and ask"}`)
	if got == nil {
		t.Fatal("envelope should still parse on its summary")
	}
	if got.isYield() {
		t.Errorf("a yield with no payload must not be a well-formed yield")
	}
	if got.hasValidOutcome() {
		t.Errorf("a yield with no payload must not satisfy the outcome gate")
	}

	// outcome=yield, summary present, payload present but invalid (choice
	// with no options — nothing for the user to click).
	bad := parseAgentResult(`{"outcome":"yield","summary":"pick one","yield":{"type":"choice","message":"which?","options":[]}}`)
	if bad == nil {
		t.Fatal("envelope should still parse on its summary")
	}
	if bad.isYield() || bad.hasValidOutcome() {
		t.Errorf("a yield with an invalid payload must not be valid: isYield=%v hasValidOutcome=%v", bad.isYield(), bad.hasValidOutcome())
	}

	// A well-formed yield (no summary) remains both isYield and a valid outcome.
	good := parseAgentResult(`{"outcome":"yield","yield":{"type":"confirmation","message":"Proceed?"}}`)
	if good == nil || !good.isYield() || !good.hasValidOutcome() {
		t.Errorf("a well-formed yield should pass: got=%+v", good)
	}
}
