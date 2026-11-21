package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckoutLeavesLFSPointers pins that a checkout TF builds leaves LFS
// files as pointers on a host whose global config makes git-lfs's filter
// required, as `git lfs install` does. The repository's LFS objects exist
// nowhere, so any attempt to fetch one fails the clone.
func TestCheckoutLeavesLFSPointers(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skipf("git-lfs not installed: %v", err)
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[filter \"lfs\"]\n"+
		"\tclean = git-lfs clean -- %f\n"+
		"\tsmudge = git-lfs smudge -- %f\n"+
		"\tprocess = git-lfs filter-process\n"+
		"\trequired = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setTestHome(t, t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_LFS_SKIP_SMUDGE", "0")

	pointer := "version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\n" +
		"size 12\n"
	seed := t.TempDir()
	bare := filepath.Join(t.TempDir(), "repo.git")
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "--initial-branch=main", seed},
		{"init", "--bare", "--initial-branch=main", bare},
	} {
		if err := gitRunCtx(ctx, "", args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(seed, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// git-lfs's clean filter leaves a valid pointer as it is, so the commit
	// holds exactly this text.
	if err := os.WriteFile(filepath.Join(seed, "asset.bin"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"-c", "user.name=Seed", "-c", "user.email=seed@example.test", "commit", "-m", "seed"},
		// --no-verify: running the clean filter installed git-lfs's own
		// pre-push hook here, which would try to upload an object that
		// exists nowhere.
		{"push", "--no-verify", bare, "main"},
	} {
		if err := gitRunCtx(ctx, seed, args...); err != nil {
			t.Fatal(err)
		}
	}

	dst := filepath.Join(t.TempDir(), "checkout")
	if err := gitRunCtx(ctx, "", "clone", bare, dst); err != nil {
		t.Fatalf("checkout of a repository with unreachable LFS objects: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "asset.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != pointer {
		t.Errorf("asset.bin = %q, want its pointer", got)
	}
	if out, err := gitOutputCtx(ctx, dst, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("checkout is not clean (%v):\n%s", err, out)
	}
}
