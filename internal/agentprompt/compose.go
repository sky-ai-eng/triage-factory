package agentprompt

import (
	"embed"
	"fmt"
	"strings"
)

// blocksFS holds every agent-facing block. Embedding the whole tree (rather
// than one //go:embed per string) is what lets the no-orphans test walk it and
// assert the manifest reaches every file — a block nobody selects is dead
// prompt text, and dead prompt text is worse than dead code because it reads
// like it is in effect.
//
//go:embed blocks
var blocksFS embed.FS

// block reads one block file, panicking when it is missing. Every path comes
// from a manifest constant compiled into this package alongside the embedded
// tree, so a miss is a build-time authoring error surfaced at first use, not a
// runtime condition a caller could handle.
func block(path string) string {
	b, err := blocksFS.ReadFile("blocks/" + path)
	if err != nil {
		panic(fmt.Sprintf("agentprompt: block %q not embedded: %v", path, err))
	}
	return strings.TrimRight(string(b), "\n")
}

// Build composes the agent-facing framework prompt for spec, then appends
// whatever per-run text parts carries.
//
// The result is byte-identical for a fixed Spec (rule 3): the block set, the
// arm each section takes, and the join are all functions of the Spec alone.
// Under RuntimeSDK the composed text is the whole return value — the SDK
// harness delivers mission and task context on channels the caller owns, so
// Parts is ignored there and callers pass Parts{}.
//
// Panics on a Spec no arm covers. The Spec is assembled from typed constants
// at a handful of call sites, so an unrepresentable combination is a wiring
// bug that must fail on the first run rather than degrade a live agent's
// instructions.
func Build(spec Spec, parts Parts) string {
	paths, err := manifest(spec)
	if err != nil {
		panic(err.Error())
	}
	sections := make([]string, 0, len(paths)+3)
	for _, p := range paths {
		sections = append(sections, block(p))
	}
	sections = append(sections, runParts(spec, parts)...)
	return strings.Join(sections, "\n\n") + "\n"
}

// runParts renders the per-run tail. Only the native runtime carries this
// material in the framework prompt; the SDK's mission and task context reach
// the model through the user message its caller composes, so returning nothing
// there is what keeps Build's SDK output equal to the static prefix.
func runParts(spec Spec, parts Parts) []string {
	if spec.Runtime != RuntimeNative {
		return nil
	}
	var out []string
	for _, s := range []string{parts.RunContext, parts.TaskContext, parts.Mission} {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// NonTerminalCompletion returns the completion-contract addendum for a step
// that hands off to a later blueprint step instead of concluding the run, or
// "" when spec's runtime has no such block yet.
//
// This is a second entry point rather than a Parts flag because the SDK
// delivers it as --append-system-prompt: a different channel, resolved at a
// different point, from the composed prompt Build returns. Folding it into
// Build would have to pick one of the two, and either choice moves text the
// caller cannot move back.
func NonTerminalCompletion(spec Spec) string {
	path := nonTerminalBlock(spec)
	if path == "" {
		return ""
	}
	return block(path) + "\n"
}
