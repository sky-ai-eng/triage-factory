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
// The session transcript is read by that SAME child in multi mode, then written
// into a parent-owned staging file; the orchestrator never opens agent-owned
// files. Local mode keeps the existing in-memory representation. The transcript
// member is absent when the run has no session or the file was absent.
func captureWorkspaceGit(ctx context.Context, wtPath, sessionID string) (state worktree.CapturedState, cleanup func(), err error) {
	if runmode.Current() != runmode.ModeMulti || runtime.GOOS != "linux" {
		state.Delta, err = worktree.CaptureWorkspaceGit(ctx, wtPath)
		if err != nil {
			return worktree.CapturedState{}, nil, err
		}
		state.SessionID = sessionID
		if data, ok := worktree.ReadClaudeSessionTranscript(wtPath, sessionID); ok {
			state.Transcript = data
		}
		return state, nil, nil
	}
	return captureIsolated(ctx, wtPath, sessionID)
}
