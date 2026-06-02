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
