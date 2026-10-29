package agentprompt

import (
	"io/fs"
	"path"
	"slices"
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
			// Native is sandboxed by construction, so it has no local arm —
			// see nativeMachinistBlocks, and TestBuild_NativeRequiresMulti
			// below, which pins that this gap is deliberate.
			if rt == RuntimeNative && mode == ModeLocal {
				continue
			}
			out = append(out, Spec{Surface: SurfaceMachinist, Runtime: rt, Family: FamilyClaude, Mode: mode})
		}
	}
	return out
}

// TestBuild_NativeRequiresMulti pins the one combination the manifest refuses.
//
// A native engagement's tools run inside a jail whose launch hard-requires a
// prebuilt per-run network, which exists only in multi mode — so its blocks
// name concrete in-jail paths (`/work/_tfac/...`) that would be wrong anywhere
// else. Refusing is what keeps a native-in-local run from silently composing a
// prompt whose every path is a lie; the launch would fail moments later
// regardless, and failing here says why.
func TestBuild_NativeRequiresMulti(t *testing.T) {
	spec := Spec{Surface: SurfaceMachinist, Runtime: RuntimeNative, Family: FamilyClaude, Mode: ModeLocal}
	if _, err := manifest(spec); err == nil {
		t.Fatal("a native local-mode Spec composed a prompt; its paths only exist inside the jail")
	}
	defer func() {
		if recover() == nil {
			t.Error("Build did not panic on a Spec the manifest refuses")
		}
	}()
	_ = Build(spec)
}

// TestBuild_StableForSpec is the cacheable-prefix property (rule 3): a fixed
// Spec composes to the same bytes every time. If this ever fails, the prefix
// stops being reusable across runs and processes and every run pays a fresh
// prompt-cache write.
func TestBuild_StableForSpec(t *testing.T) {
	for _, spec := range allSpecs() {
		first := Build(spec)
		second := Build(spec)
		if first != second {
			t.Errorf("Build(%+v) is not stable across calls", spec)
		}
		if strings.TrimSpace(first) == "" {
			t.Errorf("Build(%+v) composed to empty text", spec)
		}
	}
}

// TestBuild_ModeArmsDiverge is the reason the mode axis exists: only multi puts
// the agent behind a fail-closed egress boundary, so each mode composes its own
// git-access and isolation sections and never the other mode's.
func TestBuild_ModeArmsDiverge(t *testing.T) {
	for _, tc := range []struct {
		mode       Mode
		own, other []string
	}{
		{ModeLocal, []string{blockGitAccessLocal, blockIsolationLocal}, []string{blockGitAccessMulti, blockIsolationMulti}},
		{ModeMulti, []string{blockGitAccessMulti, blockIsolationMulti}, []string{blockGitAccessLocal, blockIsolationLocal}},
	} {
		paths, err := manifest(machinistSpec(tc.mode))
		if err != nil {
			t.Fatalf("manifest(%s): %v", tc.mode, err)
		}
		for _, p := range tc.own {
			if !slices.Contains(paths, p) {
				t.Errorf("%s does not compose its own %s", tc.mode, p)
			}
		}
		for _, p := range tc.other {
			if slices.Contains(paths, p) {
				t.Errorf("%s composes the other mode's %s", tc.mode, p)
			}
		}
	}
}

// TestBlocks_AreLiteral is the rule that makes a block readable as the text the
// model receives: what is written here is what is sent. A block that carried a
// `{{NAME}}` would be a promise that something, somewhere, fills it in — and a
// user writing their own prompt has no way to make that same promise, so a block
// relying on one would be an example of nothing they could reproduce.
//
// A fact that genuinely varies per run belongs in the tail's <run_context> /
// <task_context> / <tools> sections, which a block refers to by name.
func TestBlocks_AreLiteral(t *testing.T) {
	for _, p := range walkBlocks(t) {
		body, err := blocksFS.ReadFile(path.Join("blocks", p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if strings.Contains(string(body), "{{") {
			t.Errorf("block %s is not literal — it carries a {{...}} token nothing resolves", p)
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
		blockToolsGitHub:    true,
		blockToolsJira:      true,
		blockToolsLinear:    true,
		blockToolsWorkspace: true,
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

// TestBuild_NativeSectionOrder pins the one ordering that is load-bearing
// rather than stylistic: who the agent is comes first and the completion
// contract comes last, so how to finish is the final thing in the composition.
//
// The handoff addendum that amends that contract is deliberately not here. It
// is per step, so it would fork this prefix into a terminal and a non-terminal
// variant — two cache entries where the design wants one. Its caller appends it
// on the per-conversation channel instead, which lands it after the contract
// for the same reason and costs nothing shared.
func TestBuild_NativeSectionOrder(t *testing.T) {
	spec := Spec{Surface: SurfaceMachinist, Runtime: RuntimeNative, Family: FamilyClaude, Mode: ModeMulti}
	got := Build(spec)

	prev := -1
	for _, section := range []string{
		block(blockIdentityMachinistNv),
		block(blockCompletionNative),
	} {
		at := strings.Index(got, section)
		if at < 0 {
			t.Fatalf("composed prompt is missing a section: %.40q", section)
		}
		if at < prev {
			t.Fatalf("section %.40q is out of order", section)
		}
		prev = at
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), block(blockCompletionNative)) {
		t.Error("the completion contract is not the last thing in the composition")
	}
	if strings.Contains(got, strings.TrimSpace(block(blockCompletionNativeNT))) {
		t.Error("the handoff addendum is composed into the shared prefix, forking it per step")
	}
}
