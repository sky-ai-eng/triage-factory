package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// handleSnapshotCapture is the internal `snapshot-capture <worktree-path>`
// subcommand: it runs the git-delta capture for one parked worktree and writes
// the resulting worktree.GitDelta to stdout as JSON (a nil delta — a non-git
// run root — encodes as "null"). Undocumented in --help, like `hook`.
//
// It exists so the delegate spawner can run the capture in a dropped-privilege,
// network-isolated child instead of host-side as root: captureUncommitted's
// `git add -A` / `git diff` honor clean/smudge/textconv/fsmonitor drivers named
// by the run root's (agent-writable, once chowned to the sandbox uid)
// .git/config, so running them as root is a sandbox escape. Re-invoked as the
// sandbox uid, a fired filter is contained to the agent's own privilege. The
// parent (internal/delegate) owns the uid drop + netns; this handler is the
// unprivileged body that just captures and emits.
func handleSnapshotCapture(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: triagefactory snapshot-capture <worktree-path>")
		os.Exit(2)
	}
	delta, err := worktree.CaptureWorkspaceGit(context.Background(), args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "snapshot-capture:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(delta); err != nil {
		fmt.Fprintln(os.Stderr, "snapshot-capture: encode delta:", err)
		os.Exit(1)
	}
}
