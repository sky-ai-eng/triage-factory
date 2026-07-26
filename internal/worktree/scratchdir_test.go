package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestManagedExcludes_CoverBothScratchNames pins that the exclude block claims
// the legacy directory name too. A worktree built by an older binary keeps its
// scratch under that name, and an unexcluded leftover is one the agent's next
// `git add -A` would commit into someone's PR.
func TestManagedExcludes_CoverBothScratchNames(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)
	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("writeLocalExcludes: %v", err)
	}
	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	for _, want := range []string{ScratchDir + "/", legacyScratchDir + "/"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("exclude file is missing %q:\n%s", want, content)
		}
	}
}

func TestAdoptLegacyScratchDir(t *testing.T) {
	ctx := context.Background()

	t.Run("renames a legacy dir the repo does not track", func(t *testing.T) {
		dir := t.TempDir()
		writeUnder(t, dir, filepath.Join(legacyScratchDir, "review", "bpr-1", "security.json"), `{"findings":[]}`)

		AdoptLegacyScratchDir(ctx, dir)

		assertFileContent(t, filepath.Join(dir, ScratchDir, "review", "bpr-1", "security.json"), `{"findings":[]}`)
		if _, err := os.Stat(filepath.Join(dir, legacyScratchDir)); !os.IsNotExist(err) {
			t.Errorf("legacy dir survived the adoption: %v", err)
		}
	})

	t.Run("leaves the current dir alone when both exist", func(t *testing.T) {
		dir := t.TempDir()
		writeUnder(t, dir, filepath.Join(legacyScratchDir, "note.md"), "old")
		writeUnder(t, dir, filepath.Join(ScratchDir, "note.md"), "current")

		AdoptLegacyScratchDir(ctx, dir)

		assertFileContent(t, filepath.Join(dir, ScratchDir, "note.md"), "current")
		assertFileContent(t, filepath.Join(dir, legacyScratchDir, "note.md"), "old")
	})

	t.Run("never moves a dir the repo tracks", func(t *testing.T) {
		dir := t.TempDir()
		initRepoAt(t, dir)
		writeUnder(t, dir, filepath.Join(legacyScratchDir, "committed.md"), "repo content")
		gitAt(t, dir, "add", "-A")
		gitAt(t, dir, "commit", "-qm", "track it")

		AdoptLegacyScratchDir(ctx, dir)

		assertFileContent(t, filepath.Join(dir, legacyScratchDir, "committed.md"), "repo content")
		if out := gitAt(t, dir, "status", "--porcelain"); strings.TrimSpace(out) != "" {
			t.Errorf("adoption dirtied the repo: %q", out)
		}
	})

	t.Run("no legacy dir is a no-op", func(t *testing.T) {
		dir := t.TempDir()
		AdoptLegacyScratchDir(ctx, dir)
		if _, err := os.Stat(filepath.Join(dir, ScratchDir)); !os.IsNotExist(err) {
			t.Errorf("adoption invented a scratch dir: %v", err)
		}
	})
}

func TestTrackedUnder(t *testing.T) {
	ctx := context.Background()

	t.Run("reports what the repo tracks under the prefix", func(t *testing.T) {
		dir := t.TempDir()
		initRepoAt(t, dir)
		writeUnder(t, dir, filepath.Join(ScratchDir, "memory.md"), "repo content")
		writeUnder(t, dir, "README.md", "x")
		gitAt(t, dir, "add", "-A")
		gitAt(t, dir, "commit", "-qm", "init")
		// An untracked sibling under the same prefix stays absent from the set.
		writeUnder(t, dir, filepath.Join(ScratchDir, "ours.md"), "TF wrote this")

		tracked := TrackedUnder(ctx, dir, ScratchDir)
		if !tracked[ScratchDir+"/memory.md"] {
			t.Errorf("tracked = %v, want the committed path", tracked)
		}
		if tracked[ScratchDir+"/ours.md"] {
			t.Errorf("tracked = %v, want no untracked path", tracked)
		}
		if tracked["README.md"] {
			t.Errorf("tracked = %v, want nothing outside the prefix", tracked)
		}
	})

	t.Run("a dir that is no repo tracks nothing", func(t *testing.T) {
		dir := t.TempDir()
		writeUnder(t, dir, filepath.Join(ScratchDir, "memory.md"), "x")
		if got := TrackedUnder(ctx, dir, ScratchDir); len(got) != 0 {
			t.Errorf("TrackedUnder on a non-repo = %v, want empty", got)
		}
	})
}

// initRepoAt makes dir a git working tree, skipping when git isn't installed.
func initRepoAt(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	gitAt(t, dir, "init", "-q", ".")
	gitAt(t, dir, "config", "user.email", "test@example.com")
	gitAt(t, dir, "config", "user.name", "Test")
}

func gitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeUnder(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}
