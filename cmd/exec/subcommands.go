package exec

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
)

// SubcommandRunner is a registered exec verb. args are everything after the
// subcommand name; host is the run-scoped agenthost client (LocalClient or
// IPCClient, per the process's boot identity). Returns a process exit code.
type SubcommandRunner func(ctx context.Context, args []string, host agenthost.Client) int

// Subcommand is one registered exec verb family: the runner plus the
// documentation the dispatcher needs to serve `--help` for it exactly as it
// does for the built-ins. The docs ride the registration because the seam is
// the one place a feature author cannot forget them — a family registered
// without help text is a surface an agent can only learn by trial-and-error
// against live commands.
type Subcommand struct {
	// Run executes the verb family. host is nil on a help route: the
	// dispatcher short-circuits help BEFORE building the agenthost, exactly
	// as it does for the built-ins, so the runner must serve `--help` at
	// every depth without touching host.
	Run SubcommandRunner

	// HelpText is the family's section of the top-level exec help, under the
	// same conventions as the built-ins' HelpText constants: a literal block
	// naming its verbs bare ("slack send ...", never an invocation prefix —
	// the prefix appears in the usage line alone). Required.
	HelpText string

	// ValueFlags is every flag of the family that takes a value, for the
	// shared help scan (execflags.HasHelpFlag) — what stops `--body "--help"`
	// from reading as a help request. Booleans must be absent: listing one
	// would make `--someBool --help` swallow a real help request.
	ValueFlags map[string]bool

	// SourceKind names the event source whose availability governs whether
	// this family appears in a filtered top-level help index — the extension
	// namespace, e.g. "slack". Empty means sourceless: always listed, like
	// workspace/memory. Filtering is index-only: `<name> --help` always
	// answers (the verbs are compiled in), and execution stays gated by the
	// extension dispatch's entitlement + source-disabled checks.
	SourceKind string
}

// subcommandRegistry is the process-global map of registered exec verbs.
// Written once during single-threaded startup (an ee package's init()),
// read thereafter from Handle. Same no-mutex startup-write contract as
// routing.RegisterSource (internal/routing/source_registry.go:42) — see
// there for the rationale.
var subcommandRegistry = map[string]Subcommand{}

// reservedSubcommandNames are the names RegisterSubcommand refuses — the
// built-in switch cases in Handle, plus the help flags, none of which a
// registered verb may shadow.
var reservedSubcommandNames = map[string]bool{
	"gh": true, "jira": true, "workspace": true, "memory": true, "--help": true, "-h": true,
}

// RegisterSubcommand registers an exec verb family under name. Called from an
// ee package's init(). Panics on empty/duplicate name, a name colliding with
// a built-in switch case ("gh", "jira", "workspace", "memory", "--help",
// "-h"), a nil runner, or empty help text — an undocumented family must fail
// at boot, not surface as a verb `--help` cannot explain.
func RegisterSubcommand(name string, sub Subcommand) {
	if name == "" {
		panic("exec.RegisterSubcommand: name must not be empty")
	}
	if reservedSubcommandNames[name] {
		panic("exec.RegisterSubcommand: " + name + " is a built-in subcommand and cannot be re-registered")
	}
	if sub.Run == nil {
		panic("exec.RegisterSubcommand: " + name + " must carry a runner")
	}
	if sub.HelpText == "" {
		panic("exec.RegisterSubcommand: " + name + " must carry help text")
	}
	if _, exists := subcommandRegistry[name]; exists {
		panic("exec.RegisterSubcommand: " + name + " is already registered")
	}
	subcommandRegistry[name] = sub
}

// RegisteredSubcommands returns a snapshot copy of the registry keyed by
// subcommand name — read-only, for the composition-root parity guards that
// hold CLI registrations and tools references to one another.
func RegisteredSubcommands() map[string]Subcommand {
	out := make(map[string]Subcommand, len(subcommandRegistry))
	for name, sub := range subcommandRegistry {
		out[name] = sub
	}
	return out
}

// ResetSubcommands clears the registry (tests only).
func ResetSubcommands() {
	subcommandRegistry = map[string]Subcommand{}
}
