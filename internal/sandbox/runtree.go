package sandbox

import (
	"context"
	"os"
	"path/filepath"
)

// runTreeBasename is the directory ephemeral per-run worktrees live under
// inside os.TempDir(): os.TempDir()/triagefactory-runs/<runID>. This is the
// GitHub-PR / Jira / Slack task-run shape of Config.Worktree — org-blind by
// construction, since these trees don't outlive their own run.
// internal/worktree is the historical owner of this path (its
// makeWorktreeDir / MakeRunRoot materialize exactly here); it duplicates
// this literal in its own private runsDir constant rather than importing
// RunTreeRoot (worktree already imports this package for ChownRunTree /
// RemoveRunTree, and having the broker-side validator define the trusted
// shape independently — rather than depend on the producer's constant —
// mirrors how TrustedSDKDir/TrustedGitHooksDir resolve elsewhere in this
// package). A worktree package test cross-checks the two literals stay
// equal.
const runTreeBasename = "triagefactory-runs"

// RunTreeRoot returns the ephemeral per-run worktree root for runID. This
// is the ONLY legitimate shape for a delegated task run's Config.Worktree;
// launchspec_linux.go's mount-source validation rejects any Worktree that
// is neither this exact path nor under the org-scoped state-root tree
// (Curator sessions' shape — see worktreeScope).
func RunTreeRoot(runID string) string {
	return filepath.Join(os.TempDir(), runTreeBasename, runID)
}

// This file is the cross-platform surface for the run-tree ownership
// lifecycle: handing a run tree to the sandbox identity at run start,
// and destroying it at teardown. Both are privileged operations once
// the orchestrator's capabilities are dropped at exec (CAP_CHOWN for
// the ownership hand-off; unlinking through sandbox-owned directory
// modes for the removal), so on Linux they route through the package's
// PrivilegedOps seam — the cap-broker IPC client, exactly like
// Wrap/Close/ReapOrphans. Off Linux they degrade to the unprivileged
// equivalents the previous in-caller code used (no-op chown, plain
// os.RemoveAll), keeping local-mode/dev behavior byte-identical.

// ChownRunTree hands ownership of a run tree to the sandbox identity
// (WorktreeUID/GID) so the jailed agent can write its own worktree.
// subpath == "" chowns the whole root recursively (run start); a
// non-empty subpath is the mid-run `workspace add` case — intermediate
// directories shallowly, the subpath tree recursively. No-op off Linux
// (the sandbox path isn't reachable there).
func ChownRunTree(ctx context.Context, root, subpath string) error {
	return chownRunTree(ctx, root, subpath)
}

// RemoveRunTree removes a run tree (run root, scratch cwd, parked
// worktree) and everything under it; a missing path is a no-op
// success. Callers that used os.RemoveAll directly on run trees route
// through this instead so the removal works when the tree is owned by
// the sandbox uid and the calling process no longer has the privilege
// to unlink through it.
func RemoveRunTree(ctx context.Context, path string) error {
	return removeRunTree(ctx, path)
}

// RunTreeHandedOff reports whether path has already been handed to the sandbox
// identity — i.e. a launch chowned it and the calling (orchestrator) process no
// longer owns it. That hand-off is one-way for the life of the run tree, so this
// is the predicate for "a write in here would fail": after the chown only the
// run root's own scaffold directories are group-writable by the orchestrator, and
// everything inside belongs to the sandbox uid with the modes it was created
// with. A capability-less orchestrator therefore cannot create, truncate, or
// unlink anything below the top level.
//
// Callers use it to SKIP work that would otherwise fail with EACCES, not to
// decide whether to force it: the standing rule is that nothing TF-side writes
// into a run tree after its first launch. Conservative on error and off Linux —
// an unreadable or non-existent path reports false, since a caller that then
// proceeds gets the pre-existing behavior rather than a silent skip.
func RunTreeHandedOff(path string) bool {
	return runTreeHandedOff(path)
}

// CaptureRunDelta runs the parked-run capture in a child dropped to the
// sandbox uid inside an empty network namespace and returns its raw JSON
// stdout (a worktree.CapturedState — the git delta plus, when sessionID is
// set, the session transcript — decoded by the caller). Linux-only by
// construction; the non-Linux stub errors, and the delegate caller never
// routes here off Linux.
func CaptureRunDelta(ctx context.Context, worktree, sessionID string) ([]byte, error) {
	return captureRunDelta(ctx, worktree, sessionID)
}
