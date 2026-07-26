package agentloop

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop/tooldefs"
)

func TestSandboxTools_AreTheSevenInHarnessOrder(t *testing.T) {
	tools := SandboxTools()
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
	tools := SandboxTools()
	// `read` is declared path, offset, limit in the harness.
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

// TestSandboxTools_PreserveNestedPropertyOrder pins that ordering holds at
// depth too. `edit`'s replacement objects are declared oldText then newText,
// which is the order the model reads them in and the order it tends to write
// them in; a map-ordered render would flip it to alphabetical.
func TestSandboxTools_PreserveNestedPropertyOrder(t *testing.T) {
	for _, tool := range SandboxTools() {
		if tool.Function == nil || tool.Function.Name != "edit" {
			continue
		}
		edits, ok := tool.Function.Parameters.Properties.Get("edits")
		if !ok {
			t.Fatal("edit has no edits property")
		}
		raw, err := json.Marshal(edits)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(raw), `"oldText"`; !strings.Contains(got, want) {
			t.Fatalf("edits schema is missing %s: %s", want, got)
		}
		if strings.Index(string(raw), `"oldText"`) > strings.Index(string(raw), `"newText"`) {
			t.Errorf("nested property order is alphabetical, want declaration order: %s", raw)
		}
		return
	}
	t.Fatal("the edit tool is missing")
}

// TestToolDefinitions_MatchHarness is the guard on the seam this package's
// registry opens: the definitions the model reads are Go, and the code they
// describe is Rust in the jail. Nothing but this test stops the two from
// telling different stories.
//
// It compares the whole document — names, order, descriptions, every nested
// property and its order — because all of it reaches the provider verbatim.
//
// It skips when cargo is unavailable, so the default `go test ./...` needs no
// Rust toolchain. That is a real gap, not a nicety: on a machine without
// cargo a divergence ships silently, so anything touching either side should
// be run somewhere cargo exists.
func TestToolDefinitions_MatchHarness(t *testing.T) {
	cargo, err := exec.LookPath("cargo")
	if err != nil {
		t.Skip("cargo is not available; the harness definitions cannot be regenerated here")
	}
	crate, err := filepath.Abs(filepath.Join("..", "..", "harness", "tf-harness-tools"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(cargo, "run", "--quiet", "--bin", "tf-harness-tools", "--", "--definitions")
	cmd.Dir = crate
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("could not run the harness to emit definitions: %v", err)
	}

	registry := make([]toolDoc, 0, 7)
	for _, tool := range SandboxTools() {
		params, err := json.Marshal(tool.Function.Parameters)
		if err != nil {
			t.Fatal(err)
		}
		registry = append(registry, toolDoc{
			Name:        tool.Function.Name,
			Description: *tool.Function.Description,
			Parameters:  params,
		})
	}
	mine, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}

	// Both sides go through the same key-sorting round trip, so a difference
	// in the bytes is a difference in the definitions rather than in how each
	// side happened to lay them out. Ordering is deliberately not compared
	// here — it is pinned by the explicit order tests, which say what the
	// right order is instead of merely that the two sides agree.
	want, got := canonicalJSON(t, out), canonicalJSON(t, mine)
	if got != want {
		t.Fatalf("internal/agentloop/tooldefs has drifted from the harness.\n"+
			"The model would be told one thing and the jail would do another.\n\n"+
			"tooldefs says:\n%s\n\nharness/tf-harness-tools says:\n%s", got, want)
	}
}

// toolDoc is the harness's `--definitions` record shape.
type toolDoc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// canonicalJSON re-renders a document with every object's keys sorted, which
// is what encoding/json does for a map.
func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestBuildSystemPrompt_OrderAndAddendumGating(t *testing.T) {
	// Every assertion here is against an embedded prompt file as a whole,
	// never a sentence inside one. The wording is hand-iterated and expected
	// to churn; what must not churn is which sections are assembled, in what
	// order, under which flags.
	base := strings.TrimSpace(machinistSystemPrompt)
	completion := strings.TrimSpace(completionBlueprint)
	addendum := strings.TrimSpace(blueprintStepNonterminal)

	parts := EnvelopeParts{TaskContext: "TASKCTX", Envelope: "ENVELOPE", Mission: "MISSION", HasBlueprint: true}

	terminal := BuildSystemPrompt(parts)
	for name, want := range map[string]string{
		"base prompt":         base,
		"task context":        "TASKCTX",
		"envelope":            "ENVELOPE",
		"mission":             "MISSION",
		"completion contract": completion,
	} {
		if !containsSub(terminal, want) {
			t.Errorf("the terminal system prompt is missing the %s", name)
		}
	}
	if containsSub(terminal, addendum) {
		t.Error("a terminal step must not carry the non-terminal addendum")
	}
	// Order is what makes the prefix cacheable: the bytes every run in the
	// fleet shares come first, everything that varies per run after them.
	if !inOrder(terminal, base, "TASKCTX", "ENVELOPE", "MISSION", completion) {
		t.Error("the fixed assembly order is what makes the prefix cacheable")
	}

	parts.NonTerminalStep = true
	if nonTerminal := BuildSystemPrompt(parts); !containsSub(nonTerminal, addendum) {
		t.Error("a non-terminal step must carry the addendum that redefines what stopping means")
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
	if containsSub(taskless, strings.TrimSpace(completionBlueprint)) {
		t.Error("a taskless conversation must not be handed the completion contract — there is nobody absent to leave a reason for")
	}
	// The tool's wire name, unlike the prose around it, is a stable identifier:
	// naming it here at all would mean describing a tool this conversation was
	// never given.
	if containsSub(taskless, tooldefs.StopBlueprintName) {
		t.Error("a taskless conversation must never be told about a tool it does not have")
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
