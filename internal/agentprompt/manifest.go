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
	blockIdentityCurator     = "identity/curator.txt"
	blockIdentityCuratorTone = "identity/curator-style.txt"

	// runtime
	blockHarnessSDK        = "harness/sdk-claude-code.txt"
	blockHarnessSDKScratch = "harness/sdk-scratch.txt"
	blockHarnessSDKCurator = "harness/sdk-curator.txt"

	// invariant
	blockVerbLinkedContext   = "verbs/linked-context.txt"
	blockVerbWorkspace       = "verbs/workspace.txt"
	blockVerbProjectKnow     = "verbs/project-knowledge.txt"
	blockVerbMemory          = "verbs/memory.txt"
	blockVerbCuratorChanges  = "verbs/curator-context-changes.txt"
	blockGuardrailsCommon    = "guardrails/common.txt"
	blockCompletionSDKJSON   = "completion/sdk-json.txt"
	blockCompletionSDKNonTrm = "completion/nonterminal-sdk.txt"

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
		return machinistBlocks(spec), nil
	case SurfaceCurator:
		return curatorBlocks(spec)
	default:
		return nil, fmt.Errorf("agentprompt: unknown surface %q", spec.Surface)
	}
}

// machinistBlocks composes the delegated agent's framework prompt.
//
// The native arm resolves today to the runtime-invariant subset: the harness
// and completion sections are the SDK's own, and the native loop engine brings
// its replacements (harness/native.txt, completion/native-blueprint.txt,
// completion/nonterminal-native.txt) as new files dropped into the sections
// already named here. That is the point of shipping both arms now — the axis
// exists, so the engine adds files rather than retrofitting a seam.
func machinistBlocks(spec Spec) []string {
	blocks := []string{blockIdentityMachinist}
	if spec.Runtime == RuntimeSDK {
		blocks = append(blocks, blockHarnessSDK)
	}
	blocks = append(blocks,
		blockVerbLinkedContext,
		blockVerbWorkspace,
		blockVerbProjectKnow,
		blockGuardrailsCommon,
		gitAccessFor(spec.Mode),
		isolationFor(spec.Mode),
	)
	if spec.Runtime == RuntimeSDK {
		blocks = append(blocks, blockHarnessSDKScratch)
	}
	blocks = append(blocks, blockVerbMemory)
	if spec.Runtime == RuntimeSDK {
		blocks = append(blocks, blockCompletionSDKJSON)
	}
	return blocks
}

// curatorBlocks composes the per-project chat assistant's framework prompt.
// The curator has no worktree, no push, and no jail of its own, so it takes
// neither mode arm — its text asserts nothing that mode could falsify.
func curatorBlocks(spec Spec) ([]string, error) {
	if spec.Runtime != RuntimeSDK {
		return nil, fmt.Errorf("agentprompt: curator has no %q runtime block set", spec.Runtime)
	}
	return []string{
		blockIdentityCurator,
		blockHarnessSDKCurator,
		blockVerbCuratorChanges,
		blockIdentityCuratorTone,
	}, nil
}

// nonTerminalBlock returns the completion-contract addendum for a non-terminal
// blueprint step under spec's runtime, or "" when that runtime has none yet.
// It is a separate selection rather than a manifest section because the SDK
// delivers it on its own channel (--append-system-prompt), at a different
// point in the prompt, from the composed framework text.
func nonTerminalBlock(spec Spec) string {
	if spec.Runtime == RuntimeSDK {
		return blockCompletionSDKNonTrm
	}
	// The native arm's nonterminal-native.txt arrives with the native loop
	// engine; until then a native step gets no addendum rather than the SDK's
	// (whose JSON-envelope framing does not apply).
	return ""
}
