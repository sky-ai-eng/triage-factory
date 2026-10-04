package worktree

import (
	"os"
	"path/filepath"
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
// as real files inside the run root it owns, so nothing is planted and released
// local behavior is byte-identical.
func TestEnsureSandboxMemoryLink_LocalModeIsNoOp(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	runmode.SetForTest(t, runmode.ModeLocal)

	dir := t.TempDir()
	if err := EnsureSandboxMemoryLink(dir); err != nil {
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

	if err := EnsureSandboxMemoryLink(dir); err != nil {
		t.Fatalf("first plant: %v", err)
	}
	assertMemorySymlink(t, dir)

	if os.Geteuid() == 0 {
		// root bypasses the mode bits, so the no-write assertion can't hold; still
		// exercise plain idempotency.
		if err := EnsureSandboxMemoryLink(dir); err != nil {
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
	if err := EnsureSandboxMemoryLink(dir); err != nil {
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
		real := filepath.Join(dir, ScratchDir, EntityMemoryDir, "this-task")
		if err := os.MkdirAll(real, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(real, "01-triage.md"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := EnsureSandboxMemoryLink(dir); err != nil {
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
		if err := EnsureSandboxMemoryLink(dir); err != nil {
			t.Fatalf("plant over a stale link: %v", err)
		}
		assertMemorySymlink(t, dir)
	})
}
