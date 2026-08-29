package agentloop

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

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
	if strings.Contains(want, toolHostConfigureTool) {
		t.Fatalf("the harness exports %q as a tool definition; it is a serve-layer verb "+
			"and must stay out of what the model reads:\n%s", toolHostConfigureTool, want)
	}
}

// TestSandboxTools_OmitTheConfigureVerb is the cheap half of the same guard —
// no cargo needed, so it runs everywhere.
//
// The configure frame carries policy the model must not choose, and the tool
// definitions are the cached prefix every conversation in the fleet shares.
// A knob that leaked into them would be both a capability nobody meant to
// grant and a fleet-wide cache invalidation.
func TestSandboxTools_OmitTheConfigureVerb(t *testing.T) {
	defs, err := json.Marshal(SandboxTools())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(defs), toolHostConfigureTool) {
		t.Fatalf("%q appears in the model-facing tool definitions: %s", toolHostConfigureTool, defs)
	}
	for _, tool := range SandboxTools() {
		if tool.Function != nil && tool.Function.Name == toolHostConfigureTool {
			t.Fatalf("%q is registered as a model-facing tool", toolHostConfigureTool)
		}
	}
}

// TestBash_DeclaresItsSummaryParams pins the shape of the two params the
// interface renders in a shell command's place. The guard above already
// proves the jail declares them identically; this says what they have to be
// on both sides of it — strings, and never required, so a model that authors
// no summary still makes a valid call.
func TestBash_DeclaresItsSummaryParams(t *testing.T) {
	var bash *schemas.ChatToolFunction
	for _, tool := range SandboxTools() {
		if tool.Function != nil && tool.Function.Name == tooldefs.Bash.Name {
			bash = tool.Function
		}
	}
	if bash == nil {
		t.Fatalf("the %s tool is missing", tooldefs.Bash.Name)
	}
	for _, name := range []string{"description", "description_past"} {
		node, ok := bash.Parameters.Properties.Get(name)
		if !ok {
			t.Fatalf("%q is absent from the schema the model reads", name)
		}
		raw, err := json.Marshal(node)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"type":"string"`) {
			t.Errorf("%q is not a string: %s", name, raw)
		}
		if slices.Contains(bash.Parameters.Required, name) {
			t.Errorf("%q is required; an unauthored summary must still make a valid call", name)
		}
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
