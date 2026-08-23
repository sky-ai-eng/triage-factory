package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

func assertMemorySymlink(t *testing.T, dir string) {
	t.Helper()
	link := filepath.Join(dir, ScratchDir, EntityMemoryDir)
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %s: %v", link, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink (mode %v)", link, fi.Mode())
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %s: %v", link, err)
	}
	if target != sandbox.TrustedMemoryDestination {
		t.Fatalf("%s -> %q, want the jail's memory mount %q", link, target, sandbox.TrustedMemoryDestination)
	}
}

// TestEnsureSandboxMemoryLink_LocalModeIsNoOp: local mode renders prior memory
// as real files inside the worktree it owns, so nothing is planted and released
// local behavior is byte-identical.
func TestEnsureSandboxMemoryLink_LocalModeIsNoOp(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	runmode.SetForTest(t, runmode.ModeLocal)

	dir := t.TempDir()
	if err := EnsureSandboxMemoryLink(context.Background(), dir); err != nil {
		t.Fatalf("EnsureSandboxMemoryLink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ScratchDir)); !os.IsNotExist(err) {
		t.Errorf("local mode created %s/%s; want the tree untouched (err=%v)", dir, ScratchDir, err)
	}
}

// TestEnsureSandboxMemoryLink_PlantsAndIsIdempotent covers the two properties the
// warm-step handoff rests on: the link lands on a fresh tree, and a SECOND call
// against an already-correct link takes no write at all. The no-write half is
// asserted the only way that can't pass vacuously — by making the parent
// unwritable first, which is the situation at a real step boundary (the tree
// belongs to the sandbox uid and the orchestrator holds no CAP_DAC_OVERRIDE).
func TestEnsureSandboxMemoryLink_PlantsAndIsIdempotent(t *testing.T) {
	sandboxingMode(t)
	dir := t.TempDir()

	if err := EnsureSandboxMemoryLink(context.Background(), dir); err != nil {
		t.Fatalf("first plant: %v", err)
	}
	assertMemorySymlink(t, dir)

	if os.Geteuid() == 0 {
		// root bypasses the mode bits, so the no-write assertion can't hold; still
		// exercise plain idempotency.
		if err := EnsureSandboxMemoryLink(context.Background(), dir); err != nil {
			t.Fatalf("second plant: %v", err)
		}
		assertMemorySymlink(t, dir)
		return
	}

	scratch := filepath.Join(dir, ScratchDir)
	if err := os.Chmod(scratch, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", scratch, err)
	}
	t.Cleanup(func() { _ = os.Chmod(scratch, 0o755) })
	if err := EnsureSandboxMemoryLink(context.Background(), dir); err != nil {
		t.Fatalf("second plant into a write-protected scratch dir = %v; a warm tree must take NO write", err)
	}
	assertMemorySymlink(t, dir)
}

// TestEnsureSandboxMemoryLink_ForceReplaces: a real directory (what a tree built
// before the mount existed carries) and a stale symlink pointing somewhere else
// both converge on the correct link. TF owns this path outright.
func TestEnsureSandboxMemoryLink_ForceReplaces(t *testing.T) {
	sandboxingMode(t)

	t.Run("real_directory", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, ScratchDir, EntityMemoryDir, "this-run")
		if err := os.MkdirAll(real, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(real, "01-triage.md"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := EnsureSandboxMemoryLink(context.Background(), dir); err != nil {
			t.Fatalf("plant over a real dir: %v", err)
		}
		assertMemorySymlink(t, dir)
	})

	t.Run("wrong_target_symlink", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ScratchDir), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Symlink("/tmp/somewhere-else", filepath.Join(dir, ScratchDir, EntityMemoryDir)); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if err := EnsureSandboxMemoryLink(context.Background(), dir); err != nil {
			t.Fatalf("plant over a stale link: %v", err)
		}
		assertMemorySymlink(t, dir)
	})
}

// TestEnsureSandboxMemoryLink_NeverReplacesRepoContent: a run tree can BE the
// repo checkout, and git excludes do nothing for an already-tracked path — so a
// repo that commits files under this name keeps them. Replacing them with a
// symlink would show up as a deletion in the agent's next `git add -A` and ride
// into its PR. Losing this run's prior memory is the cheaper failure.
func TestEnsureSandboxMemoryLink_NeverReplacesRepoContent(t *testing.T) {
	sandboxingMode(t)
	dir := t.TempDir()
	initRepoAt(t, dir)

	writeUnder(t, dir, filepath.Join(ScratchDir, EntityMemoryDir, "committed.md"), "repo content")
	gitAt(t, dir, "add", "-A")
	gitAt(t, dir, "commit", "-qm", "track files under the memory dir")

	if err := EnsureSandboxMemoryLink(context.Background(), dir); err == nil {
		t.Error("planted over repo-tracked content; want a refusal")
	}
	assertFileContent(t, filepath.Join(dir, ScratchDir, EntityMemoryDir, "committed.md"), "repo content")
	if out := gitAt(t, dir, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("planting dirtied the repo: %q", out)
	}
}

// TestEnsureSandboxMemoryLink_LeavesTheScratchDirUsable pins that planting the
// link does not claim the whole scratch dir: the agent's own memory.md sits
// beside it at the fixed write path.
func TestEnsureSandboxMemoryLink_LeavesTheScratchDirUsable(t *testing.T) {
	sandboxingMode(t)
	dir := t.TempDir()

	if err := EnsureSandboxMemoryLink(context.Background(), dir); err != nil {
		t.Fatalf("plant: %v", err)
	}
	sibling := filepath.Join(dir, ScratchDir, "memory.md")
	if err := os.WriteFile(sibling, []byte("the agent's own write"), 0o644); err != nil {
		t.Fatalf("write beside the link: %v", err)
	}
	assertMemorySymlink(t, dir)
}
