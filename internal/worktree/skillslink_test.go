package worktree

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// sandboxingMode puts the process in the mode that actually jails agents, so the
// planting gate is live. Skips off Linux, where WillSandbox is false regardless.
func sandboxingMode(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the sandbox path (and therefore the skills symlink) is Linux-only")
	}
	paths.SetForTest(t, t.TempDir())
	runmode.SetForTest(t, runmode.ModeMulti)
}

func assertSkillsSymlink(t *testing.T, dir string) {
	t.Helper()
	link := filepath.Join(dir, ".claude", "skills")
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
	if target != sandbox.TrustedSkillsDestination {
		t.Fatalf("%s -> %q, want the jail's skills mount %q", link, target, sandbox.TrustedSkillsDestination)
	}
}

// TestEnsureSandboxSkillsLink_LocalModeIsNoOp: local mode keeps materializing the
// step skill inside the worktree, so nothing is planted and released local
// behavior is byte-identical.
func TestEnsureSandboxSkillsLink_LocalModeIsNoOp(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	runmode.SetForTest(t, runmode.ModeLocal)

	dir := t.TempDir()
	if err := EnsureSandboxSkillsLink(dir); err != nil {
		t.Fatalf("EnsureSandboxSkillsLink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".claude")); !os.IsNotExist(err) {
		t.Errorf("local mode created %s/.claude; want the tree untouched (err=%v)", dir, err)
	}
}

// TestEnsureSandboxSkillsLink_PlantsAndIsIdempotent covers the two properties the
// step-boundary fix rests on: the link lands on a fresh tree, and a SECOND call
// against an already-correct link takes no write at all. The no-write half is
// asserted the only way that can't pass vacuously — by making the parent
// unwritable first, which is exactly the situation at a real step boundary
// (the tree belongs to the sandbox uid and the orchestrator holds no
// CAP_DAC_OVERRIDE).
func TestEnsureSandboxSkillsLink_PlantsAndIsIdempotent(t *testing.T) {
	sandboxingMode(t)
	dir := t.TempDir()

	if err := EnsureSandboxSkillsLink(dir); err != nil {
		t.Fatalf("first plant: %v", err)
	}
	assertSkillsSymlink(t, dir)

	if os.Geteuid() == 0 {
		// root bypasses the mode bits, so the no-write assertion can't hold; still
		// exercise plain idempotency.
		if err := EnsureSandboxSkillsLink(dir); err != nil {
			t.Fatalf("second plant: %v", err)
		}
		assertSkillsSymlink(t, dir)
		return
	}

	claudeDir := filepath.Join(dir, ".claude")
	if err := os.Chmod(claudeDir, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", claudeDir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(claudeDir, 0o755) })
	if err := EnsureSandboxSkillsLink(dir); err != nil {
		t.Fatalf("second plant into a write-protected .claude = %v; a warm tree must take NO write", err)
	}
	assertSkillsSymlink(t, dir)
}

// TestEnsureSandboxSkillsLink_ForceReplaces: a real directory (what a pre-fix
// snapshot's patch or a repo that tracks .claude/skills leaves behind) and a
// stale symlink pointing somewhere else both converge on the correct link. TF
// owns <tree>/.claude/skills outright — the same contract the local-mode wipe
// has always had.
func TestEnsureSandboxSkillsLink_ForceReplaces(t *testing.T) {
	sandboxingMode(t)

	t.Run("real_directory", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, ".claude", "skills", "project-skill")
		if err := os.MkdirAll(real, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(real, "SKILL.md"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := EnsureSandboxSkillsLink(dir); err != nil {
			t.Fatalf("plant over a real dir: %v", err)
		}
		assertSkillsSymlink(t, dir)
	})

	t.Run("wrong_target_symlink", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Symlink("/tmp/somewhere-else", filepath.Join(dir, ".claude", "skills")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if err := EnsureSandboxSkillsLink(dir); err != nil {
			t.Fatalf("plant over a stale link: %v", err)
		}
		assertSkillsSymlink(t, dir)
	})
}

// TestSweepOrphanedStagingDirs reclaims leftover per-run staging dirs of both
// kinds a launch mounts, while honoring the caller's warm-worktree keep set.
func TestSweepOrphanedStagingDirs(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	bases := []string{sandbox.SkillStagingBase(), sandbox.MemoryStagingBase()}
	for _, base := range bases {
		for _, name := range []string{"gone-run", "kept-run"} {
			if err := os.MkdirAll(filepath.Join(base, name, "chain-step-0-x"), 0o755); err != nil {
				t.Fatalf("seed %s: %v", name, err)
			}
		}
	}

	sweepOrphanedStagingDirs(map[string]bool{"kept-run": true})

	for _, base := range bases {
		if _, err := os.Stat(filepath.Join(base, "gone-run")); !os.IsNotExist(err) {
			t.Errorf("orphaned staging dir under %s survived the sweep (err=%v)", base, err)
		}
		if _, err := os.Stat(filepath.Join(base, "kept-run")); err != nil {
			t.Errorf("preserved staging dir under %s was swept: %v", base, err)
		}
	}
}
