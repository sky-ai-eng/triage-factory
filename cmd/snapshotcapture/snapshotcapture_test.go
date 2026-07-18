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

// TestRun_EmitsDecodableDelta exercises the snapshot-capture child body end
// to end: capture a real worktree's delta and JSON-encode it, as the
// re-exec'd child does, and assert the parent can decode a delta carrying
// the branch, head, and the uncommitted change.
func TestRun_EmitsDecodableDelta(t *testing.T) {
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
	if err := Run(context.Background(), dir, "", &buf); err != nil {
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
	if len(state.Delta.Patch) == 0 {
		t.Error("patch is empty; the uncommitted change was not captured")
	}
	if len(state.Transcript) != 0 {
		t.Error("transcript is non-empty; none was requested (empty session id)")
	}
}

// TestRun_NonGitRoot pins that a non-git run root emits a state whose delta is
// nil (captureIsolated maps that to no delta), not an error.
func TestRun_NonGitRoot(t *testing.T) {
	var buf bytes.Buffer
	if err := Run(context.Background(), t.TempDir(), "", &buf); err != nil {
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
	if err := Run(context.Background(), dir, sessionID, &buf); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var state worktree.CapturedState
	if err := json.Unmarshal(buf.Bytes(), &state); err != nil {
		t.Fatalf("decode captured state: %v", err)
	}
	if string(state.Transcript) != `{"type":"summary"}` {
		t.Errorf("transcript = %q, want the session JSONL bytes", state.Transcript)
	}
}
