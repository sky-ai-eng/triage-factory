package exec

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
)

func noopSubcommandRunner(context.Context, []string, agenthost.Client) int { return 0 }

// noopSubcommand is the minimal valid registration for tests that exercise
// the registry's name handling rather than the entry's contents.
var noopSubcommand = Subcommand{Run: noopSubcommandRunner, HelpText: "Fake Commands:\n  fake do"}

func TestRegisterSubcommand_PanicsOnEmptyName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for an empty name")
		}
	}()
	RegisterSubcommand("", noopSubcommand)
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
			RegisterSubcommand(name, noopSubcommand)
		}()
	}
}

func TestRegisterSubcommand_PanicsOnNilRunner(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for a nil runner")
		}
	}()
	RegisterSubcommand("fake-nil-runner", Subcommand{HelpText: "x"})
}

// TestRegisterSubcommand_PanicsOnEmptyHelpText pins that an undocumented
// family fails at boot: a registered verb with no help section is a surface
// an agent can only learn by trial-and-error against live commands.
func TestRegisterSubcommand_PanicsOnEmptyHelpText(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for empty help text")
		}
	}()
	RegisterSubcommand("fake-no-help", Subcommand{Run: noopSubcommandRunner})
}

// TestRegisterSubcommand_PanicsOnDuplicate pins that a second registration
// for the same name fails at boot instead of silently swapping runners.
func TestRegisterSubcommand_PanicsOnDuplicate(t *testing.T) {
	t.Cleanup(ResetSubcommands)
	RegisterSubcommand("fake-dup", noopSubcommand)
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate registration")
		}
	}()
	RegisterSubcommand("fake-dup", noopSubcommand)
}

// TestRunRegistered_ReachesRunnerWithArgsAndHost pins the wiring seam
// dispatch's default case routes through: a registered name's runner receives
// exactly the args slice after the subcommand name and the host built for it,
// its returned exit code is what the caller sees, and the host is closed
// before runRegistered returns (the caller os.Exits after, so a deferred
// close inside dispatch would never run).
func TestRunRegistered_ReachesRunnerWithArgsAndHost(t *testing.T) {
	var gotArgs []string
	var gotHost agenthost.Client
	sub := Subcommand{
		HelpText: "Fake Commands:\n  fake do",
		Run: func(_ context.Context, args []string, host agenthost.Client) int {
			gotArgs = args
			gotHost = host
			return 7
		},
	}

	host := &closeCountingClient{}
	code := runRegistered(sub, []string{"a", "b"}, func() agenthost.Client { return host })

	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "a" || gotArgs[1] != "b" {
		t.Errorf("args = %v, want [a b]", gotArgs)
	}
	if gotHost != agenthost.Client(host) {
		t.Errorf("host = %v, want the built client", gotHost)
	}
	if host.closed != 1 {
		t.Errorf("host closed %d times, want exactly once before return", host.closed)
	}
}

// TestRunRegistered_HelpRoutesSkipTheAgentHost pins the property the built-in
// cases have always had, extended to registered families: a help request at
// ANY depth is served with a nil host and never builds the agenthost — which
// is what makes `exec slack --help` answer with no run identity in scope.
// ValueFlags guards the scan: a value-taking flag's literal "--help" payload
// is not a help request.
func TestRunRegistered_HelpRoutesSkipTheAgentHost(t *testing.T) {
	sub := Subcommand{
		HelpText:   "Fake Commands:\n  fake do",
		ValueFlags: map[string]bool{"--body": true},
		Run: func(_ context.Context, _ []string, host agenthost.Client) int {
			if host != nil {
				return 1
			}
			return 0
		},
	}
	mustNotBuild := func() agenthost.Client {
		t.Fatal("buildAgentHost invoked on a help route")
		return nil
	}

	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{"verb", "--help"},
		{"verb", "sub", "--help"},
		nil, // no args at all is a help request, matching the built-ins
	} {
		if code := runRegistered(sub, args, mustNotBuild); code != 0 {
			t.Errorf("args %v: runner saw a non-nil host on a help route", args)
		}
	}

	// The flag-value case is NOT help: the runner must get a real host.
	host := &closeCountingClient{}
	ran := false
	sub.Run = func(_ context.Context, _ []string, h agenthost.Client) int {
		ran = h == agenthost.Client(host)
		return 0
	}
	runRegistered(sub, []string{"verb", "--body", "--help"}, func() agenthost.Client { return host })
	if !ran {
		t.Error(`--body "--help" was misread as a help request`)
	}
}

// closeCountingClient is a Client stub that counts Close calls; every other
// method is inherited from the embedded nil interface and must not be reached.
type closeCountingClient struct {
	agenthost.Client
	closed int
}

func (c *closeCountingClient) Close() error {
	c.closed++
	return nil
}

// TestSubcommandRegistry_UnknownNamePreservesCurrentBehavior pins that an
// unregistered name misses the registry lookup — Handle's default case falls
// through to the existing "unknown exec command" error unchanged.
func TestSubcommandRegistry_UnknownNamePreservesCurrentBehavior(t *testing.T) {
	if _, ok := subcommandRegistry["totally-unregistered-name"]; ok {
		t.Error("expected no match for an unregistered name")
	}
}
