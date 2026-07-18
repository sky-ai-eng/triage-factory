// Package snapshotcapture implements the internal
// `triagefactory snapshot-capture <worktree-path> [session-id]` subcommand: it
// captures one parked run's non-recoverable, agent-owned state — the git delta
// AND (when a session id is given) the Claude session transcript — and writes a
// worktree.CapturedState to stdout as JSON. Undocumented in --help, like `hook`.
//
// It exists so the delegate spawner can run the capture in a
// dropped-privilege, network-isolated child instead of host-side as root: two
// reasons converge on the same child. captureUncommitted's `git add -A` /
// `git diff` honor clean/smudge/textconv/fsmonitor drivers named by the run
// root's (agent-writable, once chowned to the sandbox uid) .git/config, so
// running them as root is a sandbox escape; and the SDK's session transcript
// lives at 0600 under a 0700 projects dir it locks to its owner, which the
// orchestrator (a different uid) cannot read at all. Re-invoked as the sandbox
// uid, a fired filter is contained to the agent's own privilege AND the
// transcript is readable. The parent (internal/delegate) owns the uid drop +
// netns; this package is the unprivileged body that just captures and emits.
package snapshotcapture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// Handle dispatches `triagefactory snapshot-capture <worktree-path> [session-id]`.
// The session id is optional: absent (or empty) captures only the git delta,
// matching a caller that has no session to preserve.
func Handle(args []string) {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: triagefactory snapshot-capture <worktree-path> [session-id]")
		os.Exit(2)
	}
	sessionID := ""
	if len(args) == 2 {
		sessionID = args[1]
	}
	if err := Run(context.Background(), args[0], sessionID, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "snapshot-capture:", err)
		os.Exit(1)
	}
}

// Run captures wtPath's git delta and, when sessionID is non-empty, its Claude
// session transcript, writing the combined worktree.CapturedState as JSON to w.
// A missing transcript is not an error — it rides as an empty field, and the
// resume-side guard reports the unresumable run cleanly. The testable core of
// Handle, separated from argv parsing + os.Exit so the child's actual
// capture-then-encode body can be exercised without spawning a process.
func Run(ctx context.Context, wtPath, sessionID string, w io.Writer) error {
	delta, err := worktree.CaptureWorkspaceGit(ctx, wtPath)
	if err != nil {
		return err
	}
	state := worktree.CapturedState{Delta: delta}
	// The child is spawned only for a multi-mode sandboxed run (see
	// delegate.captureWorkspaceGit), and carries no TF_MODE, so it reads the
	// transcript from the run-root sandbox layout directly rather than via the
	// runmode-aware path — and confined against a symlink escape.
	if transcript, ok := worktree.ReadSandboxSessionTranscript(wtPath, sessionID); ok {
		state.Transcript = transcript
	}
	if err := json.NewEncoder(w).Encode(state); err != nil {
		return fmt.Errorf("encode captured state: %w", err)
	}
	return nil
}
