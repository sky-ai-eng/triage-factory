package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// sandboxingWorktree stands up a real branch worktree with the sandbox path live,
// so the tree carries the jail's `.claude/skills` symlink exactly as a production
// multi-mode build leaves it. Returns the worktree dir and its HEAD sha.
func sandboxingWorktree(t *testing.T, runID string) (wtDir, head string) {
	t.Helper()
	withTestHome(t)
	paths.SetForTest(t, t.TempDir())
	runmode.SetForTest(t, runmode.ModeMulti)

	upstream := makeTestUpstream(t)
	wtDir, err := CreateForBranch(context.Background(), "acme", "repo", upstream, "main", "aa/feature", runID)
	if err != nil {
		t.Fatalf("CreateForBranch: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtDir, runID) })
	assertSkillsSymlink(t, wtDir)

	out, err := exec.Command("git", "-C", wtDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return wtDir, strings.TrimSpace(string(out))
}

// TestRestoreWorkspaceGit_PreFixSkillsPatchConvergesToSymlink pins the restore
// ORDERING. A snapshot taken before step skills moved out of the workspace can
// carry a real `.claude/skills` tree in its patch, so restore must apply the
// delta FIRST and plant the symlink after: planting first would leave git-apply
// writing through a symlink, which it refuses outright. Either way the rebuilt
// tree converges on the symlink, which is what the next step's mount needs.
func TestRestoreWorkspaceGit_PreFixSkillsPatchConvergesToSymlink(t *testing.T) {
	const runID = "restore-skills-run"
	wtDir, head := sandboxingWorktree(t, runID)

	// Craft the pre-fix patch shape: an uncommitted, in-worktree SKILL.md under
	// the very path restore now wants to own. Built with the worktree's own index
	// (not CaptureWorkspaceGit, which deliberately excludes this path now).
	skillFile := filepath.Join(wtDir, ".claude", "skills", "chain-step-0-legacy", "SKILL.md")
	if err := os.RemoveAll(filepath.Join(wtDir, ".claude", "skills")); err != nil {
		t.Fatalf("clear symlink: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o755); err != nil {
		t.Fatalf("mkdir legacy skill: %v", err)
	}
	if err := os.WriteFile(skillFile, []byte("---\nname: chain-step-0-legacy\n---\n\nlegacy\n"), 0o644); err != nil {
		t.Fatalf("write legacy skill: %v", err)
	}
	if out, err := exec.Command("git", "-C", wtDir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	patch, err := exec.Command("git", "-C", wtDir, "diff", "--cached", "--binary", "HEAD").Output()
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	if !strings.Contains(string(patch), ".claude/skills") {
		t.Fatalf("hand-built patch does not carry .claude/skills; the fixture is wrong:\n%s", patch)
	}

	// Host loss: the tree is gone and comes back from bare + delta.
	if err := os.RemoveAll(wtDir); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}
	delta := &GitDelta{Branch: "aa/feature", Head: head, Patch: patch}
	if err := RestoreWorkspaceGit(context.Background(), "acme", "repo", wtDir, delta, "", CloneAuth{}); err != nil {
		t.Fatalf("RestoreWorkspaceGit with a pre-fix skills patch: %v", err)
	}
	assertSkillsSymlink(t, wtDir)
}

// TestCaptureWorkspaceGit_NoSkillsPathStillCaptures is the regression guard for
// the exclusion's blast radius: capture is shared by EVERY run, and the vast
// majority of worktrees have no `.claude` at all. Excluding a path that matches
// nothing must not disturb the capture — the agent's work still lands in the
// patch, and no step of the pipeline errors out.
func TestCaptureWorkspaceGit_NoSkillsPathStillCaptures(t *testing.T) {
	withTestHome(t)
	const runID = "capture-no-skills-run"
	upstream := makeTestUpstream(t)
	wtDir, err := CreateForBranch(context.Background(), "acme", "repo", upstream, "main", "aa/feature", runID)
	if err != nil {
		t.Fatalf("CreateForBranch: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtDir, runID) })
	// Local mode (the default here): no symlink is planted, and this repo tracks
	// no .claude — the ordinary non-blueprint shape.
	if _, err := os.Lstat(filepath.Join(wtDir, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("fixture has a .claude dir; this test is about the absence of one (err=%v)", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "agent.txt"), []byte("agent edit\n"), 0o644); err != nil {
		t.Fatalf("write agent file: %v", err)
	}

	delta, err := CaptureWorkspaceGit(context.Background(), wtDir)
	if err != nil {
		t.Fatalf("CaptureWorkspaceGit on a tree with no .claude/skills: %v", err)
	}
	if delta == nil || !strings.Contains(string(delta.Patch), "agent.txt") {
		t.Errorf("agent's change missing from the patch: %+v", delta)
	}
}

// TestCaptureWorkspaceGit_ExcludesSkillsPath: `.claude/skills` is TF mechanism,
// not the agent's work, so it stays out of the delta — both the symlink a
// sandboxed tree carries and, critically, any files a repo TRACKS there, which
// must not come back from a snapshot recorded as deletions just because TF took
// the path over.
func TestCaptureWorkspaceGit_ExcludesSkillsPath(t *testing.T) {
	const runID = "capture-skills-run"
	wtDir, _ := sandboxingWorktree(t, runID)

	// A repo that commits its own project skills — the case a naive
	// `rm --cached` exclusion would record as a deletion.
	tracked := filepath.Join(wtDir, ".claude", "skills", "project-skill", "SKILL.md")
	if err := os.RemoveAll(filepath.Join(wtDir, ".claude", "skills")); err != nil {
		t.Fatalf("clear symlink: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(tracked), 0o755); err != nil {
		t.Fatalf("mkdir tracked skill: %v", err)
	}
	if err := os.WriteFile(tracked, []byte("tracked project skill\n"), 0o644); err != nil {
		t.Fatalf("write tracked skill: %v", err)
	}
	for _, args := range [][]string{
		{"-C", wtDir, "config", "user.email", "test@example.com"},
		{"-C", wtDir, "config", "user.name", "Test"},
		{"-C", wtDir, "add", "-A"},
		{"-C", wtDir, "commit", "-m", "repo tracks its own project skills"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	// TF takes the path over on the next build/restore.
	if err := EnsureSandboxSkillsLink(wtDir); err != nil {
		t.Fatalf("EnsureSandboxSkillsLink: %v", err)
	}
	// Real agent work, which MUST survive the exclusion.
	if err := os.WriteFile(filepath.Join(wtDir, "agent.txt"), []byte("agent edit\n"), 0o644); err != nil {
		t.Fatalf("write agent file: %v", err)
	}

	delta, err := CaptureWorkspaceGit(context.Background(), wtDir)
	if err != nil {
		t.Fatalf("CaptureWorkspaceGit: %v", err)
	}
	if delta == nil {
		t.Fatal("nil delta for a dirty worktree")
	}
	if strings.Contains(string(delta.Patch), ".claude/skills") {
		t.Errorf("patch carries .claude/skills — TF mechanism leaked into the snapshot:\n%s", delta.Patch)
	}
	if !strings.Contains(string(delta.Patch), "agent.txt") {
		t.Errorf("patch lost the agent's own change:\n%s", delta.Patch)
	}
}
