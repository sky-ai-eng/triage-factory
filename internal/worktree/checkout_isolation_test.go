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

// sandboxingRunRoot makes rootKey's run root with the sandbox path live, so it
// carries the jail's `.claude/skills` symlink exactly as a production
// multi-mode build leaves it.
func sandboxingRunRoot(t *testing.T, rootKey string) string {
	t.Helper()
	withTestHome(t)
	sandboxingMode(t)
	root := mustRunRoot(t, rootKey)
	t.Cleanup(func() { RemoveRunRoot(rootKey) })
	assertSkillsSymlink(t, root)
	return root
}

// makeSkillsTrackingUpstream is an upstream whose repo tracks its own project
// skill at .claude/skills/x/SKILL.md and a _tfac/pinned.txt, and whose
// refs/pull/7/head is one commit past main.
func makeSkillsTrackingUpstream(t *testing.T) string {
	t.Helper()
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	work := filepath.Join(t.TempDir(), "work")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "--bare", "--initial-branch=main", upstream)
	run("init", "-b", "main", work)
	run("-C", work, "config", "user.email", "test@example.com")
	run("-C", work, "config", "user.name", "Test")
	writeUnder(t, work, filepath.Join(".claude", "skills", "x", "SKILL.md"), "the repo's own skill\n")
	writeUnder(t, work, filepath.Join(ScratchDir, "pinned.txt"), "the repo's own file\n")
	run("-C", work, "add", "-A")
	run("-C", work, "commit", "-q", "-m", "repo tracks .claude/skills and _tfac")
	run("-C", work, "remote", "add", "origin", upstream)
	run("-C", work, "push", "-q", "origin", "main")
	writeUnder(t, work, "pr.txt", "pr change\n")
	run("-C", work, "add", "-A")
	run("-C", work, "commit", "-q", "-m", "pr commit")
	run("-C", work, "push", "-q", "origin", "HEAD:refs/pull/7/head")
	return upstream
}

// assertCheckoutUntouched is the acceptance shape for "nothing of TF's lands
// in a checkout": the repo's own .claude/skills is a real tracked directory
// with its file, git reports nothing changed, and the exclude file carries no
// block of ours.
func assertCheckoutUntouched(t *testing.T, wtDir string) {
	t.Helper()
	skill := filepath.Join(wtDir, ".claude", "skills", "x", "SKILL.md")
	assertFileContent(t, skill, "the repo's own skill\n")
	if fi, err := os.Lstat(filepath.Join(wtDir, ".claude", "skills")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("%s/.claude/skills is not the repo's directory (err=%v)", wtDir, err)
	}
	if out := gitAt(t, wtDir, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("checkout is dirty after setup:\n%s", out)
	}
	exclude := strings.TrimSpace(gitAt(t, wtDir, "rev-parse", "--git-path", "info/exclude"))
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(wtDir, exclude)
	}
	if data, err := os.ReadFile(exclude); err == nil && strings.Contains(string(data), "triagefactory") {
		t.Errorf("exclude file carries a managed block:\n%s", data)
	}
}

// TestCheckoutBuilders_WriteNothingOfTFs covers both builders in both layouts:
// a self-contained clone (sandboxed) and a linked worktree (unsandboxed), each
// of a repo that tracks its own .claude/skills — which the old builders
// replaced with a symlink, so an agent's `git add -A` committed the deletion.
func TestCheckoutBuilders_WriteNothingOfTFs(t *testing.T) {
	for _, mode := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"self-contained", func(t *testing.T) {
			sandboxingMode(t)
			t.Setenv("TF_STATE_ROOT", t.TempDir())
		}},
		{"linked", func(t *testing.T) {
			paths.SetForTest(t, t.TempDir())
			runmode.SetForTest(t, runmode.ModeLocal)
		}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			withTestHome(t)
			mode.setup(t)
			upstream := makeSkillsTrackingUpstream(t)
			root := mustRunRoot(t, "isolation-run")
			t.Cleanup(func() { RemoveRunRoot("isolation-run") })

			pr, err := CreateForPRInRoot(context.Background(), testRepo("acme", "repo"), upstream, upstream, "feature", 7, "conv-a", root)
			if err != nil {
				t.Fatalf("CreateForPRInRoot: %v", err)
			}
			if pr != filepath.Join(root, "acme", "repo", "pr-7") {
				t.Errorf("PR checkout at %s, want it under the run root at acme/repo/pr-7", pr)
			}
			assertCheckoutUntouched(t, pr)

			co, err := CreateForCheckoutInRoot(context.Background(), testRepo("acme", "repo"), upstream, "main", "conv-a", root)
			if err != nil {
				t.Fatalf("CreateForCheckoutInRoot: %v", err)
			}
			assertCheckoutUntouched(t, co)

			if IsGitWorktree(root) {
				t.Errorf("run root %s is a git worktree; it must be a plain folder", root)
			}
		})
	}
}

// TestRestoreCheckout_TrackedSkillsRoundTrip: the repo's own .claude/skills is
// the agent's work like any other path in the checkout, so an edit there is
// captured and comes back from a restore exactly.
func TestRestoreCheckout_TrackedSkillsRoundTrip(t *testing.T) {
	root := sandboxingRunRoot(t, "skills-roundtrip-run")
	t.Setenv("TF_STATE_ROOT", t.TempDir())
	upstream := makeSkillsTrackingUpstream(t)

	wtDir, err := CreateForCheckoutInRoot(context.Background(), testRepo("acme", "repo"), upstream, "main", "conv-a", root)
	if err != nil {
		t.Fatalf("CreateForCheckoutInRoot: %v", err)
	}
	writeUnder(t, wtDir, filepath.Join(".claude", "skills", "x", "SKILL.md"), "edited by the agent\n")

	delta, err := CaptureWorkspaceGit(context.Background(), wtDir)
	if err != nil {
		t.Fatalf("CaptureWorkspaceGit: %v", err)
	}
	if delta == nil || !strings.Contains(string(delta.Patch), ".claude/skills/x/SKILL.md") {
		t.Fatalf("the repo's own skill edit is missing from the patch: %+v", delta)
	}
	if err := os.RemoveAll(wtDir); err != nil {
		t.Fatalf("rm checkout: %v", err)
	}
	got := restoreDelta(t, root, "skills-roundtrip-run", "acme", "repo", CheckoutRefSlug("main"), upstream, CloneAuth{}, delta, nil)
	assertFileContent(t, filepath.Join(got, ".claude", "skills", "x", "SKILL.md"), "edited by the agent\n")
	assertSkillsSymlink(t, root)
}
