package agentloop

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop/tooldefs"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
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
	envelope := strings.TrimSpace(nativeEnvelope)
	completion := strings.TrimSpace(completionBlueprint)
	addendum := strings.TrimSpace(blueprintStepNonterminal)

	parts := EnvelopeParts{RunContext: "RUNCTX", TaskContext: "TASKCTX", Mission: "MISSION", HasBlueprint: true}

	terminal := BuildSystemPrompt(parts)
	for name, want := range map[string]string{
		"base prompt":         base,
		"standing envelope":   envelope,
		"completion contract": completion,
		"run context":         "RUNCTX",
		"task context":        "TASKCTX",
		"mission":             "MISSION",
	} {
		if !containsSub(terminal, want) {
			t.Errorf("the terminal system prompt is missing the %s", name)
		}
	}
	if containsSub(terminal, addendum) {
		t.Error("a terminal step must not carry the non-terminal addendum")
	}
	// The shared block comes first and in one piece — that contiguity is what
	// makes it a cacheable prefix rather than three fragments — and every
	// per-run section follows it.
	if !inOrder(terminal, base, envelope, completion, "RUNCTX", "TASKCTX", "MISSION") {
		t.Error("the fixed assembly order is what makes the prefix cacheable")
	}
	// The mission is last so the authoritative instruction is read after the
	// externally-authored task context, not before it.
	if !inOrder(terminal, "TASKCTX", "MISSION") {
		t.Error("the mission must come after the untrusted task context")
	}

	parts.NonTerminalStep = true
	nonTerminal := BuildSystemPrompt(parts)
	if !containsSub(nonTerminal, addendum) {
		t.Error("a non-terminal step must carry the addendum that redefines what stopping means")
	}
	// The addendum sits on the boundary: after the whole shared block, before
	// anything per-run. Inside it, the block would fork into two variants.
	if !inOrder(nonTerminal, completion, addendum, "RUNCTX") {
		t.Error("the addendum must fall between the shared block and the per-run sections")
	}
}

// TestNativeEnvelope_CarriesNoPlaceholders is the guard on the property the
// whole split exists for. A single {{...}} in the standing envelope makes it
// per-run text, and the fleet-wide cached prefix silently becomes a per-run
// one — with nothing else failing to reveal it.
func TestNativeEnvelope_CarriesNoPlaceholders(t *testing.T) {
	for name, body := range map[string]string{
		"envelope.txt":                   nativeEnvelope,
		"machinist-system.txt":           machinistSystemPrompt,
		"completion-blueprint.txt":       completionBlueprint,
		"blueprint-step-nonterminal.txt": blueprintStepNonterminal,
	} {
		if containsSub(body, "{{") {
			t.Errorf("%s carries a placeholder; per-run values belong in RunContext", name)
		}
	}
}

// TestNativePrompts_NameTheRealPaths ties the absolute paths the vendored
// prompts spell out to the constants that actually produce them. The prompts
// name them literally because they must stay placeholder-free to be cacheable,
// which trades interpolation for a drift risk: rename the scratch dir or move
// the sandbox work root, and every native run is told to write its handoff
// somewhere that does not exist, with nothing else failing.
//
// It asserts the composed strings appear, not merely the two constants, since
// the failure being guarded is a stale path rather than a missing word.
func TestNativePrompts_NameTheRealPaths(t *testing.T) {
	root := agentproc.SandboxWorkRoot + "/" + worktree.ScratchDir
	for name, body := range map[string]string{
		"envelope.txt":                   nativeEnvelope,
		"machinist-system.txt":           machinistSystemPrompt,
		"blueprint-step-nonterminal.txt": blueprintStepNonterminal,
	} {
		for _, stale := range []string{"_scratch/", "./_tfac", "{{RUN_ROOT}}"} {
			if containsSub(body, stale) {
				t.Errorf("%s still names %q", name, stale)
			}
		}
		// Every occurrence of the scratch dir must be reached from the fixed
		// run root: a bare relative one resolves against whatever the agent
		// last cd'd into, which for a materialized repo is the wrong tree.
		if bare, rooted := strings.Count(body, worktree.ScratchDir), strings.Count(body, root); bare != rooted {
			t.Errorf("%s names %q %d times but only %d are under the fixed %q root", name, worktree.ScratchDir, bare, rooted, root)
		}
	}
	// The memory write path is the whole contract with the orchestrator: it
	// reads back exactly this file, so a prompt naming another one loses the
	// run's memory silently.
	if want := root + "/" + agentMemoryFileNameForTest; !containsSub(nativeEnvelope, want) {
		t.Errorf("the envelope does not name the memory write path %q", want)
	}
}

// agentMemoryFileNameForTest mirrors internal/delegate's unexported
// agentMemoryFileName. Duplicated rather than exported: the constant is that
// package's contract with itself, and widening its visibility to let a test in
// another package read it would be the larger change.
const agentMemoryFileNameForTest = "memory.md"

// TestBuildSystemPrompt_TasklessHasNoCompletionContract pins the other half
// of the gate. A conversation with a person in it ends when they stop
// writing: there is no run to conclude, no tool to be told about, and no
// mission whose artifact could be missing.
func TestBuildSystemPrompt_TasklessHasNoCompletionContract(t *testing.T) {
	taskless := BuildSystemPrompt(EnvelopeParts{RunContext: "RUNCTX", Mission: "MISSION"})

	if !containsSub(taskless, "RUNCTX") || !containsSub(taskless, "MISSION") {
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
