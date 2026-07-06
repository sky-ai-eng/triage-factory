//go:build !linux

package delegate

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// captureIsolated has no non-Linux implementation: the dropped-privilege +
// network-namespace child needs Linux, and multi mode is Linux-only
// (shouldSandbox gates on it), so captureWorkspaceGit never routes here off
// Linux. Present only to keep the package building on dev machines; it runs the
// capture in-process as a safe fallback.
func captureIsolated(ctx context.Context, wtPath string) (*worktree.GitDelta, error) {
	return worktree.CaptureWorkspaceGit(ctx, wtPath)
}
