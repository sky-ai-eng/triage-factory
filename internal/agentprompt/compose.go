package agentprompt

import (
	"embed"
	"fmt"
	"io/fs"
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

// blockText is every block read out of the embedded tree once, at init.
//
// embed.FS.ReadFile copies its bytes on every call, so reading through the FS
// per composition would allocate the whole block set on each Build — and would
// defer a missing-file panic to the first agent dispatch instead of raising it
// at process start. Reading once buys both: block() is a map lookup returning a
// shared string, and a broken tree fails before any run is claimed.
var blockText = func() map[string]string {
	out := map[string]string{}
	err := fs.WalkDir(blocksFS, "blocks", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, readErr := blocksFS.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		out[strings.TrimPrefix(p, "blocks/")] = strings.TrimRight(string(b), "\n")
		return nil
	})
	if err != nil {
		panic("agentprompt: read embedded blocks: " + err.Error())
	}
	return out
}()

// block returns one block's text, panicking when it is missing. Every path
// comes from a manifest constant compiled into this package alongside the
// embedded tree, so a miss is an authoring error, not a runtime condition a
// caller could handle.
func block(path string) string {
	text, ok := blockText[path]
	if !ok {
		panic(fmt.Sprintf("agentprompt: block %q not embedded", path))
	}
	return text
}

// staticPrompts is the composed block set for every Spec the manifest accepts,
// built once at init.
//
// Rule 3 says Build is byte-identical for a fixed Spec; that makes the
// composition a pure function of the Spec, so there is no reason to redo the
// manifest walk and the join on every dispatch. Enumerating here rather than
// memoizing on demand keeps the work at process start and needs no lock.
//
// A Spec absent from this map (an unknown Family, say) still resolves through
// Build's manifest call, which produces the real error — the cache is an
// optimization, never the authority on what is representable.
var staticPrompts = func() map[Spec]string {
	out := map[Spec]string{}
	for _, surface := range []Surface{SurfaceMachinist, SurfaceCurator} {
		for _, runtime := range []Runtime{RuntimeSDK, RuntimeNative} {
			for _, mode := range []Mode{ModeLocal, ModeMulti} {
				spec := Spec{Surface: surface, Runtime: runtime, Family: FamilyClaude, Mode: mode}
				paths, err := manifest(spec)
				if err != nil {
					continue // an arm this surface/runtime pair doesn't cover
				}
				out[spec] = composeBlocks(paths)
			}
		}
	}
	return out
}()

// composeBlocks joins a manifest's blocks into the static composition: one
// blank line between sections, one trailing newline.
func composeBlocks(paths []string) string {
	sections := make([]string, 0, len(paths))
	for _, p := range paths {
		sections = append(sections, block(p))
	}
	return strings.Join(sections, "\n\n")
}

// Build composes the agent-facing framework prompt for spec, plus the one
// spec-static addendum parts can select.
//
// The result is byte-identical for a fixed Spec and Parts (rule 3): the block
// set, the arm each section takes, and the join are all functions of the Spec
// alone, and Parts names a block rather than carrying text. No per-run material
// reaches this function at all — the mission and the task context ride the
// conversation's opening turn. Under RuntimeSDK the composed text is the whole
// return value, since that harness delivers the addendum on a channel the
// caller owns, so callers pass Parts{} and the return is the init-time string
// with no per-call work at all.
//
// Panics on a Spec no arm covers. The Spec is assembled from typed constants
// at a handful of call sites, so an unrepresentable combination is a wiring
// bug that must fail on the first run rather than degrade a live agent's
// instructions.
func Build(spec Spec, parts Parts) string {
	static, ok := staticPrompts[spec]
	if !ok {
		paths, err := manifest(spec)
		if err != nil {
			panic(err.Error())
		}
		static = composeBlocks(paths)
	}
	tail := runParts(spec, parts)
	if len(tail) == 0 {
		return static + "\n"
	}
	return static + "\n\n" + strings.Join(tail, "\n\n") + "\n"
}

// runParts renders everything that follows the cached prefix — on the native
// runtime, the handoff addendum and nothing else.
//
// Appending rather than composing in is the whole reason it is not a block: it
// has to sit after the completion contract it amends, and putting it inside the
// static set would fork that set into terminal and non-terminal variants — two
// cache entries for a few hundred bytes. The SDK reaches it through
// NonTerminalCompletion instead, because its harness delivers the addendum on
// a channel of its own.
func runParts(spec Spec, parts Parts) []string {
	if spec.Runtime != RuntimeNative || !parts.NonTerminalStep {
		return nil
	}
	addendum := strings.TrimSpace(NonTerminalCompletion(spec))
	if addendum == "" {
		return nil
	}
	return []string{addendum}
}

// nonTerminalCompletions is the handoff addendum per runtime, resolved once at
// init for the same reason staticPrompts is — it is read on every non-final
// blueprint step dispatch and never varies.
var nonTerminalCompletions = func() map[Runtime]string {
	out := map[Runtime]string{}
	for _, runtime := range []Runtime{RuntimeSDK, RuntimeNative} {
		if p := nonTerminalBlock(Spec{Runtime: runtime}); p != "" {
			out[runtime] = block(p) + "\n"
		}
	}
	return out
}()

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
	return nonTerminalCompletions[spec.Runtime]
}
