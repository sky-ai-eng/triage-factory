package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCleanupWithOptions_SkipClaudeProjectCleanup is the non-local-mode
// path: the parked-worktree preserve set can't be determined, so we skip
// ALL ~/.claude/projects deletions. Worktree dirs and bare-repo pruning
// should still run — those leaks compound fast and aren't
// session-state-sensitive.
func TestCleanupWithOptions_SkipClaudeProjectCleanup(t *testing.T) {
	tmp := longTestTempDir(t)
	home := t.TempDir()
	setTestTmp(t, tmp)
	setTestHome(t, home)

	// One worktree dir + a corresponding ~/.claude/projects entry.
	wtDir := filepath.Join(tmp, runsDir, "run-skip")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(wtDir)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	encoded := encodeClaudeProjectDir(resolved)
	projectDir := filepath.Join(home, claudeProjectsDir, encoded)
	jsonlPath := filepath.Join(projectDir, "session.jsonl")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := os.WriteFile(jsonlPath, []byte("data"), 0644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	CleanupWithOptions(CleanupOptions{SkipClaudeProjectCleanup: true})

	// Worktree dir gone — the leak we care about.
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir should have been removed even with SkipClaudeProjectCleanup (err=%v)", err)
	}
	// Project dir preserved — we don't know if it belongs to a
	// takeover, so we err on the side of "leave it alone."
	if _, err := os.Stat(jsonlPath); err != nil {
		t.Errorf("session JSONL should have been preserved with SkipClaudeProjectCleanup (err=%v)", err)
	}
}

// TestRemoveAt_HandlesNonCanonicalPath guards review-comment fix #3:
// callers hold the source worktree path explicitly. If they used
// Remove(conversationID) (which derives the path from the canonical /tmp
// layout), they'd silently target the wrong directory whenever the
// source is elsewhere — leaking the actual source on disk and possibly
// destroying an unrelated conversationID's canonical dir.
//
// RemoveAt takes the path explicitly so this can't happen.
func TestRemoveAt_HandlesNonCanonicalPath(t *testing.T) {
	noncanonicalSrc := filepath.Join(t.TempDir(), "elsewhere", "wt-x")
	if err := os.MkdirAll(noncanonicalSrc, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Drop a marker file inside so we can verify the right dir is gone.
	if err := os.WriteFile(filepath.Join(noncanonicalSrc, "marker"), []byte("x"), 0644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	if err := RemoveAt(noncanonicalSrc, "run-x"); err != nil {
		t.Fatalf("RemoveAt: %v", err)
	}
	if _, err := os.Stat(noncanonicalSrc); !os.IsNotExist(err) {
		t.Errorf("non-canonical src should have been removed (err=%v)", err)
	}
}

// TestRemoveAt_EmptyPath is a no-op — guards against a caller passing
// an empty path (e.g., a no-worktree run that somehow reached this
// code path) from accidentally trying to RemoveAll("").
func TestRemoveAt_EmptyPath(t *testing.T) {
	if err := RemoveAt("", "run-y"); err != nil {
		t.Errorf("RemoveAt with empty path should be a no-op, got: %v", err)
	}
}

// TestCleanupWithOptions_NilPreserveSet is safe (no panic) and behaves
// like the legacy Cleanup() — every orphan's project dir gets nuked.
// Map reads on nil maps return the zero value in Go, so the index
// expression `opts.PreserveClaudeProjectFor[conversationID]` returns false and
// every run is treated as non-preserved.
func TestCleanupWithOptions_NilPreserveSet(t *testing.T) {
	tmp := longTestTempDir(t)
	home := t.TempDir()
	setTestTmp(t, tmp)
	setTestHome(t, home)

	runDir := filepath.Join(tmp, runsDir, "run-nil")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CleanupWithOptions panicked on nil PreserveClaudeProjectFor: %v", r)
		}
	}()
	CleanupWithOptions(CleanupOptions{}) // PreserveClaudeProjectFor is nil

	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("run dir should have been removed (err=%v)", err)
	}
}
