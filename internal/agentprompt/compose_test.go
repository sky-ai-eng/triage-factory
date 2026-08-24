package agentprompt

import (
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
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

// TestBuild_ModeArmsDiverge is the reason the mode axis exists. Both modes now
// mediate the managed Git path, but only multi puts the agent behind a
// fail-closed egress boundary. Local is explicit that its process-scoped
// routing is not a security boundary against the unsandboxed process.
func TestBuild_ModeArmsDiverge(t *testing.T) {
	local := Build(machinistSpec(ModeLocal))
	multi := Build(machinistSpec(ModeMulti))
	if local == multi {
		t.Fatal("ModeLocal and ModeMulti composed identical text; the mode arms are not wired")
	}
	if !strings.Contains(multi, "fails closed") {
		t.Error("multi-mode text does not state that its gate fails closed")
	}
	if !strings.Contains(local, "not a security boundary") {
		t.Error("local-mode text does not state the limit of its managed Git routing")
	}
	if !strings.Contains(local, "configured credential") {
		t.Error("local-mode text does not state that managed Git uses TF's configured identity")
	}
}

// TestBuildNonTerminalStep_SDKIsUnchanged pins the channel split: the SDK
// harness delivers the handoff addendum itself (--append-system-prompt), so
// appending it to the composed text as well would send it twice.
func TestBuildNonTerminalStep_SDKIsUnchanged(t *testing.T) {
	spec := machinistSpec(ModeMulti)
	if Build(spec) != BuildNonTerminalStep(spec) {
		t.Error("the SDK's non-terminal composition grew an addendum its harness already delivers")
	}
}

// TestBuildNonTerminalStep_NativeAppendsOnlyTheAddendum pins what a native
// composition may carry beyond the static blocks: the handoff addendum, and
// nothing else. Everything about the particular run — the mission, the run
// context, the externally-authored task block — reaches the model through the
// opening turn instead, and this package takes no argument that could carry it.
func TestBuildNonTerminalStep_NativeAppendsOnlyTheAddendum(t *testing.T) {
	spec := Spec{Surface: SurfaceMachinist, Runtime: RuntimeNative, Family: FamilyClaude, Mode: ModeMulti}
	terminal := Build(spec)
	handoff := BuildNonTerminalStep(spec)

	addendum := strings.TrimSpace(block(blockCompletionNativeNT))
	if want := strings.TrimSuffix(terminal, "\n") + "\n\n" + addendum + "\n"; handoff != want {
		t.Errorf("a non-terminal step's prompt is not exactly the prefix plus the addendum;\ngot  %q\nwant %q", handoff, want)
	}
	if strings.Contains(terminal, addendum) {
		t.Error("a terminal step carries the handoff addendum")
	}
}

// TestNonTerminalCompletion_RuntimeArms pins that each runtime gets its own
// handoff addendum, because the two describe incompatible ways to end a step.
// The SDK's step emits a JSON envelope carrying a "continue" outcome; the
// native loop's just stops, and stopping IS the handoff. Handing either text
// to the other runtime instructs the model to use a mechanism it does not have.
func TestNonTerminalCompletion_RuntimeArms(t *testing.T) {
	sdk := NonTerminalCompletion(machinistSpec(ModeMulti))
	if !strings.Contains(sdk, `"outcome": "continue"`) {
		t.Error("SDK non-terminal addendum does not offer the continue outcome")
	}
	native := NonTerminalCompletion(Spec{Surface: SurfaceMachinist, Runtime: RuntimeNative, Family: FamilyClaude, Mode: ModeMulti})
	if native == "" {
		t.Fatal("native non-terminal addendum is empty; a native step would be told nothing about handing off")
	}
	if strings.Contains(native, `"outcome"`) {
		t.Error("the native addendum describes the SDK's JSON envelope, which this runtime never emits")
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

// TestBuild_NativeSectionOrder pins the one ordering that is load-bearing
// rather than stylistic: the handoff addendum falls after the completion
// contract it amends, and outside the cached region, so the prefix does not
// fork into terminal and non-terminal variants.
func TestBuild_NativeSectionOrder(t *testing.T) {
	spec := Spec{Surface: SurfaceMachinist, Runtime: RuntimeNative, Family: FamilyClaude, Mode: ModeMulti}
	got := BuildNonTerminalStep(spec)

	prev := -1
	for _, section := range []string{
		block(blockIdentityMachinistNv),
		block(blockCompletionNative),
		strings.TrimSpace(block(blockCompletionNativeNT)),
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
}

// TestNativeBlocks_NameTheRealPaths ties the absolute paths the native blocks
// spell out to the constants that produce them.
//
// Writing them out is what keeps the prefix cacheable, and it carries a drift
// risk in exchange: move the sandbox work root or rename the scratch dir, and
// every native run is told to write its handoff somewhere that does not exist,
// with nothing else failing.
//
// Every mention must also be reached from the fixed root. A bare relative
// `_tfac/` resolves against whatever repo the agent last changed into, which
// is the wrong tree and a file nobody finds again.
func TestNativeBlocks_NameTheRealPaths(t *testing.T) {
	root := agentproc.SandboxWorkRoot + "/" + worktree.ScratchDir
	spec := Spec{Surface: SurfaceMachinist, Runtime: RuntimeNative, Family: FamilyClaude, Mode: ModeMulti}
	paths, err := manifest(spec)
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, blockCompletionNativeNT)

	for _, p := range paths {
		body := block(p)
		if bare, rooted := strings.Count(body, worktree.ScratchDir), strings.Count(body, root); bare != rooted {
			t.Errorf("%s names %q %d times but only %d are under the fixed %q root", p, worktree.ScratchDir, bare, rooted, root)
		}
	}

	// The memory write path is the whole contract with the orchestrator: it
	// reads back exactly this file, so a block naming another one loses the
	// run's memory silently.
	if want := root + "/memory.md"; !strings.Contains(block(blockVerbMemoryNv), want) {
		t.Errorf("the native memory block does not name the write path %q", want)
	}
}

// TestKnowledgeBlock_IsInBothRuntimes pins the pair, because a section that
// exists for only one engine is a section half the fleet never reads — and the
// two runtimes reach the same tree by different paths, so one file cannot serve
// both.
//
// It asserts what the blocks may say and what they may NOT: they describe the
// staged layout, which is fixed, and they never name a document or a folder,
// which is per-run data. A block that listed what an org happens to hold today
// would fork the cached prefix per team.
func TestKnowledgeBlock_IsInBothRuntimes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  Spec
		block string
		root  string
	}{
		{"sdk", Spec{Surface: SurfaceMachinist, Runtime: RuntimeSDK, Family: FamilyClaude, Mode: ModeMulti},
			blockVerbTeamKnowledge, "_tfac/knowledge/"},
		{"native", Spec{Surface: SurfaceMachinist, Runtime: RuntimeNative, Family: FamilyClaude, Mode: ModeMulti},
			blockVerbTeamKnowledgeNv, agentproc.SandboxWorkRoot + "/" + worktree.ScratchDir + "/knowledge/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			composed := Build(tc.spec)
			body := block(tc.block)
			if !strings.Contains(composed, strings.TrimSpace(body)) {
				t.Fatalf("the %s machinist prompt does not carry its knowledge block", tc.name)
			}
			// The staged layout, named the way the staging step writes it.
			for _, want := range []string{tc.root, tc.root + "team/", tc.root + "org/", "private/", "shared/"} {
				if !strings.Contains(body, want) {
					t.Errorf("the %s knowledge block does not name %q", tc.name, want)
				}
			}
			// The block says the tree exists; <run_context> says what is in it.
			if !strings.Contains(body, "run_context") && !strings.Contains(body, "run context") {
				t.Errorf("the %s knowledge block does not point at the run context that lists what was staged", tc.name)
			}
		})
	}
}

// TestNativeGHBlock_ClaimsNoInvisibility guards a claim that used to be true
// and is not any more: every write the native `gh` performs — REST or GraphQL —
// now lands an audit row, so a prompt telling the agent otherwise is teaching it
// something false about the harness it is in.
//
// The redirect it carries is unaffected and stays: the TF review verbs dominate
// `gh pr review` on what they can do, which was always the load-bearing reason.
// This pins only that the justification is not the retired one.
func TestNativeGHBlock_ClaimsNoInvisibility(t *testing.T) {
	body := block(blockHarnessNativeGH)
	for _, claim := range []string{"invisible", "no trace", "not recorded", "unrecorded"} {
		if strings.Contains(strings.ToLower(body), claim) {
			t.Errorf("the native gh block still claims %q; gh-channel writes are audited on both transports", claim)
		}
	}
	// The refusal is still a refusal — losing it would hand the agent a review
	// path that produces nothing the product can stage or route.
	if !strings.Contains(body, "`gh pr review` is refused") {
		t.Error("the native gh block no longer refuses `gh pr review`")
	}
}

// TestNativeGHBlock_RefusesRepoCreation pins the second refusal the harness
// teaches. The gate refuses the command either way, but a model that learns the
// boundary here spends no turn discovering it — and the prompt is the only half
// of the pair the SDK runtime sees at all, since that runtime has no matcher.
func TestNativeGHBlock_RefusesRepoCreation(t *testing.T) {
	body := block(blockHarnessNativeGH)
	if !strings.Contains(body, "`gh repo create` is refused") {
		t.Error("the native gh block does not refuse `gh repo create`")
	}
	// The reason has to be the one that is actually true, since an agent that
	// finds a stated rule false has reason to doubt the rest of the block.
	if !strings.Contains(body, "before the run started") {
		t.Error("the block refuses repo creation without giving the tracked-set reason")
	}
}
