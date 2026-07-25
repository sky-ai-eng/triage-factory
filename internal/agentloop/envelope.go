package agentloop

import (
	_ "embed" // powers the vendored prompt text below
	"strings"
)

// codingAgentSystemPrompt is the base coding-agent system prompt — the
// stable, task-independent half of every native system prompt. It is
// vendored as a file rather than composed in Go because it co-evolved with
// the seven tool definitions and is iterated by hand, not by code. See
// prompts/PROVENANCE.md for its source and current status.
//
//go:embed prompts/coding-agent-system.txt
var codingAgentSystemPrompt string

// completionNative is the native path's terminal contract: stopping is
// concluding, plus the flow-control tools. It replaces the SDK path's JSON
// completion envelope, which this runtime never emits and never parses.
//
//go:embed prompts/completion-native.txt
var completionNative string

// blueprintStepNonterminal is the addendum appended on a non-terminal
// blueprint step — the same guidance the SDK path's addendum carries, minus
// its JSON-envelope references.
//
// It changes no tool: the tool set is the same everywhere. What it changes
// is what an ordinary stop means, since on a non-final step that is a
// handoff rather than the end of the task. Step position is carried here and
// nowhere else, which is why a prompt and a tool list cannot fall out of
// step.
//
//go:embed prompts/blueprint-step-nonterminal.txt
var blueprintStepNonterminal string

// EnvelopeParts are the caller-supplied sections of a native system prompt.
// Assembly is mechanical — this package concatenates in a fixed order and
// does no interpolation, templating, or rewriting. Prompt composition and
// quality are owned by the caller (and by the humans who edit the vendored
// files).
type EnvelopeParts struct {
	// TaskContext is the system-rendered task/entity block. It carries
	// externally-authored text (PR titles, issue bodies), so the caller is
	// responsible for having marked it as untrusted before it gets here.
	TaskContext string
	// Envelope is the runtime-independent TF envelope: scope, tools,
	// guardrails, scratch, entity memory.
	Envelope string
	// Mission is the step's prompt body.
	Mission string
	// NonTerminalStep appends the blueprint addendum, which tells the model
	// that stopping hands off rather than ending the task. This is the only
	// place step position is expressed.
	NonTerminalStep bool
}

// BuildSystemPrompt assembles the native system prompt in the one fixed
// order every native call uses:
//
//	base coding-agent prompt
//	task context
//	TF envelope
//	mission
//	completion contract
//	[non-terminal step addendum]
//
// The order is what makes the prefix cacheable: the base prompt and the tool
// schemas are byte-identical across every run in the fleet, and everything
// that varies comes after them.
func BuildSystemPrompt(parts EnvelopeParts) string {
	sections := []string{
		strings.TrimSpace(codingAgentSystemPrompt),
		strings.TrimSpace(parts.TaskContext),
		strings.TrimSpace(parts.Envelope),
		strings.TrimSpace(parts.Mission),
		strings.TrimSpace(completionNative),
	}
	if parts.NonTerminalStep {
		sections = append(sections, strings.TrimSpace(blueprintStepNonterminal))
	}
	kept := sections[:0]
	for _, s := range sections {
		if s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "\n\n")
}
