// Package snapshotcapture implements the internal
// `triagefactory snapshot-capture <worktree-path> <staging-dir> [session-id]` subcommand: it
// captures one parked run's non-recoverable, agent-owned state — the git delta
// AND (when a session id is given) the Claude session transcript — into staged
// files, then writes their small worktree.CapturedState manifest to stdout as
// JSON. Undocumented in --help, like `hook`.
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
	"path/filepath"

	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// Handle dispatches `triagefactory snapshot-capture <worktree-path> <staging-dir> [session-id]`.
// The session id is optional: absent (or empty) captures only the git delta,
// matching a caller that has no session to preserve.
func Handle(args []string) {
	if len(args) < 2 || len(args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: triagefactory snapshot-capture <worktree-path> <staging-dir> [session-id]")
		os.Exit(2)
	}
	sessionID := ""
	if len(args) == 3 {
		sessionID = args[2]
	}
	outputs, err := inheritedOutputs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "snapshot-capture:", err)
		os.Exit(1)
	}
	defer outputs.Close()
	if err := Run(context.Background(), args[0], args[1], sessionID, outputs, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "snapshot-capture:", err)
		os.Exit(1)
	}
}

// Outputs are parent-owned staging files inherited across exec. The child can
// write through these already-open descriptors without gaining path access to
// their 0700 directory or 0600 files.
type Outputs struct {
	Bundle     *os.File
	Patch      *os.File
	Transcript *os.File
}

func inheritedOutputs() (Outputs, error) {
	var out Outputs
	for _, inherited := range []struct {
		fd   uintptr
		name string
		dst  **os.File
	}{
		{fd: 3, name: worktree.CaptureBundleFile, dst: &out.Bundle},
		{fd: 4, name: worktree.CapturePatchFile, dst: &out.Patch},
		{fd: 5, name: worktree.CaptureTranscriptFile, dst: &out.Transcript},
	} {
		f, err := inheritedOutput(inherited.fd, inherited.name)
		if err != nil {
			out.Close()
			return Outputs{}, err
		}
		*inherited.dst = f
	}
	return out, nil
}

func inheritedOutput(fd uintptr, name string) (*os.File, error) {
	f := os.NewFile(fd, name)
	if f == nil {
		return nil, fmt.Errorf("capture staging descriptor %d (%s) is missing", fd, name)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("capture staging descriptor %d (%s) is invalid: %w", fd, name, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("capture staging descriptor %d (%s) is not a regular file", fd, name)
	}
	return f, nil
}

func (o Outputs) Close() {
	for _, f := range []*os.File{o.Bundle, o.Patch, o.Transcript} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// Run captures wtPath's git delta and, when sessionID is non-empty, its Claude
// session transcript, streaming large members into stagingDir and writing the
// small worktree.CapturedState manifest as JSON to w.
// A missing transcript is not an error — it rides as an empty field, and the
// resume-side guard reports the unresumable run cleanly. The testable core of
// Handle, separated from argv parsing + os.Exit so the child's actual
// capture-then-encode body can be exercised without spawning a process.
func Run(ctx context.Context, wtPath, stagingDir, sessionID string, outputs Outputs, w io.Writer) error {
	if outputs.Bundle == nil || outputs.Patch == nil || outputs.Transcript == nil {
		return fmt.Errorf("capture staging outputs are incomplete")
	}
	bundlePath := filepath.Join(stagingDir, worktree.CaptureBundleFile)
	patchPath := filepath.Join(stagingDir, worktree.CapturePatchFile)
	transcriptPath := filepath.Join(stagingDir, worktree.CaptureTranscriptFile)
	delta, hasBundle, hasPatch, err := worktree.CaptureWorkspaceGitToWriters(ctx, wtPath, outputs.Bundle, outputs.Patch)
	if err != nil {
		return err
	}
	state := worktree.CapturedState{Delta: delta, SessionID: sessionID}
	if hasBundle {
		state.BundlePath = bundlePath
	}
	if hasPatch {
		state.PatchPath = patchPath
	}
	// The child is spawned only for a multi-mode sandboxed run (see
	// delegate.captureWorkspaceGit), and carries no TF_MODE, so it reads the
	// transcript from the run-root sandbox layout directly rather than via the
	// runmode-aware path — and confined against a symlink escape.
	hasTranscript, err := worktree.CopySandboxSessionTranscript(wtPath, sessionID, outputs.Transcript)
	if err != nil {
		return err
	}
	if hasTranscript {
		state.TranscriptPath = transcriptPath
	}
	if err := json.NewEncoder(w).Encode(state); err != nil {
		return fmt.Errorf("encode captured state: %w", err)
	}
	return nil
}
