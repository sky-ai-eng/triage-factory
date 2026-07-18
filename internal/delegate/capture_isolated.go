package delegate

import (
	"context"
	"runtime"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// captureWorkspaceGit captures a parked run's git delta, choosing where the
// filter-honoring git runs by deployment mode.
//
// In multi mode the run root is chowned to the sandbox uid, so its .git/config
// and tracked .gitattributes are agent-writable. captureUncommitted's
// `git add -A` / `git diff` consult those to decide whether to invoke an
// external clean/smudge/textconv/diff driver — so running them as the host
// (root) process is a sandbox escape: a compromised agent plants the driver and
// gets root to execute it during the parking snapshot. There it runs in a
// dropped-privilege, network-isolated child (the sandbox uid, an empty netns),
// so a fired filter is confined to exactly the privilege the jailed agent
// already had, and the parking snapshot's own git (rev-parse/bundle/diff) stops
// tripping dubious-ownership because the reader's uid now matches the owner.
//
// Local mode has no sandbox and no chown — the operator runs their own repos on
// a trusted machine — so the capture runs in-process, unchanged.
//
// The session transcript rides out of the SAME capture: in multi mode the
// child (running as the sandbox uid) is the only identity that can read the
// SDK's owner-only transcript, so it reads it there; in local mode the
// in-process reader already IS that uid. Either way the orchestrator, which can
// read neither the transcript nor a chowned worktree, never touches the files.
// transcript is empty when the run has no session or the file was absent.
func captureWorkspaceGit(ctx context.Context, wtPath, sessionID string) (delta *worktree.GitDelta, transcript []byte, err error) {
	if runmode.Current() != runmode.ModeMulti || runtime.GOOS != "linux" {
		delta, err = worktree.CaptureWorkspaceGit(ctx, wtPath)
		if err != nil {
			return nil, nil, err
		}
		if data, ok := worktree.ReadClaudeSessionTranscript(wtPath, sessionID); ok {
			transcript = data
		}
		return delta, transcript, nil
	}
	return captureIsolated(ctx, wtPath, sessionID)
}
