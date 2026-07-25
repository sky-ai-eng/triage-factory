package agentloop

import (
	_ "embed" // powers the vendored prompt text below
	"strings"
)

// codingAgentSystemPrompt is the base coding-agent system prompt — the
// stable, task-independent half of every native system prompt, and the first
// thing in the prefix every provider caches. It is a file rather than text
// composed in Go because it is iterated by hand alongside the seven tool
// definitions it was written against: the descriptions in
// tools/definitions.json and the wording here are one artifact, and editing
// either alone changes model behavior for the worse.
//
//go:embed prompts/coding-agent-system.txt
var codingAgentSystemPrompt string

// completionBlueprint is the terminal contract for a conversation executing
// a blueprint: stopping is concluding, plus the flow-control tool and the
// artifact contract. It replaces the SDK path's JSON completion envelope,
// which this runtime never emits and never parses.
//
// There is deliberately no taskless counterpart. A completion contract
// answers "how does this run end", and that is only a question when a run
// was dispatched to do something for someone who is not here. A conversation
// with a person in it ends when they stop writing, which is not a protocol
// and does not need stating.
//
//go:embed prompts/completion-blueprint.txt
var completionBlueprint string

// blueprintStepNonterminal is the addendum appended on a non-terminal
// blueprint step. It changes no tool: within a blueprint the tool set is the
// same everywhere. What it changes is what an ordinary stop means, since on
// a non-final step that is a handoff rather than the end of the task. Step
// position is carried here and nowhere else, which is why a prompt and a
// tool list cannot fall out of step.
//
// It shares its handoff and external-action guidance with the SDK path's
// file of the same name, but it is NOT a rewrite of it and the two must be
// edited independently: there, a step declares `continue` in a JSON envelope
// and stopping any other way ends the blueprint; here stopping IS the
// handoff. Porting wording across mechanically would invert the safe
// default on one side or the other.
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
	// HasBlueprint appends the completion contract. It must agree with
	// Spec.HasBlueprint, which registers the tool that contract describes: a
	// model must never be handed a tool its instructions don't mention, nor
	// told about one it doesn't have.
	HasBlueprint bool
	// NonTerminalStep appends the blueprint addendum, which tells the model
	// that stopping hands off rather than ending the task. This is the only
	// place step position is expressed. It implies HasBlueprint — a step is
	// a step of something.
	NonTerminalStep bool
}

// BuildSystemPrompt assembles the native system prompt in the one fixed
// order every native call uses:
//
//	base coding-agent prompt
//	task context
//	TF envelope
//	mission
//	[completion contract]
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
	}
	if parts.HasBlueprint {
		sections = append(sections, strings.TrimSpace(completionBlueprint))
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
