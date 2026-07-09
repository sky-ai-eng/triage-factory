// Package snapshotcapture implements the internal
// `triagefactory snapshot-capture <worktree-path>` subcommand: it runs the
// git-delta capture for one parked worktree and writes the resulting
// worktree.GitDelta to stdout as JSON (a nil delta — a non-git run root —
// encodes as "null"). Undocumented in --help, like `hook`.
//
// It exists so the delegate spawner can run the capture in a
// dropped-privilege, network-isolated child instead of host-side as root:
// captureUncommitted's `git add -A` / `git diff` honor clean/smudge/textconv/
// fsmonitor drivers named by the run root's (agent-writable, once chowned to
// the sandbox uid) .git/config, so running them as root is a sandbox escape.
// Re-invoked as the sandbox uid, a fired filter is contained to the agent's
// own privilege. The parent (internal/delegate) owns the uid drop + netns;
// this package is the unprivileged body that just captures and emits.
package snapshotcapture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// Handle dispatches `triagefactory snapshot-capture <worktree-path>`.
func Handle(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: triagefactory snapshot-capture <worktree-path>")
		os.Exit(2)
	}
	if err := Run(context.Background(), args[0], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "snapshot-capture:", err)
		os.Exit(1)
	}
}

// Run captures the git delta for wtPath and writes it as JSON to w (a nil
// delta encodes as "null"). The testable core of Handle, separated from the
// argv parsing + os.Exit so the child's actual capture-then-encode body can
// be exercised without spawning a process.
func Run(ctx context.Context, wtPath string, w io.Writer) error {
	delta, err := worktree.CaptureWorkspaceGit(ctx, wtPath)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(delta); err != nil {
		return fmt.Errorf("encode delta: %w", err)
	}
	return nil
}
