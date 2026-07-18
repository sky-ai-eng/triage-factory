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

// captureIsolated captures wtPath's non-recoverable state via the privileged
// capture op and decodes the child's JSON worktree.CapturedState — the git
// delta (nil for a non-git run root) and, when sessionID is set, the session
// transcript the child read as the sandbox uid.
func captureIsolated(ctx context.Context, wtPath, sessionID string) (*worktree.GitDelta, []byte, error) {
	raw, err := captureViaSandbox(ctx, wtPath, sessionID)
	if err != nil {
		return nil, nil, err
	}
	out := bytes.TrimSpace(raw)
	if len(out) == 0 {
		return nil, nil, nil // defensive: the child always encodes a state object
	}
	var state worktree.CapturedState
	if err := json.Unmarshal(out, &state); err != nil {
		return nil, nil, fmt.Errorf("isolated capture: decode captured state: %w", err)
	}
	return state.Delta, state.Transcript, nil
}
