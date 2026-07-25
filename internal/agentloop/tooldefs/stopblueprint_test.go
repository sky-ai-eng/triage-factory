package tooldefs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestFlowControl_IsOneToolGatedOnHavingABlueprint pins the shape the prompts
// are written against: a single tool with both arguments required, offered
// only where there is something for it to stop.
func TestFlowControl_IsOneToolGatedOnHavingABlueprint(t *testing.T) {
	if got := FlowControl(false); len(got) != 0 {
		t.Fatalf("a conversation with no blueprint has nothing to stop: %+v", got)
	}
	flow := FlowControl(true)
	if len(flow) != 1 || flow[0].Name != StopBlueprintName {
		t.Fatalf("flow control is exactly one tool: %+v", flow)
	}
	if flow[0].Handler == nil {
		t.Error("a flow-control tool must be answered loop-side, never dispatched into the jail")
	}
	if req := flow[0].Params.Required; len(req) != 2 || req[0] != "type" || req[1] != "reason" {
		t.Fatalf("required = %v, want both type and reason — an unexplained stop is the thing being prevented", req)
	}

	var enum []string
	for _, f := range flow[0].Params.Fields {
		if f.Name == "type" {
			enum = f.Enum
		}
	}
	if len(enum) != 2 || enum[0] != stopTypeFinish || enum[1] != stopTypeAbort {
		t.Fatalf("type enum = %v, want exactly finish and abort", enum)
	}
	for _, v := range enum {
		if v == string(domain.RunOutcomeContinue) {
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
			t.Errorf("%s has a loop-side handler; the harness owns this tool", tool.Name)
		}
	}
}

// TestLoopSide_ResolvesOnlyWhatWasOffered pins that a name means nothing
// where the tool was never registered. A hallucinated stop_blueprint in a
// taskless conversation must fall through to the jail and come back "unknown
// tool" rather than silently ending a conversation someone is mid-sentence in.
func TestLoopSide_ResolvesOnlyWhatWasOffered(t *testing.T) {
	if _, ok := LoopSide(StopBlueprintName, true); !ok {
		t.Error("a blueprint conversation must resolve its own flow-control tool")
	}
	if _, ok := LoopSide(StopBlueprintName, false); ok {
		t.Error("nothing is loop-side in a conversation with no blueprint")
	}
	if _, ok := LoopSide("bash", true); ok {
		t.Error("a jail tool must never resolve loop-side")
	}
}

func TestStopBlueprint_Handler(t *testing.T) {
	tests := []struct {
		name         string
		args         map[string]any
		wantTerminal domain.RunOutcome
		wantReason   string
		wantError    bool
		wantContent  string
	}{
		{
			name:         "finish",
			args:         map[string]any{"type": "finish", "reason": "nothing to review"},
			wantTerminal: domain.RunOutcomeFinish, wantReason: "nothing to review",
			wantContent: "nothing to review",
		},
		{
			name:         "abort",
			args:         map[string]any{"type": "abort", "reason": "the branch is gone"},
			wantTerminal: domain.RunOutcomeAbort, wantReason: "the branch is gone",
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
			args:      map[string]any{"type": "abort"},
			wantError: true, wantContent: "requires a reason",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StopBlueprint.Handler(tc.args)
			if got.IsError != tc.wantError {
				t.Errorf("IsError = %v, want %v (%q)", got.IsError, tc.wantError, got.Content)
			}
			if got.Terminal != tc.wantTerminal {
				t.Errorf("Terminal = %q, want %q", got.Terminal, tc.wantTerminal)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
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
	if len(keys) != 2 || keys[0] != "type" || keys[1] != "reason" {
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
