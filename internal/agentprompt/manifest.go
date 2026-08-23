package agentprompt

import "fmt"

// The manifest is Go, not a config format (rule 5): selection is explicit code
// a reviewer can read top-to-bottom, and an unrepresentable combination fails
// at the switch rather than in a lookup table.
//
// Every path below is relative to blocks/. A block file varies on at most one
// axis (rule 2), so which arm a section takes is decided here and nowhere
// else — the file itself never branches.

// Section paths, named so the manifest reads as prose and a rename is one
// edit. Grouped by the axis the section varies on.
const (
	// surface
	blockIdentityMachinist   = "identity/machinist.txt"
	blockIdentityMachinistNv = "identity/machinist-native.txt"

	// runtime
	blockHarnessSDK           = "harness/sdk-claude-code.txt"
	blockHarnessSDKScratch    = "harness/sdk-scratch.txt"
	blockHarnessNative        = "harness/native.txt"
	blockHarnessNativeGH      = "harness/native-gh.txt"
	blockHarnessNativeVerbs   = "harness/native-verbs.txt"
	blockHarnessNativeScratch = "harness/native-scratch.txt"

	// invariant
	blockVerbLinkedContext   = "verbs/linked-context.txt"
	blockVerbWorkspace       = "verbs/workspace.txt"
	blockVerbTeamKnowledge   = "verbs/team-knowledge.txt"
	blockVerbMemory          = "verbs/memory.txt"
	blockGuardrailsCommon    = "guardrails/common.txt"
	blockCompletionSDKJSON   = "completion/sdk-json.txt"
	blockCompletionSDKNonTrm = "completion/nonterminal-sdk.txt"

	// The native counterparts of the sections above. They are separate files
	// rather than arms of the same file because the two runtimes are different
	// harnesses that happen to discuss the same subjects: the SDK's text names
	// the wrapper verbs (`exec gh pr view`) and reaches its scratch dir through
	// the run root, while the native loop's names the real `gh` and the fixed
	// in-jail paths. Merging a converged pair is a text edit, not a structural
	// one.
	blockVerbLinkedContextNv = "verbs/linked-context-native.txt"
	blockVerbWorkspaceNv     = "verbs/workspace-native.txt"
	blockVerbTeamKnowledgeNv = "verbs/team-knowledge-native.txt"
	blockVerbMemoryNv        = "verbs/memory-native.txt"
	blockGuardrailsCommonNv  = "guardrails/common-native.txt"
	blockCompletionNative    = "completion/native-blueprint.txt"
	blockCompletionNativeNT  = "completion/nonterminal-native.txt"

	// Native-only today, but nothing in either file is runtime-specific: the
	// SDK gets this material from Claude Code's own base system prompt, which
	// the native loop has no equivalent of and must supply itself. Promoting
	// them to the SDK arm is a prompt-quality question, deliberately not
	// bundled with a package move.
	blockPracticesCoding = "practices/coding.txt"
	blockPracticesComms  = "practices/communication.txt"

	// mode
	blockGitAccessLocal = "github/local.txt"
	blockGitAccessMulti = "github/multi.txt"
	blockIsolationLocal = "guardrails/local.txt"
	blockIsolationMulti = "guardrails/multi.txt"

	// per-run injected, not manifest-composed (see toolsref.go)
	blockToolsGitHub = "tools/github.txt"
	blockToolsJira   = "tools/jira.txt"
)

// gitAccessFor and isolationFor are the two mode arms. They are separate
// sections because they answer separate questions — what backs `git` and what
// it is allowed to do, versus what the process is wrapped in — and a block
// that answered both would vary on one axis but conflate two subjects.
func gitAccessFor(mode Mode) string {
	if mode == ModeMulti {
		return blockGitAccessMulti
	}
	return blockGitAccessLocal
}

func isolationFor(mode Mode) string {
	if mode == ModeMulti {
		return blockIsolationMulti
	}
	return blockIsolationLocal
}

// manifest returns the ordered block list for spec, or an error for a
// combination no arm covers. Order is the order the agent reads them in, and
// it is load-bearing: identity, then harness, then the verbs that orient a
// run, then the rules, then the completion contract last so it is the final
// thing in the window.
func manifest(spec Spec) ([]string, error) {
	if spec.Family != FamilyClaude {
		return nil, fmt.Errorf("agentprompt: no block set for model family %q", spec.Family)
	}
	switch spec.Surface {
	case SurfaceMachinist:
		return machinistBlocks(spec)
	default:
		return nil, fmt.Errorf("agentprompt: unknown surface %q", spec.Surface)
	}
}

// machinistBlocks composes the delegated agent's framework prompt.
//
// Both runtimes walk the same section order — identity, harness, orientation,
// rules, then the completion contract last — and differ only in which file
// each section resolves to. The two mode sections are the exception and are
// shared verbatim: what backs `git` and what the process is wrapped in are
// facts about the deployment, not about the loop reading them.
func machinistBlocks(spec Spec) ([]string, error) {
	if spec.Runtime == RuntimeNative {
		return nativeMachinistBlocks(spec)
	}
	return []string{
		blockIdentityMachinist,
		blockHarnessSDK,
		blockVerbLinkedContext,
		blockVerbWorkspace,
		blockVerbTeamKnowledge,
		blockGuardrailsCommon,
		gitAccessFor(spec.Mode),
		isolationFor(spec.Mode),
		blockHarnessSDKScratch,
		blockVerbMemory,
		blockCompletionSDKJSON,
	}, nil
}

// nativeMachinistBlocks is the native loop's arm.
//
// It requires multi mode. A native engagement runs its tools inside a gVisor
// jail whose launch hard-requires a prebuilt per-run network, so there is no
// local-mode native run to compose for — and the blocks say so concretely,
// naming the in-jail `/work` root the agent actually sees. Returning an error
// rather than composing a local variant keeps that from becoming the next
// prompt that asserts something untrue: if the native loop ever runs
// unsandboxed, the paths change and this arm has to be written, not inherited.
//
// The completion contract is unconditional because every delegation executes a
// blueprint — a single-step one is still one — so there is no machinist shape
// that lacks it. Gating it on a Parts flag would also make the static prefix
// depend on per-run state, which is exactly what rule 3 forbids.
func nativeMachinistBlocks(spec Spec) ([]string, error) {
	if spec.Mode != ModeMulti {
		return nil, fmt.Errorf("agentprompt: the native runtime has no %q block set — a native engagement is always sandboxed", spec.Mode)
	}
	return []string{
		blockIdentityMachinistNv,
		blockHarnessNative,
		blockPracticesCoding,
		blockPracticesComms,
		blockHarnessNativeGH,
		blockHarnessNativeVerbs,
		blockVerbLinkedContextNv,
		blockVerbWorkspaceNv,
		blockVerbTeamKnowledgeNv,
		blockGuardrailsCommonNv,
		gitAccessFor(spec.Mode),
		isolationFor(spec.Mode),
		blockHarnessNativeScratch,
		blockVerbMemoryNv,
		blockCompletionNative,
	}, nil
}

// nonTerminalBlock returns the completion-contract addendum for a non-terminal
// blueprint step under spec's runtime, or "" when that runtime has none yet.
// It is a separate selection rather than a manifest section because the SDK
// delivers it on its own channel (--append-system-prompt), at a different
// point in the prompt, from the composed framework text.
func nonTerminalBlock(spec Spec) string {
	if spec.Runtime == RuntimeNative {
		return blockCompletionNativeNT
	}
	return blockCompletionSDKNonTrm
}
