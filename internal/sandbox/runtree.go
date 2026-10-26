package sandbox

import (
	"context"
	"os"
	"path/filepath"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// runTreeBasename is the directory ephemeral per-run worktrees live under
// inside os.TempDir() locally: os.TempDir()/triagefactory-runs/<rootKey>, and
// runTreeStateDir the one they live under inside the state root in multi:
// <state root>/runs/<rootKey>. This is the GitHub-PR / Jira / Slack task-run
// shape of Config.Worktree — org-blind by construction, since these trees
// don't outlive their task. internal/worktree is the owner of this path (its
// MakeRunRoot materializes exactly here); it duplicates these literals in its
// own private runsDir / stateRunsDir constants rather than importing
// RunTreeRoot (worktree already imports this package for ChownRunTree /
// RemoveRunTree, and having the broker-side validator define the trusted
// shape independently — rather than depend on the producer's constant —
// mirrors how TrustedGitHooksDir resolves elsewhere in this package). A
// worktree package test cross-checks the two stay equal in both modes.
const (
	runTreeBasename = "triagefactory-runs"
	runTreeStateDir = "runs"
)

// RunTreeRoot returns the ephemeral per-run worktree root for rootKey. This
// is the ONLY legitimate shape for a delegated task run's Config.Worktree;
// launchspec_linux.go's mount-source validation rejects any Worktree that
// is neither this exact path nor under the org-scoped state-root tree
// (see worktreeScope).
//
// In multi it is under the state root, the executor's data volume, beside
// every jail's /tmp: an executor's temp dir is its container's writable layer.
// The broker resolves the state root from the same environment the
// orchestrator does, as it does for the jails' /tmp.
//
// rootKey is the tree's key, not a conversation id: a delegated run's tree is
// keyed by its workspace key (the task id), so a task's conversations share one
// root and a resumed one rebuilds at the same path. Both keys reach this
// package — the launch pins the worktree by workspace key while
// Config.ConversationID stays the conversation — which is why worktreeScope
// accepts either and this parameter claims neither.
func RunTreeRoot(rootKey string) string {
	if runmode.Current() == runmode.ModeMulti {
		return filepath.Join(paths.StateRoot(), runTreeStateDir, rootKey)
	}
	return filepath.Join(os.TempDir(), runTreeBasename, rootKey)
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

// ChownRunTree hands ownership of a run tree to a jail identity — jailUID,
// the uid of the engagement that is about to run in it (JailUID of its subnet
// index) — so the jailed agent can write its own worktree. subpath == ""
// chowns the whole root recursively (every claim: a task's tree outlives an
// engagement, and the next claim may hold a different index); a non-empty
// subpath is the mid-run `workspace add` case — intermediate directories
// shallowly, the subpath tree recursively — and joins the checkout to the
// engagement already holding the root, so jailUID must be the root's owner.
// No-op off Linux (the sandbox path isn't reachable there).
func ChownRunTree(ctx context.Context, root, subpath string, jailUID int) error {
	return chownRunTree(ctx, root, subpath, jailUID)
}

// RunTreeJailUID returns the jail uid that holds the run tree at path, if one
// does: the identity a checkout added mid-run must be handed to. False when
// the tree is not held by an engagement, and always off Linux.
func RunTreeJailUID(path string) (int, bool) {
	return runTreeJailUID(path)
}

// MkdirRunTreeScaffold creates the directories rel names under root — the
// owner/ and owner/repo/ levels a checkout is materialized beneath — as run
// tree SCAFFOLD, group-writable from birth rather than only from the
// ownership hand-off that follows a successful create.
//
// Birth is the load-bearing word. The hand-off's recursive form preserves
// whatever modes it finds, so a scaffold directory that reaches it at the
// 0755 an ordinary mkdir produces is frozen sandbox-owned and orchestrator-
// unwritable, and every later checkout under that owner fails EACCES. Minting
// it group-writable makes the scaffold contract hold for the life of the tree
// no matter which creates in between succeeded.
//
// rel is taken apart and walked one component at a time off a pinned
// directory fd, never re-resolved as a path string: the run root is writable
// by the jailed agent, so a component swapped for a symlink between the
// create and the mode assertion would otherwise redirect that assertion out
// of the tree. Off Linux this is the plain MkdirAll the callers used before
// the scaffold contract existed — no sandbox identity owns these trees there.
func MkdirRunTreeScaffold(root, rel string) error {
	return mkdirRunTreeScaffold(root, rel)
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

// RunTreeHandedOff reports whether path has already been handed to a jail
// identity — i.e. a launch chowned it (to a jail uid, or to ParkedRunTreeUID
// once that engagement ended) and the calling (orchestrator) process no longer
// owns it. That hand-off is one-way for the life of the run tree, so this
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

// CaptureRunTree captures a run tree's non-recoverable state in a one-shot
// jail the cap-broker builds: p.Root's checkouts' git deltas and its _tfac
// scratch, framed (CaptureStreamWriter) into p.StagingDir's CaptureStreamFile,
// and a small JSON manifest returned. Linux-only by construction; the
// non-Linux stub errors, and the delegate caller never routes here off Linux.
func CaptureRunTree(ctx context.Context, p CaptureParams) ([]byte, error) {
	return captureRunTree(ctx, p)
}
