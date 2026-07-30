package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
)

// TestResolveBootIdentity pins the matrix. These three cells are the whole
// contract: what the process is must be a function of argv and the marker
// alone, decided before anything reads TF_MODE, stats a socket, or opens a
// file.
func TestResolveBootIdentity(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		sandboxed bool
		want      bootIdentity
	}{
		{"no args → server", nil, false, bootServer},
		{"empty args → server", []string{}, false, bootServer},
		{"args, no marker → host-cli", []string{"exec", "gh", "pr", "view"}, false, bootHostCLI},
		{"args + marker → jail-cli", []string{"exec", "gh", "pr", "view"}, true, bootJailCLI},

		// The applet's implicit `exec` is applied before resolution, so both
		// spellings of a jailed verb land on the same identity — argv0 answers
		// "what do you want", never "where am I".
		{"applet-resolved args + marker → jail-cli", []string{"exec", "workspace", "list"}, true, bootJailCLI},

		// An operator subcommand and a server flag are still CLI args: they
		// resolve to a CLI identity and get refused by the jailed dispatcher,
		// rather than quietly booting a second server or a migration inside a
		// run sandbox.
		{"operator subcommand + marker → jail-cli", []string{"migrate", "up"}, true, bootJailCLI},
		{"server flag + marker → jail-cli", []string{"--port", "8080"}, true, bootJailCLI},
		{"server flag, no marker → host-cli", []string{"--port", "8080"}, false, bootHostCLI},

		// A stray marker must never refuse a server boot: with no argv there is
		// nothing to serve as a CLI, and nothing execs the binary bare inside a
		// jail (the agent's tool allowlist admits only `<binary> exec ...`).
		{"no args + marker → server", nil, true, bootServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveBootIdentity(tt.args, tt.sandboxed); got != tt.want {
				t.Errorf("resolveBootIdentity(%q, %v) = %s, want %s", tt.args, tt.sandboxed, got, tt.want)
			}
		})
	}
}

// TestInRunSandbox_ExactMatch pins the marker read: only the assembler's exact
// value counts. A truthy-looking value from somewhere else is not the assembler
// saying "you are in a jail", and reading it as one would send a host CLI down
// the jailed path for anyone with a stray export.
func TestInRunSandbox_ExactMatch(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{agentproc.SandboxMarkerEnvValue, true},
		{"", false},
		{"0", false},
		{"true", false},
		{"1 ", false},
	} {
		t.Setenv(agentproc.SandboxMarkerEnvVar, tc.value)
		if got := inRunSandbox(); got != tc.want {
			t.Errorf("%s=%q: inRunSandbox() = %v, want %v", agentproc.SandboxMarkerEnvVar, tc.value, got, tc.want)
		}
	}
}

// TestDispatchJailCLI_RefusesHostSubcommands pins the refusal set. Each of
// these either needs local state the jail hasn't got or would create some by
// reaching for it, which is exactly the failure this identity exists to
// prevent — so each must be turned away by name, not attempted.
func TestDispatchJailCLI_RefusesHostSubcommands(t *testing.T) {
	for _, args := range [][]string{
		{"install"},
		{"uninstall", "--yes"},
		{"migrate", "up"},
		{"jwk-init"},
		{"instance", "list"},
		{"operator", "list"},
		{"hook", "record-push"},
		{"snapshot-capture", "/work"},
		{"cap-broker"},
		{"run-sidecar"},
		{"--port", "8080"}, // server mode
		{"bogus"},
	} {
		t.Run(args[0], func(t *testing.T) {
			err := dispatchJailCLI(args)
			if err == nil {
				t.Fatalf("dispatchJailCLI(%q) = nil; want a refusal", args)
			}
			if !strings.Contains(err.Error(), "not available inside a run sandbox") {
				t.Errorf("dispatchJailCLI(%q) error = %q; want the sandbox refusal", args, err)
			}
		})
	}
}

// TestDispatchJailCLI_HelpAndVersion pins the two things that still work with
// no daemon behind them: they are pure text, and a jail whose socket is missing
// is exactly when a reader most wants to see them.
func TestDispatchJailCLI_HelpAndVersion(t *testing.T) {
	for _, args := range [][]string{
		{"--help"}, {"-h"}, {"help"},
		{"--version"}, {"-v"}, {"version"},
	} {
		t.Run(args[0], func(t *testing.T) {
			if err := withStdoutDiscarded(t, func() error { return dispatchJailCLI(args) }); err != nil {
				t.Errorf("dispatchJailCLI(%q) = %v; want nil", args, err)
			}
		})
	}
}

// TestRunJailCLI_NoBootSpine is the structural assertion, made through the real
// entrypoint: a jailed verb invocation resolves to jail-cli and fails closed on
// the absent socket WITHOUT having run any of the host boot spine.
//
// The two probes are set so that running that spine would be loud. TF_MODE is
// invalid, so runmode.InitFromEnv would return its own error; a deployment
// secret points at a nonexistent file, so secretenv.Resolve would fail fast on
// it. Getting the socket error instead proves neither ran. The temp HOME then
// proves the rest: had the DB been opened, its state directory would be sitting
// under it — which under a jail's HOME is the agent's own worktree.
func TestRunJailCLI_NoBootSpine(t *testing.T) {
	if _, err := os.Stat(agenthost.DefaultSocketPath); err == nil {
		t.Skipf("%s exists on this host; the missing-socket path cannot be exercised here", agenthost.DefaultSocketPath)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(agentproc.SandboxMarkerEnvVar, agentproc.SandboxMarkerEnvValue)
	t.Setenv("TF_MODE", "not-a-mode")
	t.Setenv("TF_SECRET_ENCRYPTION_KEY_FILE", filepath.Join(home, "absent-secret"))

	err := run(context.Background(), []string{"/usr/local/bin/triagefactory", "exec", "gh", "pr", "view", "1"})
	if !errors.Is(err, agenthost.ErrSandboxSocketMissing) {
		t.Fatalf("run() = %v; want ErrSandboxSocketMissing (a boot-spine error here means the jailed path ran it)", err)
	}
	assertNoLocalState(t, home)
}

// TestRunJailCLI_NoLocalStateForRefusedSubcommand covers the other half of the
// same property: a refused subcommand must also leave nothing behind. The
// dogfood failure was a state directory in the worktree, and a refusal path
// that opened a DB before refusing would reproduce it just as well as a verb
// that did.
func TestRunJailCLI_NoLocalStateForRefusedSubcommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(agentproc.SandboxMarkerEnvVar, agentproc.SandboxMarkerEnvValue)
	t.Setenv("TF_MODE", "not-a-mode")

	err := run(context.Background(), []string{"/usr/local/bin/triagefactory", "migrate", "up"})
	if err == nil || !strings.Contains(err.Error(), "not available inside a run sandbox") {
		t.Fatalf("run() = %v; want the sandbox refusal", err)
	}
	assertNoLocalState(t, home)
}

// assertNoLocalState fails if anything at all was written under home. The
// acceptance criterion names the state directory specifically, but a jailed
// process has no business creating any file on the host filesystem, so the
// check is the stronger one.
func assertNoLocalState(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(home, ".triagefactory")); err == nil {
		t.Error("the jailed path created a .triagefactory state directory; under a jail's HOME that lands in the run worktree")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("read home: %v", err)
	}
	for _, e := range entries {
		t.Errorf("the jailed path wrote %q into HOME; it must write nothing", e.Name())
	}
}

// withStdoutDiscarded runs fn with stdout pointed at /dev/null, so a help
// dump doesn't drown the test output.
func withStdoutDiscarded(t *testing.T, fn func() error) error {
	t.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	saved := os.Stdout
	os.Stdout = devNull
	defer func() {
		os.Stdout = saved
		_ = devNull.Close()
	}()
	return fn()
}
