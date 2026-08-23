// Package agentprompt owns every piece of agent-facing prompt text in the
// product — who the agent is, what harness it is running in, and what it may
// do — and the one composition layer that assembles them.
//
// Three kinds of prompt text exist in this repo and only one of them lives
// here:
//
//   - Agent framework prompts (this package). Composed per Spec, static for a
//     fixed Spec, shared by every surface that spawns an agent.
//   - Seed content (internal/promptseed). Seeded into the `prompts` table as
//     user-editable rows; the .txt in the repo is a seed, not a runtime
//     artifact.
//   - System job prompts. Toolless LLM calls TF makes for itself; they live
//     next to their consumer (internal/ai for scoring, internal/repoprofile
//     for profiling).
//
// The composition rules that keep the four axes below from becoming a
// combinatorial directory tree:
//
//  1. One package owns agent-facing prompt text. Nothing outside this package
//     embeds or exports agent prompt strings.
//  2. Each block file varies on at most one axis. If a block would need two,
//     split it. A GPT set later is `identity/machinist.gpt.txt` plus a family
//     arm in the manifest, not a new tree.
//  3. Build is byte-identical for a fixed Spec, across runs and across
//     processes — the cacheable-prefix property. Per-run text never arrives
//     here: the mission, and the task context it is about, ride the
//     conversation's opening turn instead. Nothing in this package takes an
//     argument that could carry them.
//  4. Mode is a parameter, not a package-level read. Build never calls
//     runmode.Current(); the caller passes it. That keeps the package pure and
//     both variants testable without env manipulation.
//  5. The manifest is Go, not a config format. Explicit selection code beats a
//     DSL nobody else will learn.
//
// Rule 3 is what makes the mode axis free: mode is fixed per deployment, so
// two static variants selected by a process-level constant are still one
// static string per process, and every run in a deployment shares the prefix.
package agentprompt

// Surface names which agent the prompt is for — the "who am I" axis. A surface
// is a product concept (a delegated worker), not a runtime detail.
type Surface string

const (
	// SurfaceMachinist is the delegated agent that executes a task's mission
	// in an isolated worktree and terminates with a completion envelope.
	SurfaceMachinist Surface = "machinist"
)

// Runtime names the loop driving the agent — the "what harness am I in" axis.
type Runtime string

const (
	// RuntimeSDK is the Claude Code SDK subprocess: it owns the conversation
	// loop, tool dispatch, and permission prompts, and TF hands it a prompt
	// plus an --append-system-prompt.
	RuntimeSDK Runtime = "sdk"

	// RuntimeNative is TF's own loop, driving the conversation from rows. Its
	// blocks arrive with the native loop engine; today the arm resolves to the
	// runtime-invariant blocks only (see manifest.go) so adding it is adding
	// files, not adding an axis.
	RuntimeNative Runtime = "native"
)

// Family names the model family the text is tuned for — the "who am I talking
// to" axis. Claude is the only populated arm; the axis exists so a later GPT
// or Gemini set is a `.gpt.txt` sibling plus a manifest arm rather than a
// parallel tree.
type Family string

const (
	// FamilyClaude is the Anthropic model family.
	FamilyClaude Family = "claude"
)

// Mode names the deployment posture — the "what is actually enforced around
// me" axis. It is the axis that stops the prompts from asserting things that
// are false. Both modes mediate the managed Git path, but only multi puts the
// agent in a sandbox with a fail-closed egress allowlist. Local's process-scoped
// routing is a safety and identity guarantee for ordinary behavior, not a
// security boundary against an unsandboxed process trying to bypass it.
type Mode string

const (
	// ModeLocal is the single-binary, single-user posture: the agent runs
	// unsandboxed on the operator's own machine while managed Git traffic uses
	// TF's configured identity through a loopback proxy.
	ModeLocal Mode = "local"

	// ModeMulti is the split control/executor posture: the agent runs in a
	// gVisor jail behind a fail-closed egress allowlist, and its git traffic
	// transits a per-run proxy that holds the credential and gates pushes.
	ModeMulti Mode = "multi"
)

// ModeFor maps the deployment-mode string internal/runmode resolves at startup
// onto this package's mode axis. It exists so callers convert explicitly at
// the call site (rule 4) instead of this package reaching for process state;
// an unrecognized value resolves to ModeLocal, matching runmode's own default.
func ModeFor(mode string) Mode {
	if Mode(mode) == ModeMulti {
		return ModeMulti
	}
	return ModeLocal
}

// Spec is the full selector for one composed prompt: the four axes, resolved.
// Build is a pure function of it (rule 3), so a Spec is also the cache key for
// the prompt prefix a deployment reuses across every run.
type Spec struct {
	Surface Surface
	Runtime Runtime
	Family  Family
	Mode    Mode
}
