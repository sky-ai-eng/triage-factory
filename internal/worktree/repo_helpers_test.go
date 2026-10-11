// Shared helpers for tests that build small repos and assert on files.

package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

// testRepo is the Repo a test names by its slug: a stand-in row id built from
// the slug, so two tests' repositories never share a bare and one test's
// repeated calls always reach the same one.
func testRepo(owner, repo string) Repo {
	return Repo{ID: "repo-" + owner + "-" + repo, Owner: owner, Name: repo}
}

// registerWorktree records a linked worktree named name against bare, its
// gitdir pointing into checkout — the admin entry `git worktree add` writes,
// without the git.
func registerWorktree(t *testing.T, bare, name, checkout string) {
	t.Helper()
	adminDir := filepath.Join(bare, "worktrees", name)
	if err := os.MkdirAll(adminDir, 0o755); err != nil {
		t.Fatalf("register worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(adminDir, "gitdir"), []byte(filepath.Join(checkout, ".git")+"\n"), 0o644); err != nil {
		t.Fatalf("write gitdir: %v", err)
	}
}
