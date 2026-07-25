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

// TestFlowControlTools_IsOneToolWithBothArgumentsRequired pins the shape the
// prompts are written against: a single tool, both arguments required, and
// no `continue` type — stopping is what continuing means, so offering it
// here would restore the ambiguity the consolidation removed.
func TestFlowControlTools_IsOneToolWithBothArgumentsRequired(t *testing.T) {
	if flow := flowControlTools(false); len(flow) != 0 {
		t.Fatalf("a conversation with no blueprint has nothing to stop: %+v", flow)
	}
	flow := flowControlTools(true)
	if len(flow) != 1 || flow[0].Function.Name != ToolStopBlueprint {
		t.Fatalf("flow control is exactly one tool: %+v", flow)
	}
	req := flow[0].Function.Parameters.Required
	if len(req) != 2 || req[0] != "type" || req[1] != "reason" {
		t.Fatalf("required = %v, want both type and reason — an unexplained stop is the thing being prevented", req)
	}
	spec, ok := flow[0].Function.Parameters.Properties.Get("type")
	if !ok {
		t.Fatal("the type argument must be declared")
	}
	enum := spec.(map[string]any)["enum"].([]string)
	if len(enum) != 2 || enum[0] != stopTypeFinish || enum[1] != stopTypeAbort {
		t.Errorf("type enum = %v, want exactly finish and abort", enum)
	}
	for _, v := range enum {
		if v == string(domain.RunOutcomeContinue) {
			t.Error("continue must not be callable: it is what stopping already means")
		}
	}
}

func TestFlowControlOutcome_MapsToTheBlueprintVocabulary(t *testing.T) {
	tests := []struct {
		name        string
		call        domain.ToolCall
		wantOutcome domain.RunOutcome
		wantReason  string
		wantFlow    bool
	}{
		{
			name:        "finish",
			call:        domain.ToolCall{Name: ToolStopBlueprint, Input: map[string]any{"type": "finish", "reason": "nothing to review"}},
			wantOutcome: domain.RunOutcomeFinish, wantReason: "nothing to review", wantFlow: true,
		},
		{
			name:        "abort",
			call:        domain.ToolCall{Name: ToolStopBlueprint, Input: map[string]any{"type": "abort", "reason": "blocked"}},
			wantOutcome: domain.RunOutcomeAbort, wantReason: "blocked", wantFlow: true,
		},
		{
			// Reaching for `continue` is the mistake most worth catching, and
			// it must not resolve to an outcome: the run keeps going and the
			// model is told to just stop instead.
			name:       "continue is not a type",
			call:       domain.ToolCall{Name: ToolStopBlueprint, Input: map[string]any{"type": "continue", "reason": "did my part"}},
			wantReason: "did my part", wantFlow: true,
		},
		{
			name:       "no type at all",
			call:       domain.ToolCall{Name: ToolStopBlueprint, Input: map[string]any{"reason": "r"}},
			wantReason: "r", wantFlow: true,
		},
		{name: "a sandbox tool is not flow control", call: domain.ToolCall{Name: "bash"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome, reason, isFlow := flowControlOutcome(tc.call, true)
			if isFlow != tc.wantFlow || outcome != tc.wantOutcome || reason != tc.wantReason {
				t.Errorf("-> (%q, %q, %v), want (%q, %q, %v)",
					outcome, reason, isFlow, tc.wantOutcome, tc.wantReason, tc.wantFlow)
			}
			// Without a blueprint the name carries no meaning at all, so the
			// same call is an ordinary (unknown) tool call.
			if _, _, isFlow := flowControlOutcome(tc.call, false); isFlow {
				t.Error("nothing is flow control in a conversation with no blueprint")
			}
		})
	}
}

// TestFlowControlAck_CorrectsRatherThanTerminates pins that a malformed stop
// never lands a terminal state: each correction is an in-band error the
// model can act on.
func TestFlowControlAck_CorrectsRatherThanTerminates(t *testing.T) {
	badType := flowControlAck("", "did my part")
	if !badType.isError || !containsSub(badType.text, "no tool calls") {
		t.Errorf("a bad type must be corrected by naming the real way to hand off: %+v", badType)
	}
	noReason := flowControlAck(domain.RunOutcomeAbort, "")
	if !noReason.isError {
		t.Errorf("a reasonless stop must be refused: %+v", noReason)
	}
	for _, outcome := range []domain.RunOutcome{domain.RunOutcomeAbort, domain.RunOutcomeFinish} {
		if ack := flowControlAck(outcome, "because"); ack.isError {
			t.Errorf("%s with a reason must be accepted: %+v", outcome, ack)
		}
	}
}

func TestBuildSystemPrompt_OrderAndAddendumGating(t *testing.T) {
	parts := EnvelopeParts{TaskContext: "TASKCTX", Envelope: "ENVELOPE", Mission: "MISSION", HasBlueprint: true}

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
		t.Error("a non-terminal step must carry the addendum that redefines what stopping means")
	}
	// The addendum's whole job: say that stopping hands off, and that ending
	// the workflow early is the thing requiring a deliberate call.
	if !containsSub(nonTerminal, "hands off to the step that comes next") {
		t.Error("the addendum must state that stopping normally is the handoff")
	}
	if !containsSub(nonTerminal, `stop_blueprint(type: "finish")`) {
		t.Error("the addendum must name the tool that ends the whole blueprint")
	}
}

// TestBuildSystemPrompt_TasklessHasNoCompletionContract pins the other half
// of the gate. A conversation with a person in it ends when they stop
// writing: there is no run to conclude, no tool to be told about, and no
// mission whose artifact could be missing.
func TestBuildSystemPrompt_TasklessHasNoCompletionContract(t *testing.T) {
	taskless := BuildSystemPrompt(EnvelopeParts{Envelope: "ENVELOPE", Mission: "MISSION"})

	if !containsSub(taskless, "ENVELOPE") || !containsSub(taskless, "MISSION") {
		t.Fatal("the caller's own sections must still be assembled")
	}
	for _, unwanted := range []string{"<completion>", "stop_blueprint", "external artifact"} {
		if containsSub(taskless, unwanted) {
			t.Errorf("a taskless conversation must not be told about %q — there is nobody absent to leave a reason for", unwanted)
		}
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
