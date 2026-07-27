package agentprompt

import (
	"io/fs"
	"path"
	"strings"
	"testing"
)

func machinistSpec(mode Mode) Spec {
	return Spec{Surface: SurfaceMachinist, Runtime: RuntimeSDK, Family: FamilyClaude, Mode: mode}
}

// allSpecs enumerates every Spec the manifest covers. The mode/runtime tests
// and the no-orphans walk both drive off this, so a new arm added to the
// manifest without a matching entry here shows up as an orphaned block rather
// than passing silently.
func allSpecs() []Spec {
	var out []Spec
	for _, mode := range []Mode{ModeLocal, ModeMulti} {
		for _, rt := range []Runtime{RuntimeSDK, RuntimeNative} {
			out = append(out, Spec{Surface: SurfaceMachinist, Runtime: rt, Family: FamilyClaude, Mode: mode})
		}
		out = append(out, Spec{Surface: SurfaceCurator, Runtime: RuntimeSDK, Family: FamilyClaude, Mode: mode})
	}
	return out
}

// TestBuild_StableForSpec is the cacheable-prefix property (rule 3): a fixed
// Spec composes to the same bytes every time. If this ever fails, the prefix
// stops being reusable across runs and processes and every run pays a fresh
// prompt-cache write.
func TestBuild_StableForSpec(t *testing.T) {
	for _, spec := range allSpecs() {
		first := Build(spec, Parts{})
		second := Build(spec, Parts{})
		if first != second {
			t.Errorf("Build(%+v) is not stable across calls", spec)
		}
		if strings.TrimSpace(first) == "" {
			t.Errorf("Build(%+v) composed to empty text", spec)
		}
	}
}

// TestBuild_ModeArmsDiverge is the reason the mode axis exists. Both modes now
// check the same base-branch push policy, but they enforce it at opposite
// postures — the multi proxy fails closed, the local pre-push hook fails open —
// and only multi has an egress allowlist and a scoped per-run credential. A
// single text serving both would have to assert something false in one.
func TestBuild_ModeArmsDiverge(t *testing.T) {
	local := Build(machinistSpec(ModeLocal), Parts{})
	multi := Build(machinistSpec(ModeMulti), Parts{})
	if local == multi {
		t.Fatal("ModeLocal and ModeMulti composed identical text; the mode arms are not wired")
	}
	if !strings.Contains(multi, "fails closed") {
		t.Error("multi-mode text does not state that its gate fails closed")
	}
	if !strings.Contains(local, "fails open") {
		t.Error("local-mode text does not state that its check fails open — an agent told otherwise would over-trust it")
	}
	// The credential difference is the other half, and it survives 691: multi
	// scopes per run, local pushes as the operator.
	if !strings.Contains(local, "as the operator") {
		t.Error("local-mode text does not state that raw git runs as the operator")
	}
}

// TestBuild_SDKIgnoresParts pins the channel split: under the SDK runtime the
// mission and task context reach the model through the caller's user message,
// so Build's output must not vary with Parts — otherwise the same text would
// arrive twice and the prefix would stop being static.
func TestBuild_SDKIgnoresParts(t *testing.T) {
	spec := machinistSpec(ModeMulti)
	bare := Build(spec, Parts{})
	loaded := Build(spec, Parts{RunContext: "RC", TaskContext: "TC", Mission: "M", NonTerminalStep: true})
	if bare != loaded {
		t.Error("SDK Build varied with Parts; per-run text belongs in the caller's user message")
	}
}

// TestBuild_NativeAppendsParts pins the seam the native loop engine drops
// into: its per-run material is carried in the framework prompt, appended
// after the static prefix so the prefix stays cacheable.
func TestBuild_NativeAppendsParts(t *testing.T) {
	spec := Spec{Surface: SurfaceMachinist, Runtime: RuntimeNative, Family: FamilyClaude, Mode: ModeMulti}
	prefix := Build(spec, Parts{})
	full := Build(spec, Parts{RunContext: "RUN-CTX", TaskContext: "TASK-CTX", Mission: "MISSION"})
	if !strings.HasPrefix(full, strings.TrimSuffix(prefix, "\n")) {
		t.Error("native Build did not keep the static composition as a prefix")
	}
	for _, want := range []string{"RUN-CTX", "TASK-CTX", "MISSION"} {
		if !strings.Contains(full, want) {
			t.Errorf("native Build dropped %q", want)
		}
	}
}

// TestNonTerminalCompletion_RuntimeArms: the SDK carries the handoff addendum
// on --append-system-prompt; the native arm has no block yet and must return
// empty rather than the SDK's JSON-envelope framing.
func TestNonTerminalCompletion_RuntimeArms(t *testing.T) {
	sdk := NonTerminalCompletion(machinistSpec(ModeMulti))
	if !strings.Contains(sdk, `"outcome": "continue"`) {
		t.Error("SDK non-terminal addendum does not offer the continue outcome")
	}
	native := NonTerminalCompletion(Spec{Surface: SurfaceMachinist, Runtime: RuntimeNative, Family: FamilyClaude, Mode: ModeMulti})
	if native != "" {
		t.Errorf("native non-terminal addendum should be empty until the native loop engine lands; got %q", native)
	}
}

// interpolatedBlocks is the shrinking allowlist of blocks still carrying
// {{PLACEHOLDER}} interpolation. Every other block is enforced literal.
//
// These are resolved by delegate.BuildPromptReplacer and curator.renderEnvelope
// today. The follow-up that deletes the placeholder system empties this list;
// an allowlist that must shrink keeps the debt visible instead of silent, so
// entries are removed here and never added.
var interpolatedBlocks = map[string]bool{
	"identity/machinist.txt":      true, // {{SCOPE}}, {{BINARY_PATH}}
	"identity/curator.txt":        true, // {{PROJECT_NAME}}, {{PROJECT_DESCRIPTION}}, {{PINNED_REPOS_BLOCK}}, {{TRACKERS_BLOCK}}
	"harness/sdk-claude-code.txt": true, // {{TOOLS_REFERENCE}}
	"harness/sdk-curator.txt":     true, // {{BINARY_PATH}}, {{TOOLS_REFERENCE}}
	"guardrails/common.txt":       true, // {{BRANCH_TEMPLATE}}
	"verbs/project-knowledge.txt": true, // {{RUN_ROOT}}
	"verbs/memory.txt":            true, // {{RUN_ROOT}}, {{BINARY_PATH}}
	"tools/github.txt":            true, // {{BINARY_PATH}}, {{RUN_ROOT}}
	"tools/jira.txt":              true, // {{BINARY_PATH}}, {{RUN_ROOT}}, {{BRANCH_TEMPLATE}}
}

// TestBlocks_NoUnexpectedPlaceholders enforces the no-placeholder rule by
// default and names the exceptions explicitly. It fails in both directions: a
// literal `{{` in a block outside the list, and a listed block that no longer
// carries one (so the allowlist can only shrink).
func TestBlocks_NoUnexpectedPlaceholders(t *testing.T) {
	for _, p := range walkBlocks(t) {
		body, err := blocksFS.ReadFile(path.Join("blocks", p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		has := strings.Contains(string(body), "{{")
		switch {
		case has && !interpolatedBlocks[p]:
			t.Errorf("block %s carries a {{placeholder}} but is not on the interpolation allowlist", p)
		case !has && interpolatedBlocks[p]:
			t.Errorf("block %s is on the interpolation allowlist but has no {{placeholder}} left — remove the entry", p)
		}
	}
}

// TestStaticPrompts_CoversEveryManifestSpec keeps the init-time cache in step
// with the manifest. A Spec the cache misses still composes correctly — Build
// falls back to the manifest — so a gap would be silent, showing up only as
// per-dispatch work the design says is unnecessary. Adding an axis arm without
// extending the enumeration in compose.go fails here instead.
func TestStaticPrompts_CoversEveryManifestSpec(t *testing.T) {
	for _, spec := range allSpecs() {
		if _, ok := staticPrompts[spec]; !ok {
			t.Errorf("spec %+v resolves through the manifest but is missing from the init-time cache", spec)
		}
	}
}

// TestBlocks_NoOrphans asserts every embedded block is reachable from the
// manifest or one of the named accessors. A block nobody selects is dead
// prompt text, which is worse than dead code: it reads like it is in effect.
func TestBlocks_NoOrphans(t *testing.T) {
	referenced := map[string]bool{
		blockToolsGitHub: true,
		blockToolsJira:   true,
	}
	for _, spec := range allSpecs() {
		paths, err := manifest(spec)
		if err != nil {
			t.Fatalf("manifest(%+v): %v", spec, err)
		}
		for _, p := range paths {
			referenced[p] = true
		}
		if p := nonTerminalBlock(spec); p != "" {
			referenced[p] = true
		}
	}
	for _, p := range walkBlocks(t) {
		if !referenced[p] {
			t.Errorf("block %s is not referenced by any manifest arm or accessor", p)
		}
	}
}

// walkBlocks lists every embedded block path, relative to blocks/.
func walkBlocks(t *testing.T) []string {
	t.Helper()
	var out []string
	err := fs.WalkDir(blocksFS, "blocks", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		out = append(out, strings.TrimPrefix(p, "blocks/"))
		return nil
	})
	if err != nil {
		t.Fatalf("walk blocks: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no blocks embedded")
	}
	return out
}
