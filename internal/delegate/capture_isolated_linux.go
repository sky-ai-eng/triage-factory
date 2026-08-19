//go:build linux

package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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
// capture op. The child writes large members into fixed parent-owned staging
// files; this function decodes and validates only their small JSON manifest.
func captureIsolated(ctx context.Context, wtPath, sessionID string) (_ worktree.CapturedState, cleanup func(), err error) {
	stagingDir, err := prepareCaptureStaging()
	if err != nil {
		return worktree.CapturedState{}, nil, fmt.Errorf("isolated capture: prepare staging: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(stagingDir) }
	defer func() {
		if err != nil {
			cleanup()
			cleanup = nil
		}
	}()
	raw, err := captureViaSandbox(ctx, wtPath, stagingDir, sessionID)
	if err != nil {
		return worktree.CapturedState{}, cleanup, err
	}
	out := bytes.TrimSpace(raw)
	if len(out) == 0 {
		return worktree.CapturedState{}, cleanup, nil // defensive: the child always encodes a state object
	}
	var state worktree.CapturedState
	if err := json.Unmarshal(out, &state); err != nil {
		return worktree.CapturedState{}, cleanup, fmt.Errorf("isolated capture: decode captured state: %w", err)
	}
	if len(state.Transcript) != 0 || (state.Delta != nil && (len(state.Delta.Bundle) != 0 || len(state.Delta.Patch) != 0)) {
		return worktree.CapturedState{}, cleanup, fmt.Errorf("isolated capture: child returned buffered payload members")
	}
	if state.SessionID != sessionID {
		return worktree.CapturedState{}, cleanup, fmt.Errorf("isolated capture: manifest session id %q does not match %q", state.SessionID, sessionID)
	}
	for label, path := range map[string]string{
		"bundle":     state.BundlePath,
		"patch":      state.PatchPath,
		"transcript": state.TranscriptPath,
	} {
		if path == "" {
			continue
		}
		want := filepath.Join(stagingDir, captureMemberName(label))
		if path != want {
			return worktree.CapturedState{}, cleanup, fmt.Errorf("isolated capture: %s path %q is outside prepared staging member %q", label, path, want)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return worktree.CapturedState{}, cleanup, fmt.Errorf("isolated capture: stat %s: %w", label, err)
		}
		if !info.Mode().IsRegular() {
			return worktree.CapturedState{}, cleanup, fmt.Errorf("isolated capture: %s staging member is not a regular file", label)
		}
	}
	return state, cleanup, nil
}

func captureMemberName(label string) string {
	switch label {
	case "bundle":
		return worktree.CaptureBundleFile
	case "patch":
		return worktree.CapturePatchFile
	default:
		return worktree.CaptureTranscriptFile
	}
}

func prepareCaptureStaging() (string, error) {
	dir, err := os.MkdirTemp("", "tf-capture-*")
	if err != nil {
		return "", err
	}
	clean := true
	defer func() {
		if clean {
			_ = os.RemoveAll(dir)
		}
	}()
	for _, name := range []string{worktree.CaptureBundleFile, worktree.CapturePatchFile, worktree.CaptureTranscriptFile} {
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	clean = false
	return dir, nil
}
