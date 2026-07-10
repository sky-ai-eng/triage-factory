//go:build linux

package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// captureViaSandbox is the seam to the privileged capture op —
// sandbox.CaptureRunDelta routes through the PrivilegedOps implementation
// (the cap-broker client, since both halves of the capture child's
// confinement — the setuid away from the orchestrator's identity and the
// CLONE_NEWNET — need capabilities the post-drop orchestrator no longer
// holds). A package var so tests can pin routing without a real privileged
// child.
var captureViaSandbox = sandbox.CaptureRunDelta

// captureIsolated captures wtPath's git delta via the privileged capture
// op and decodes the child's JSON output. Empty / "null" output means a
// non-git run root: no delta.
func captureIsolated(ctx context.Context, wtPath string) (*worktree.GitDelta, error) {
	raw, err := captureViaSandbox(ctx, wtPath)
	if err != nil {
		return nil, err
	}
	out := bytes.TrimSpace(raw)
	if len(out) == 0 || string(out) == "null" {
		return nil, nil // non-git run root: no delta
	}
	var delta worktree.GitDelta
	if err := json.Unmarshal(out, &delta); err != nil {
		return nil, fmt.Errorf("isolated capture: decode delta: %w", err)
	}
	return &delta, nil
}
