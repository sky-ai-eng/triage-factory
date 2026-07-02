package exec

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
)

func noopSubcommandRunner(context.Context, []string, agenthost.Client) int { return 0 }

func TestRegisterSubcommand_PanicsOnEmptyName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for an empty name")
		}
	}()
	RegisterSubcommand("", noopSubcommandRunner)
}

// TestRegisterSubcommand_PanicsOnReservedName pins the shadow guard: none of
// the built-in switch cases or help flags in Handle may be registered over.
func TestRegisterSubcommand_PanicsOnReservedName(t *testing.T) {
	for _, name := range []string{"gh", "jira", "workspace", "--help", "-h"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("expected panic for reserved name %q", name)
				}
			}()
			RegisterSubcommand(name, noopSubcommandRunner)
		}()
	}
}

func TestRegisterSubcommand_PanicsOnNilRunner(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for a nil runner")
		}
	}()
	RegisterSubcommand("fake-nil-runner", nil)
}

// TestRegisterSubcommand_PanicsOnDuplicate pins that a second registration
// for the same name fails at boot instead of silently swapping runners.
func TestRegisterSubcommand_PanicsOnDuplicate(t *testing.T) {
	t.Cleanup(ResetSubcommands)
	RegisterSubcommand("fake-dup", noopSubcommandRunner)
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate registration")
		}
	}()
	RegisterSubcommand("fake-dup", noopSubcommandRunner)
}

// TestSubcommandRegistry_ReachesRunnerWithArgsAndHost pins the wiring seam
// Handle's default case reads from: a registered name's runner receives
// exactly the args slice after the subcommand name (cmdArgs in Handle) and
// the host passed at the call site, and its returned exit code is what the
// caller sees. Handle itself isn't exercised here (it os.Exits) — per the
// ticket's own guidance, this tests the lookup + wiring seam directly.
func TestSubcommandRegistry_ReachesRunnerWithArgsAndHost(t *testing.T) {
	t.Cleanup(ResetSubcommands)

	var gotArgs []string
	var gotHost agenthost.Client
	RegisterSubcommand("fake", func(_ context.Context, args []string, host agenthost.Client) int {
		gotArgs = args
		gotHost = host
		return 7
	})

	run, ok := subcommandRegistry["fake"]
	if !ok {
		t.Fatal("expected \"fake\" to be registered")
	}

	var wantHost agenthost.Client // nil sentinel this test threads through
	code := run(context.Background(), []string{"a", "b"}, wantHost)

	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "a" || gotArgs[1] != "b" {
		t.Errorf("args = %v, want [a b]", gotArgs)
	}
	if gotHost != wantHost {
		t.Errorf("host = %v, want %v", gotHost, wantHost)
	}
}

// TestSubcommandRegistry_UnknownNamePreservesCurrentBehavior pins that an
// unregistered name misses the registry lookup — Handle's default case falls
// through to the existing "unknown exec command" error unchanged.
func TestSubcommandRegistry_UnknownNamePreservesCurrentBehavior(t *testing.T) {
	if _, ok := subcommandRegistry["totally-unregistered-name"]; ok {
		t.Error("expected no match for an unregistered name")
	}
}
