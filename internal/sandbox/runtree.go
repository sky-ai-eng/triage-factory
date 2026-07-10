package sandbox

import "context"

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

// CaptureRunDelta runs the parked-run git-delta capture in a child
// dropped to the sandbox uid inside an empty network namespace and
// returns its raw JSON stdout (a worktree.GitDelta — decoded by the
// caller). Linux-only by construction; the non-Linux stub errors, and
// the delegate caller never routes here off Linux.
func CaptureRunDelta(ctx context.Context, worktree string) ([]byte, error) {
	return captureRunDelta(ctx, worktree)
}
