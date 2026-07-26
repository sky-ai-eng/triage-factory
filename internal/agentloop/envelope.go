package agentloop

import (
	_ "embed" // powers the vendored prompt text below
	"strings"
)

// The four vendored prompt files below are the native runtime's own. They
// deliberately do not share text with the SDK path's files in
// internal/ai/prompts: those describe a different harness — a tool allowlist,
// approval prompts, a different set of tool names — and the two drifting into
// one file is how the native prompt came to assert a permission model that
// does not exist here. Edit them independently.

// machinistSystemPrompt is the base system prompt: who the agent is, what the
// harness actually does, how to work, and who reads the result. It is the
// first thing in the prefix every provider caches.
//
//go:embed prompts/machinist-system.txt
var machinistSystemPrompt string

// nativeEnvelope is the standing contract every delegated run works under —
// the GitHub surface, the TF verbs, workspace materialization, guardrails,
// scratch discipline, entity memory.
//
// It carries no placeholders, which is the property that matters: everything
// here is identical for every Machinist run in the fleet, so it sits inside
// the cached prefix rather than being re-sent per run. The two values that do
// vary — the team's branch convention and this run's memory path — are
// supplied by EnvelopeParts.RunContext, and this text refers to them rather
// than embedding them.
//
//go:embed prompts/envelope.txt
var nativeEnvelope string

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
//go:embed prompts/blueprint-step-nonterminal.txt
var blueprintStepNonterminal string

// EnvelopeParts are the caller-supplied sections of a native system prompt.
// Assembly is mechanical — this package concatenates in a fixed order and
// does no interpolation, templating, or rewriting. Prompt composition and
// quality are owned by the caller (and by the humans who edit the vendored
// files).
type EnvelopeParts struct {
	// RunContext is trusted, system-authored fact about this particular run:
	// the team's branch convention, the path this run writes its memory to.
	// It is the only per-run text the vendored envelope depends on, which is
	// what lets that envelope stay byte-identical across the fleet.
	RunContext string
	// TaskContext is the system-rendered task/entity block. It carries
	// externally-authored text (PR titles, issue bodies), so the caller is
	// responsible for having marked it as untrusted before it gets here.
	TaskContext string
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
//	base coding-agent prompt   ─┐
//	standing envelope           ├─ identical for every run in the fleet
//	[completion contract]      ─┘
//	[non-terminal step addendum]
//	run context
//	task context
//	mission
//
// The split is the point. Everything above the addendum is the same bytes
// for every Machinist run anywhere, so it is one cacheable prefix rather
// than per-run tokens; everything below it varies. The addendum sits on the
// boundary rather than inside the shared block because gating it there would
// fork that block into terminal and non-terminal variants for a few hundred
// bytes.
//
// Within the varying tail, the mission comes last so the authoritative
// instruction is the most recent thing the model reads, after the
// externally-authored task context rather than before it. That ordering
// forgoes a per-prompt cache tier — two runs of the same prompt could
// otherwise have shared a little further — and the trade is deliberate.
func BuildSystemPrompt(parts EnvelopeParts) string {
	sections := []string{
		strings.TrimSpace(machinistSystemPrompt),
		strings.TrimSpace(nativeEnvelope),
	}
	if parts.HasBlueprint {
		sections = append(sections, strings.TrimSpace(completionBlueprint))
	}
	if parts.NonTerminalStep {
		sections = append(sections, strings.TrimSpace(blueprintStepNonterminal))
	}
	sections = append(sections,
		strings.TrimSpace(parts.RunContext),
		strings.TrimSpace(parts.TaskContext),
		strings.TrimSpace(parts.Mission),
	)

	kept := sections[:0]
	for _, s := range sections {
		if s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "\n\n")
}
