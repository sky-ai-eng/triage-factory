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
	if err := Run(context.Background(), dir, &buf); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var delta worktree.GitDelta
	if err := json.Unmarshal(buf.Bytes(), &delta); err != nil {
		t.Fatalf("decode delta: %v (out=%q)", err, buf.String())
	}
	if delta.Branch != "work" {
		t.Errorf("branch = %q, want work", delta.Branch)
	}
	if delta.Head == "" {
		t.Error("head is empty")
	}
	if len(delta.Patch) == 0 {
		t.Error("patch is empty; the uncommitted change was not captured")
	}
}

// TestRun_NonGitRoot pins that a non-git run root emits the null delta
// (which captureIsolated maps back to nil), not an error.
func TestRun_NonGitRoot(t *testing.T) {
	var buf bytes.Buffer
	if err := Run(context.Background(), t.TempDir(), &buf); err != nil {
		t.Fatalf("Run on a non-git dir: %v", err)
	}
	if got := bytes.TrimSpace(buf.Bytes()); string(got) != "null" {
		t.Errorf("non-git root emitted %q, want null", got)
	}
}
