package agentloop

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestSandboxTools_ParseTheVendoredDefinitions(t *testing.T) {
	tools, err := SandboxTools()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"read", "bash", "edit", "write", "grep", "find", "ls"}
	if len(tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(tools), len(want))
	}
	for i, name := range want {
		fn := tools[i].Function
		if fn == nil || fn.Name != name {
			t.Fatalf("tool %d = %+v, want %q", i, fn, name)
		}
		if fn.Description == nil || *fn.Description == "" {
			t.Errorf("%s has no description — the descriptions are part of the parity contract", name)
		}
		if fn.Parameters == nil || fn.Parameters.Type != "object" {
			t.Errorf("%s has no object parameter schema: %+v", name, fn.Parameters)
		}
	}
}

// TestSandboxTools_PreserveHarnessPropertyOrder pins that the wire schema
// keeps the harness's key order rather than a map-ordered re-serialization.
// Property order is part of the cached prefix, so churning it between
// processes would quietly cost cache hits.
func TestSandboxTools_PreserveHarnessPropertyOrder(t *testing.T) {
	tools, err := SandboxTools()
	if err != nil {
		t.Fatal(err)
	}
	// `read` is declared path, offset, limit in the harness.
	var read *string
	_ = read
	for _, tool := range tools {
		if tool.Function == nil || tool.Function.Name != "read" {
			continue
		}
		keys := tool.Function.Parameters.Properties.Keys()
		want := []string{"path", "offset", "limit"}
		if len(keys) != len(want) {
			t.Fatalf("read properties = %v, want %v", keys, want)
		}
		for i := range want {
			if keys[i] != want[i] {
				t.Fatalf("read property order = %v, want %v", keys, want)
			}
		}
		return
	}
	t.Fatal("the read tool is missing")
}

// TestToolDefinitions_MatchHarness regenerates the vendored definitions from
// the harness and compares. It skips when cargo is unavailable, so the
// default `go test ./...` needs no Rust toolchain — but on a machine that
// has one, a definitions.rs edit that never made it into the vendored file
// fails here rather than silently changing model behavior.
func TestToolDefinitions_MatchHarness(t *testing.T) {
	cargo, err := exec.LookPath("cargo")
	if err != nil {
		t.Skip("cargo is not available; the vendored tool definitions cannot be regenerated here")
	}
	crate, err := filepath.Abs(filepath.Join("..", "..", "harness", "tf-harness-tools"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(cargo, "run", "--quiet", "--bin", "tf-harness-tools", "--", "--definitions")
	cmd.Dir = crate
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("could not run the harness to regenerate definitions: %v", err)
	}

	var fresh, vendored any
	if err := json.Unmarshal(out, &fresh); err != nil {
		t.Fatalf("harness output is not JSON: %v", err)
	}
	if err := json.Unmarshal(toolDefinitionsJSON, &vendored); err != nil {
		t.Fatalf("vendored definitions are not JSON: %v", err)
	}
	freshCanon, _ := json.Marshal(fresh)
	vendoredCanon, _ := json.Marshal(vendored)
	if string(freshCanon) != string(vendoredCanon) {
		t.Fatalf("internal/agentloop/tools/definitions.json has drifted from the harness.\n" +
			"Regenerate it:\n" +
			"  (cd harness/tf-harness-tools && cargo run --quiet --bin tf-harness-tools -- --definitions) \\\n" +
			"    > internal/agentloop/tools/definitions.json")
	}
}

func TestFlowControlTools_RegisteredByStepPosition(t *testing.T) {
	terminal := flowControlTools(false)
	if len(terminal) != 1 || terminal[0].Function.Name != ToolAbort {
		t.Fatalf("a terminal step gets abort only: %+v", terminal)
	}
	nonTerminal := flowControlTools(true)
	if len(nonTerminal) != 2 || nonTerminal[0].Function.Name != ToolContinue || nonTerminal[1].Function.Name != ToolAbort {
		t.Fatalf("a non-terminal step gets continue and abort: %+v", nonTerminal)
	}
	for _, tool := range append(terminal, nonTerminal...) {
		if len(tool.Function.Parameters.Required) == 0 {
			t.Errorf("%s must require its companion field", tool.Function.Name)
		}
	}
}

func TestFlowControlOutcome_MapsToTheBlueprintVocabulary(t *testing.T) {
	tests := []struct {
		call        domain.ToolCall
		wantOutcome domain.RunOutcome
		wantSummary string
		wantReason  string
		wantOK      bool
	}{
		{
			call:        domain.ToolCall{Name: ToolContinue, Input: map[string]any{"summary": "did it"}},
			wantOutcome: domain.RunOutcomeContinue, wantSummary: "did it", wantOK: true,
		},
		{
			call:        domain.ToolCall{Name: ToolAbort, Input: map[string]any{"reason": "blocked"}},
			wantOutcome: domain.RunOutcomeAbort, wantReason: "blocked", wantOK: true,
		},
		{call: domain.ToolCall{Name: "bash"}, wantOK: false},
	}
	for _, tc := range tests {
		outcome, summary, reason, ok := flowControlOutcome(tc.call)
		if ok != tc.wantOK || outcome != tc.wantOutcome || summary != tc.wantSummary || reason != tc.wantReason {
			t.Errorf("%s -> (%q, %q, %q, %v), want (%q, %q, %q, %v)",
				tc.call.Name, outcome, summary, reason, ok, tc.wantOutcome, tc.wantSummary, tc.wantReason, tc.wantOK)
		}
	}
}

func TestBuildSystemPrompt_OrderAndAddendumGating(t *testing.T) {
	parts := EnvelopeParts{TaskContext: "TASKCTX", Envelope: "ENVELOPE", Mission: "MISSION"}

	terminal := BuildSystemPrompt(parts)
	for _, want := range []string{"coding agent", "TASKCTX", "ENVELOPE", "MISSION", "<completion>"} {
		if !containsSub(terminal, want) {
			t.Errorf("terminal system prompt is missing %q", want)
		}
	}
	if containsSub(terminal, "NOT the final step") {
		t.Error("a terminal step must not carry the non-terminal addendum")
	}
	// Order: base, task context, envelope, mission, completion.
	if !inOrder(terminal, "coding agent", "TASKCTX", "ENVELOPE", "MISSION", "<completion>") {
		t.Error("the fixed assembly order is what makes the prefix cacheable")
	}

	parts.NonTerminalStep = true
	nonTerminal := BuildSystemPrompt(parts)
	if !containsSub(nonTerminal, "NOT the final step") {
		t.Error("a non-terminal step must carry the addendum that introduces `continue`")
	}
	if !containsSub(nonTerminal, "`continue(summary)`") {
		t.Error("the addendum must describe the tool, not a JSON envelope")
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func inOrder(s string, subs ...string) bool {
	pos := 0
	for _, sub := range subs {
		found := -1
		for i := pos; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = i
				break
			}
		}
		if found < 0 {
			return false
		}
		pos = found + len(sub)
	}
	return true
}
