//go:build linux

package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// captureCommand builds the child command that runs the git-delta capture. A
// package var so tests can point it at a helper process instead of re-execing
// the real binary. Production re-invokes this same binary's internal
// `snapshot-capture` subcommand.
var captureCommand = func(ctx context.Context, wtPath string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate self: %w", err)
	}
	return exec.CommandContext(ctx, self, "snapshot-capture", wtPath), nil
}

// captureIsolated runs CaptureWorkspaceGit for wtPath inside a
// dropped-privilege, network-isolated child and returns the decoded delta.
//
// The child drops to the sandbox uid/gid (matching the run-root chown) and runs
// in a fresh, empty network namespace. So: any clean/smudge/textconv/diff
// filter a hostile .git/config could trigger executes only as the agent's own
// uid with no network — not as the host root — which is the whole point; and
// the reader's uid matching the tree owner means git's own capture commands
// (rev-parse/bundle/diff) no longer fail dubious-ownership. The child receives a
// deliberately minimal environment: never the TF process env (which carries DB
// and service credentials), plus config overrides that neuter the two
// attribute-free exec vectors (core.fsmonitor, diff.external) as defense in
// depth on top of the uid drop.
func captureIsolated(ctx context.Context, wtPath string) (*worktree.GitDelta, error) {
	cmd, err := captureCommand(ctx, wtPath)
	if err != nil {
		return nil, err
	}
	cmd.Dir = "/" // a neutral cwd; the child is passed the worktree path explicitly
	cmd.Env = captureChildEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(sandbox.WorktreeUID),
			Gid: uint32(sandbox.WorktreeGID),
			// Empty Groups (NoSetGroups stays false) forces setgroups(0), shedding
			// the parent root process's supplementary groups — otherwise the child
			// keeps group 0 (root) et al. and retains group-readable/writable host
			// access it must not have.
			Groups: []uint32{},
		},
		Cloneflags: syscall.CLONE_NEWNET, // empty network namespace: capture is local-only
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("isolated capture: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 || string(out) == "null" {
		return nil, nil // non-git run root: no delta
	}
	var delta worktree.GitDelta
	if err := json.Unmarshal(out, &delta); err != nil {
		return nil, fmt.Errorf("isolated capture: decode delta: %w", err)
	}
	return &delta, nil
}

// captureChildEnv is the minimal environment for the capture child. It
// deliberately does NOT inherit the TF process env — that holds DB passwords and
// service tokens the child (running attacker-influenceable git) must never see.
// It carries only what git needs to run locally, plus config overrides
// disabling the attribute-free code-exec config keys (core.fsmonitor,
// diff.external); the uid drop, not these overrides, is the actual boundary.
func captureChildEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"), // locate the git binary
		"HOME=/tmp",                 // git's global-config lookup; absent file is fine
		"GIT_TERMINAL_PROMPT=0",
		// Neutralize the two exec vectors that need no .gitattributes selection.
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=diff.external", "GIT_CONFIG_VALUE_1=",
	}
	return env
}
