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
	blockIdentityWorkbench   = "identity/workbench.txt"
	blockIdentityWorkbenchNv = "identity/workbench-native.txt"

	// The workbench's own counterparts of three surface-invariant sections.
	// Its memory contract has no completion gate to write against (TF files
	// the file at every turn end, so the blocks ask for it to be kept current
	// rather than written before finishing), and its communication block is
	// written for a person reading as the agent works.
	blockVerbMemoryWorkbench     = "verbs/memory-workbench.txt"
	blockVerbMemoryWorkbenchNv   = "verbs/memory-workbench-native.txt"
	blockPracticesCommsWorkbench = "practices/communication-workbench.txt"

	// runtime
	blockHarnessSDK           = "harness/sdk-claude-code.txt"
	blockHarnessSDKScratch    = "harness/sdk-scratch.txt"
	blockHarnessNative        = "harness/native.txt"
	blockHarnessNativeGH      = "harness/native-gh.txt"
	blockHarnessNativeVerbs   = "harness/native-verbs.txt"
	blockHarnessNativeScratch = "harness/native-scratch.txt"

	// invariant
	blockVerbLinkedContext = "verbs/linked-context.txt"
	blockVerbWorkspace     = "verbs/workspace.txt"
	blockVerbTeamKnowledge = "verbs/team-knowledge.txt"
	blockVerbMemory        = "verbs/memory.txt"
	blockGuardrailsCommon  = "guardrails/common.txt"

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

	// Shared by both runtimes, which conclude a run the same way: a turn that
	// ends with no tool calls is the step's part done, and stop_blueprint is
	// the one deliberate other ending. The text names the tool bare; the SDK
	// serves it as mcp__tf__stop_blueprint, which the model connects to that
	// name.
	blockCompletion            = "completion/blueprint.txt"
	blockCompletionNonTerminal = "completion/nonterminal.txt"

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
	blockToolsGitHub    = "tools/github.txt"
	blockToolsJira      = "tools/jira.txt"
	blockToolsLinear    = "tools/linear.txt"
	blockToolsWorkspace = "tools/workspace.txt"
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
// run, then the rules, then the completion contract, on a surface that has
// one, last so it is the final thing in the window.
func manifest(spec Spec) ([]string, error) {
	if spec.Family != FamilyClaude {
		return nil, fmt.Errorf("agentprompt: no block set for model family %q", spec.Family)
	}
	switch spec.Surface {
	case SurfaceMachinist:
		return machinistBlocks(spec)
	case SurfaceWorkbench:
		return workbenchBlocks(spec)
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
		blockCompletion,
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
		blockCompletion,
	}, nil
}

// workbenchBlocks composes the prompt for the agent a person works with on a
// task. Each arm is the machinist's, section for section, with identity,
// communication and memory resolved to the workbench's files and the
// completion contract dropped: the conversation has no blueprint to stop, so
// there is no ending to explain.
//
// The SDK arm has no communication section, as the machinist's has none: TF
// appends to Claude Code's own system prompt, which is written for a person at
// the keyboard.
func workbenchBlocks(spec Spec) ([]string, error) {
	if spec.Runtime == RuntimeNative {
		return nativeWorkbenchBlocks(spec)
	}
	return []string{
		blockIdentityWorkbench,
		blockHarnessSDK,
		blockVerbLinkedContext,
		blockVerbWorkspace,
		blockVerbTeamKnowledge,
		blockGuardrailsCommon,
		gitAccessFor(spec.Mode),
		isolationFor(spec.Mode),
		blockHarnessSDKScratch,
		blockVerbMemoryWorkbench,
	}, nil
}

// nativeWorkbenchBlocks is the native loop's workbench arm. It requires multi
// for the reason nativeMachinistBlocks does: its blocks name the in-jail
// paths, which exist only behind a sandboxed launch.
func nativeWorkbenchBlocks(spec Spec) ([]string, error) {
	if spec.Mode != ModeMulti {
		return nil, fmt.Errorf("agentprompt: the native runtime has no %q block set — a native engagement is always sandboxed", spec.Mode)
	}
	return []string{
		blockIdentityWorkbenchNv,
		blockHarnessNative,
		blockPracticesCoding,
		blockPracticesCommsWorkbench,
		blockHarnessNativeGH,
		blockHarnessNativeVerbs,
		blockVerbLinkedContextNv,
		blockVerbWorkspaceNv,
		blockVerbTeamKnowledgeNv,
		blockGuardrailsCommonNv,
		gitAccessFor(spec.Mode),
		isolationFor(spec.Mode),
		blockHarnessNativeScratch,
		blockVerbMemoryWorkbenchNv,
	}, nil
}
