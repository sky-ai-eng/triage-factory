package tooldefs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestStopBlueprint_Shape pins the shape the prompts are written against:
// all three arguments required, and a type that is exactly finish or abort.
func TestStopBlueprint_Shape(t *testing.T) {
	if StopBlueprint.Handler == nil {
		t.Error("stop_blueprint must be answered by TF, never dispatched into the jail")
	}
	if req := StopBlueprint.Params.Required; len(req) != 3 || req[0] != "type" || req[1] != "reason" || req[2] != "summary" {
		t.Fatalf("required = %v, want type, reason and summary — an unexplained stop with a blank result is the thing being prevented", req)
	}

	var enum []string
	for _, f := range StopBlueprint.Params.Fields {
		if f.Name == "type" {
			enum = f.Enum
		}
	}
	if len(enum) != 2 || enum[0] != stopTypeFinish || enum[1] != stopTypeAbort {
		t.Fatalf("type enum = %v, want exactly finish and abort", enum)
	}
	for _, v := range enum {
		if v == string(domain.ConversationOutcomeContinue) {
			t.Error("continue must not be callable: it is what stopping already means")
		}
	}
}

// TestSandbox_HasNoHandlers pins the other half of the split. Every one of
// the seven is implemented in the jail, so a Handler appearing on one would
// silently shadow the real implementation with a Go stub.
func TestSandbox_HasNoHandlers(t *testing.T) {
	for _, tool := range Sandbox() {
		if tool.Handler != nil {
			t.Errorf("%s has a TF-side handler; the harness owns this tool", tool.Name)
		}
	}
}

func TestStopBlueprint_Handler(t *testing.T) {
	tests := []struct {
		name         string
		args         map[string]any
		wantTerminal domain.ConversationOutcome
		wantReason   string
		wantSummary  string
		wantError    bool
		wantContent  string
	}{
		{
			name:         "finish",
			args:         map[string]any{"type": "finish", "reason": "nothing to review", "summary": "checked the diff; no reviewable changes"},
			wantTerminal: domain.ConversationOutcomeFinish, wantReason: "nothing to review",
			wantSummary: "checked the diff; no reviewable changes",
			wantContent: "nothing to review",
		},
		{
			name:         "abort",
			args:         map[string]any{"type": "abort", "reason": "the branch is gone", "summary": "tried to rebase; upstream deleted the branch"},
			wantTerminal: domain.ConversationOutcomeAbort, wantReason: "the branch is gone",
			wantSummary: "tried to rebase; upstream deleted the branch",
			wantContent: "the branch is gone",
		},
		{
			// Reaching for `continue` is the mistake most worth catching. It
			// must not terminate, and the correction must point at stopping
			// rather than at another tool.
			name:      "continue is not a type",
			args:      map[string]any{"type": "continue", "reason": "did my part"},
			wantError: true, wantContent: "no tool calls",
		},
		{
			name:      "no type at all",
			args:      map[string]any{"reason": "r"},
			wantError: true, wantContent: "no tool calls",
		},
		{
			name:      "a reasonless stop is refused",
			args:      map[string]any{"type": "abort", "summary": "s"},
			wantError: true, wantContent: "requires a reason",
		},
		{
			// Models routinely emit tool-call-only messages, so without this a
			// completed run lands with a blank result and nothing to fall back on.
			name:      "a summaryless stop is refused",
			args:      map[string]any{"type": "finish", "reason": "r"},
			wantError: true, wantContent: "requires a summary",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StopBlueprint.Handler(context.Background(), Call{Args: tc.args})
			if got.IsError != tc.wantError {
				t.Errorf("IsError = %v, want %v (%q)", got.IsError, tc.wantError, got.Content)
			}
			if got.Terminal != tc.wantTerminal {
				t.Errorf("Terminal = %q, want %q", got.Terminal, tc.wantTerminal)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.Summary != tc.wantSummary {
				t.Errorf("Summary = %q, want %q", got.Summary, tc.wantSummary)
			}
			if !strings.Contains(got.Content, tc.wantContent) {
				t.Errorf("Content = %q, want it to mention %q", got.Content, tc.wantContent)
			}
			// A correction is never a termination: the model is told what it
			// owes and the run carries on.
			if got.IsError && got.Terminal != "" {
				t.Error("a refused call must not land a terminal state")
			}
		})
	}
}

// TestChatTools_RenderTheDeclaredShape pins the conversion the provider sees:
// declared property order at every depth, and required lists carried through.
func TestChatTools_RenderTheDeclaredShape(t *testing.T) {
	got := ChatTools([]Tool{StopBlueprint})
	if len(got) != 1 || got[0].Function == nil {
		t.Fatalf("conversion produced %+v", got)
	}
	fn := got[0].Function
	if fn.Name != StopBlueprintName || fn.Description == nil || *fn.Description == "" {
		t.Fatalf("name/description lost in conversion: %+v", fn)
	}
	keys := fn.Parameters.Properties.Keys()
	if len(keys) != 3 || keys[0] != "type" || keys[1] != "reason" || keys[2] != "summary" {
		t.Errorf("property order = %v, want declaration order — it is part of the cached prefix", keys)
	}

	raw, err := json.Marshal(fn.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"enum"`, `"finish"`, `"abort"`, `"required"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("rendered schema is missing %s: %s", want, raw)
		}
	}
}
