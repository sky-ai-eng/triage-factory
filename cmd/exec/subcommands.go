package exec

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
)

// SubcommandRunner is a registered exec verb. args are everything after the
// subcommand name; host is the run-scoped agenthost client (LocalClient or
// IPCClient per AutoDetect). Returns a process exit code.
type SubcommandRunner func(ctx context.Context, args []string, host agenthost.Client) int

// subcommandRegistry is the process-global map of registered exec verbs.
// Written once during single-threaded startup (an ee package's init()),
// read thereafter from Handle. Same no-mutex startup-write contract as
// routing.RegisterSource (internal/routing/source_registry.go:42) — see
// there for the rationale.
var subcommandRegistry = map[string]SubcommandRunner{}

// reservedSubcommandNames are the names RegisterSubcommand refuses — the
// built-in switch cases in Handle, plus the help flags, none of which a
// registered verb may shadow.
var reservedSubcommandNames = map[string]bool{
	"gh": true, "jira": true, "workspace": true, "--help": true, "-h": true,
}

// RegisterSubcommand registers an exec verb under name. Called from an ee
// package's init(). Panics on empty/duplicate name or a name colliding with
// a built-in switch case ("gh", "jira", "workspace", "--help", "-h").
func RegisterSubcommand(name string, run SubcommandRunner) {
	if name == "" {
		panic("exec.RegisterSubcommand: name must not be empty")
	}
	if reservedSubcommandNames[name] {
		panic("exec.RegisterSubcommand: " + name + " is a built-in subcommand and cannot be re-registered")
	}
	if run == nil {
		panic("exec.RegisterSubcommand: run must not be nil")
	}
	if _, exists := subcommandRegistry[name]; exists {
		panic("exec.RegisterSubcommand: " + name + " is already registered")
	}
	subcommandRegistry[name] = run
}

// ResetSubcommands clears the registry (tests only).
func ResetSubcommands() {
	subcommandRegistry = map[string]SubcommandRunner{}
}
