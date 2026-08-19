package snapshotcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestRun_EmitsStagedDelta exercises the snapshot-capture child body end to
// end: the large patch lands in the parent-prepared file while stdout carries
// only the small manifest.
func TestRun_EmitsStagedDelta(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "work")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "seed")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	staging, outputs := captureStaging(t)
	if err := Run(context.Background(), dir, staging, "", outputs, &buf); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var state worktree.CapturedState
	if err := json.Unmarshal(buf.Bytes(), &state); err != nil {
		t.Fatalf("decode captured state: %v (out=%q)", err, buf.String())
	}
	if state.Delta == nil {
		t.Fatal("delta is nil; a git worktree's delta was not captured")
	}
	if state.Delta.Branch != "work" {
		t.Errorf("branch = %q, want work", state.Delta.Branch)
	}
	if state.Delta.Head == "" {
		t.Error("head is empty")
	}
	if state.BundlePath != filepath.Join(staging, worktree.CaptureBundleFile) {
		t.Errorf("bundle path = %q, want staged member", state.BundlePath)
	} else if bundle, err := os.Stat(state.BundlePath); err != nil || bundle.Size() == 0 {
		t.Errorf("staged bundle = %v, %v; want local-only commit data", bundle, err)
	}
	if len(state.Delta.Patch) != 0 {
		t.Error("patch bytes rode the manifest; want a staged path only")
	}
	if state.PatchPath != filepath.Join(staging, worktree.CapturePatchFile) {
		t.Errorf("patch path = %q, want staged member", state.PatchPath)
	}
	patch, err := os.ReadFile(state.PatchPath)
	if err != nil || len(patch) == 0 {
		t.Errorf("staged patch = %d bytes, %v; want the uncommitted change", len(patch), err)
	}
}

// TestRun_NonGitRoot pins that a non-git run root emits a state whose delta is
// nil (captureIsolated maps that to no delta), not an error.
func TestRun_NonGitRoot(t *testing.T) {
	var buf bytes.Buffer
	staging, outputs := captureStaging(t)
	if err := Run(context.Background(), t.TempDir(), staging, "", outputs, &buf); err != nil {
		t.Fatalf("Run on a non-git dir: %v", err)
	}
	var state worktree.CapturedState
	if err := json.Unmarshal(buf.Bytes(), &state); err != nil {
		t.Fatalf("decode captured state: %v (out=%q)", err, buf.String())
	}
	if state.Delta != nil {
		t.Errorf("non-git root emitted a delta %+v, want nil", state.Delta)
	}
}

// TestRun_CapturesSessionTranscript pins the reason this child gained a session
// id: it reads the SDK's session transcript (which the orchestrator cannot) and
// emits it in the state. A non-git run root with only a transcript is exactly
// the Jira/Slack lazy shape whose resume was failing.
func TestRun_CapturesSessionTranscript(t *testing.T) {
	dir := t.TempDir()
	const sessionID = "sess-xyz"
	// The child reads a sandboxed run's transcript from the run-root layout, so
	// seed it exactly where ReadSandboxSessionTranscript looks.
	sessPath := worktree.SandboxClaudeSessionPath(dir, sessionID)
	if err := os.MkdirAll(filepath.Dir(sessPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessPath, []byte(`{"type":"summary"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	staging, outputs := captureStaging(t)
	if err := Run(context.Background(), dir, staging, sessionID, outputs, &buf); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var state worktree.CapturedState
	if err := json.Unmarshal(buf.Bytes(), &state); err != nil {
		t.Fatalf("decode captured state: %v", err)
	}
	if len(state.Transcript) != 0 {
		t.Error("transcript bytes rode the manifest; want a staged path only")
	}
	transcript, err := os.ReadFile(state.TranscriptPath)
	if err != nil || string(transcript) != `{"type":"summary"}` {
		t.Errorf("staged transcript = %q, %v; want the session JSONL bytes", transcript, err)
	}
}

func captureStaging(t *testing.T) (string, Outputs) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{worktree.CaptureBundleFile, worktree.CapturePatchFile, worktree.CaptureTranscriptFile} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	open := func(name string) *os.File {
		t.Helper()
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	outputs := Outputs{
		Bundle:     open(worktree.CaptureBundleFile),
		Patch:      open(worktree.CapturePatchFile),
		Transcript: open(worktree.CaptureTranscriptFile),
	}
	t.Cleanup(outputs.Close)
	return dir, outputs
}
