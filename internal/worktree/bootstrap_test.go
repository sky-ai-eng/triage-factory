package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeTestUpstream creates a minimal bare git repository at a tempdir
// path that EnsureBareClone can use as an "origin" URL. The bare gets
// one commit on main so there's a ref to fetch.
func makeTestUpstream(t *testing.T) string {
	t.Helper()
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", upstream).CombinedOutput(); err != nil {
		t.Fatalf("git init bare: %v: %s", err, out)
	}

	// Build a commit elsewhere and push it so the bare has a ref.
	work := filepath.Join(t.TempDir(), "work")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init work: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "test@example.com"},
		{"-C", work, "config", "user.name", "Test"},
		{"-C", work, "commit", "--allow-empty", "-m", "initial"},
		{"-C", work, "remote", "add", "origin", upstream},
		{"-C", work, "push", "origin", "main"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	return upstream
}

// withTestHome points $HOME at a tempdir for the duration of the test
// so repoDir() returns paths under it instead of touching the user's
// real ~/.triagefactory. Also overrides $TMPDIR so worktrees created
// via runDir() land under a tempdir that t.TempDir() will auto-clean.
// os.TempDir() honors $TMPDIR on Unix, so this isolation works without
// any worktree-package changes.
func withTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	return home
}

func TestEnsureBareClone_Idempotent(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	for i := 0; i < 3; i++ {
		if _, err := EnsureBareClone(context.Background(), "owner2", "repo2", upstream); err != nil {
			t.Fatalf("EnsureBareClone iteration %d: %v", i, err)
		}
	}

	bareDir, _ := repoDir("owner2", "repo2")
	if _, err := os.Stat(bareDir); err != nil {
		t.Fatalf("bare dir missing after repeated EnsureBareClone: %v", err)
	}
}

func TestEnsureBareClone_RepairsOriginURL(t *testing.T) {
	withTestHome(t)
	upstream1 := makeTestUpstream(t)
	upstream2 := makeTestUpstream(t)

	bareDir, err := EnsureBareClone(context.Background(), "owner3", "repo3", upstream1)
	if err != nil {
		t.Fatalf("first EnsureBareClone: %v", err)
	}
	out, err := exec.Command("git", "-C", bareDir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		t.Fatalf("read origin url: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != upstream1 {
		t.Fatalf("setup: expected origin %q, got %q", upstream1, got)
	}

	if _, err := EnsureBareClone(context.Background(), "owner3", "repo3", upstream2); err != nil {
		t.Fatalf("second EnsureBareClone: %v", err)
	}
	out, err = exec.Command("git", "-C", bareDir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		t.Fatalf("read origin url after repair: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != upstream2 {
		t.Errorf("expected origin repaired to %q, got %q", upstream2, got)
	}
}

func TestBootstrapBareClones_SkipsEmptyCloneURL(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	BootstrapBareClones(context.Background(), []BootstrapTarget{
		{Owner: "with", Repo: "url", CloneURL: upstream},
		{Owner: "without", Repo: "url", CloneURL: ""},
	})

	withDir, _ := repoDir("with", "url")
	if _, err := os.Stat(withDir); err != nil {
		t.Errorf("expected bare for non-empty URL: %v", err)
	}
	withoutDir, _ := repoDir("without", "url")
	if _, err := os.Stat(withoutDir); !os.IsNotExist(err) {
		t.Errorf("expected no bare for empty URL, got err=%v", err)
	}
}

func TestBootstrapBareClones_EmptyTargets(t *testing.T) {
	BootstrapBareClones(context.Background(), nil)
	BootstrapBareClones(context.Background(), []BootstrapTarget{})
}

// TestCreateForPR_ForkPR is the regression test for the full fork-PR
// flow: fetch via refs/pull/<n>/head, check out under a pr-<n> local
// branch, configure tracking so `git push` (no remote arg) sends
// commits to the fork's actual branch — not to upstream. The
// previous implementation either failed to fetch (fork branch
// missing on origin) or, after the fetch fix, pushed commits to
// upstream where they'd create a stray branch instead of updating
// the contributor's PR.
//
// Setup mirrors GitHub's real fork-PR state:
//
//   - upstream bare repo holds refs/pull/42/head (GitHub's mirror
//     of the PR head). Does NOT have refs/heads/feature-branch.
//   - fork bare repo holds refs/heads/feature-branch (the
//     contributor's actual branch). Does NOT have any pull refs.
//
// After CreateForPR + a simulated agent push:
//
//   - Fork's refs/heads/feature-branch must advance to a new commit.
//   - Upstream must be untouched (no stray branch created).
func TestCreateForPR_ForkPR(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	fork := makeTestUpstream(t) // separate bare; acts as the contributor's fork

	// Seed the fork with feature-branch and the upstream with
	// refs/pull/42/head, both pointing at the same "fork PR commit".
	work := filepath.Join(t.TempDir(), "fork-work")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "fork@example.com"},
		{"-C", work, "config", "user.name", "Forker"},
		{"-C", work, "remote", "add", "fork", fork},
		{"-C", work, "remote", "add", "up", upstream},
		{"-C", work, "fetch", "up", "main"},
		{"-C", work, "checkout", "-b", "feature-branch", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "fork PR commit"},
		{"-C", work, "push", "fork", "feature-branch:refs/heads/feature-branch"},
		{"-C", work, "push", "up", "HEAD:refs/pull/42/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	out, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse fork PR commit: %v", err)
	}
	originalForkCommit := strings.TrimSpace(string(out))

	// Sanity: upstream lacks the head branch.
	if out, err := exec.Command("git", "-C", upstream, "show-ref", "--verify", "refs/heads/feature-branch").CombinedOutput(); err == nil {
		t.Fatalf("test setup: upstream unexpectedly has refs/heads/feature-branch: %s", out)
	}

	wtPath, err := CreateForPR(context.Background(), "owner-fork-test", "repo-fork-test", upstream, fork, "feature-branch", 42, "fork-pr-test-run")
	if err != nil {
		t.Fatalf("CreateForPR for fork PR: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtPath, "fork-pr-test-run") })

	// Worktree HEAD must point at the PR's head commit.
	out, err = exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != originalForkCommit {
		t.Errorf("worktree HEAD = %q, want %q", got, originalForkCommit)
	}

	// For fork PRs the local branch is namespaced under
	// triagefactory/pr-<n>. The slash-prefix puts it out of reach
	// of any literal contributor branch name (e.g. one called pr-42)
	// that could otherwise share refs/heads/pr-42 in the bare.
	out, err = exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse abbrev: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "triagefactory/pr-42" {
		t.Errorf("worktree branch = %q, want %q", got, "triagefactory/pr-42")
	}

	// Simulate an agent: commit a change in the worktree, push (no
	// remote argument so branch tracking takes effect), then verify
	// the FORK's feature-branch advanced — and the UPSTREAM stayed
	// put. This is the actual user-visible win of the fork-tracking
	// configuration.
	agentCmds := [][]string{
		{"-C", wtPath, "config", "user.email", "agent@example.com"},
		{"-C", wtPath, "config", "user.name", "Agent"},
		{"-C", wtPath, "commit", "--allow-empty", "-m", "agent fix"},
		{"-C", wtPath, "push"},
	}
	for _, c := range agentCmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}

	// Fork's feature-branch must now point at a commit different from
	// the original (the agent's empty commit on top).
	out, err = exec.Command("git", "-C", fork, "rev-parse", "refs/heads/feature-branch").Output()
	if err != nil {
		t.Fatalf("rev-parse fork feature-branch after push: %v", err)
	}
	newForkTip := strings.TrimSpace(string(out))
	if newForkTip == originalForkCommit {
		t.Errorf("fork's feature-branch did not advance after agent push (still %q) — push went somewhere else", newForkTip)
	}

	// Upstream must NOT have grown a stray branch. Pre-tracking-fix,
	// the agent's push would have created refs/heads/triagefactory/pr-42
	// on upstream; this assertion catches a regression of that bug.
	if out, err := exec.Command("git", "-C", upstream, "show-ref", "--verify", "refs/heads/triagefactory/pr-42").CombinedOutput(); err == nil {
		t.Errorf("upstream gained a stray refs/heads/triagefactory/pr-42 branch — push leaked: %s", out)
	}
	if out, err := exec.Command("git", "-C", upstream, "show-ref", "--verify", "refs/heads/feature-branch").CombinedOutput(); err == nil {
		t.Errorf("upstream gained a stray refs/heads/feature-branch branch — push leaked: %s", out)
	}
}

// TestCleanupPRConfig_RemovesForkPRArtifacts is the regression test
// for the bare-repo accumulation bug: every fork PR delegation adds
// a head-<n> remote, branch.triagefactory/pr-<n>.* config block, and
// refs/heads/triagefactory/pr-<n> branch to the shared bare. Without
// CleanupPRConfig, repeated PR runs leak these into the config file
// indefinitely. After CleanupPRConfig, all three artifacts must be
// gone — and the bare must still be functional (no collateral
// damage to origin or other config).
func TestCleanupPRConfig_RemovesForkPRArtifacts(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	fork := makeTestUpstream(t)

	work := filepath.Join(t.TempDir(), "fork-work")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "fork@example.com"},
		{"-C", work, "config", "user.name", "Forker"},
		{"-C", work, "remote", "add", "fork", fork},
		{"-C", work, "remote", "add", "up", upstream},
		{"-C", work, "fetch", "up", "main"},
		{"-C", work, "checkout", "-b", "feature", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "fork PR commit"},
		{"-C", work, "push", "fork", "feature:refs/heads/feature"},
		{"-C", work, "push", "up", "HEAD:refs/pull/99/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}

	wtPath, err := CreateForPR(context.Background(), "owner-cleanup-test", "repo-cleanup-test", upstream, fork, "feature", 99, "cleanup-test-run")
	if err != nil {
		t.Fatalf("CreateForPR: %v", err)
	}
	bareDir, _ := repoDir("owner-cleanup-test", "repo-cleanup-test")

	// Sanity-check the artifacts are present before cleanup.
	if out, err := exec.Command("git", "-C", bareDir, "remote").Output(); err != nil || !strings.Contains(string(out), "head-99") {
		t.Fatalf("setup: head-99 remote missing pre-cleanup: %v / %s", err, out)
	}
	if out, err := exec.Command("git", "-C", bareDir, "show-ref", "--verify", "refs/heads/triagefactory/pr-99").CombinedOutput(); err != nil {
		t.Fatalf("setup: triagefactory/pr-99 branch missing pre-cleanup: %v / %s", err, out)
	}
	if out, err := exec.Command("git", "-C", bareDir, "config", "--get", "branch.triagefactory/pr-99.remote").Output(); err != nil || strings.TrimSpace(string(out)) != "head-99" {
		t.Fatalf("setup: branch tracking missing pre-cleanup: %v / %s", err, out)
	}

	// Cleanup runs after the worktree is removed — same ordering as
	// the spawner's run-finalization defer.
	if err := RemoveAt(wtPath, "cleanup-test-run"); err != nil {
		t.Fatalf("RemoveAt: %v", err)
	}
	CleanupPRConfig("owner-cleanup-test", "repo-cleanup-test", "feature", 99)

	if out, err := exec.Command("git", "-C", bareDir, "remote").Output(); err != nil || strings.Contains(string(out), "head-99") {
		t.Errorf("head-99 remote still present after cleanup: %s", out)
	}
	if out, err := exec.Command("git", "-C", bareDir, "show-ref", "--verify", "refs/heads/triagefactory/pr-99").CombinedOutput(); err == nil {
		t.Errorf("triagefactory/pr-99 branch still present after cleanup: %s", out)
	}
	if _, err := exec.Command("git", "-C", bareDir, "config", "--get", "branch.triagefactory/pr-99.remote").Output(); err == nil {
		t.Errorf("branch tracking config still present after cleanup")
	}

	// Origin must still be intact — cleanup must not touch unrelated
	// config.
	if out, err := exec.Command("git", "-C", bareDir, "config", "--get", "remote.origin.url").Output(); err != nil || strings.TrimSpace(string(out)) != upstream {
		t.Errorf("origin URL damaged by cleanup: %v / %s", err, out)
	}
}

// TestCleanupPRConfig_OwnRepoIsNoOp confirms that calling cleanup on
// an own-repo PR (where head-<n> and triagefactory/pr-<n> never get
// created in the first place) is silent. The spawner calls cleanup
// unconditionally for every PR run, so the no-op path must not log
// errors or fail.
func TestCleanupPRConfig_OwnRepoIsNoOp(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	if _, err := EnsureBareClone(context.Background(), "owner-noop-test", "repo-noop-test", upstream); err != nil {
		t.Fatalf("EnsureBareClone: %v", err)
	}

	// Cleanup for a PR that was never set up. Should not panic, fail,
	// or affect the bare.
	CleanupPRConfig("owner-noop-test", "repo-noop-test", "", 12345)

	bareDir, _ := repoDir("owner-noop-test", "repo-noop-test")
	if out, err := exec.Command("git", "-C", bareDir, "config", "--get", "remote.origin.url").Output(); err != nil || strings.TrimSpace(string(out)) != upstream {
		t.Errorf("origin URL damaged by no-op cleanup: %v / %s", err, out)
	}
}

// TestCreateForPR_DeletedFork_NoTrackingConfigured covers the
// deleted-fork PR edge case: head.repo is null, so headCloneURL is
// empty and isFork is false — but the PR is also not own-repo, and
// configuring origin as the push target would silently send commits
// to the upstream's refs/heads/<headBranch> when the agent runs
// `git push`. The right behavior is to skip tracking entirely so
// `git push` (no args) errors with "no upstream branch" instead of
// pushing to the wrong place.
func TestCreateForPR_DeletedFork_NoTrackingConfigured(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	// Stand up refs/pull/55/head on upstream (PR exists, head.repo is
	// null in the API response).
	work := filepath.Join(t.TempDir(), "deleted-work")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "x@y.z"},
		{"-C", work, "config", "user.name", "x"},
		{"-C", work, "remote", "add", "up", upstream},
		{"-C", work, "fetch", "up", "main"},
		{"-C", work, "checkout", "-b", "feature", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "deleted-fork PR commit"},
		{"-C", work, "push", "up", "HEAD:refs/pull/55/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}

	// Empty headCloneURL — the spawner passes pr.CloneURL which is
	// empty when GitHub returned head.repo = null.
	wtPath, err := CreateForPR(context.Background(), "owner-deleted-test", "repo-deleted-test", upstream, "", "feature", 55, "deleted-test-run")
	if err != nil {
		t.Fatalf("CreateForPR for deleted-fork PR: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtPath, "deleted-test-run") })

	// No branch tracking should be configured — push must fail.
	if _, err := exec.Command("git", "-C", wtPath, "config", "--get", "branch.feature.remote").Output(); err == nil {
		t.Errorf("branch.feature.remote configured for deleted-fork PR; push would silently target upstream")
	}
	// Sanity: `git push` (no args) should fail loudly.
	cmd := exec.Command("git", "-C", wtPath, "config", "user.email", "x@y.z")
	_ = cmd.Run()
	cmd = exec.Command("git", "-C", wtPath, "config", "user.name", "x")
	_ = cmd.Run()
	cmd = exec.Command("git", "-C", wtPath, "commit", "--allow-empty", "-m", "agent fix")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit in deleted-fork worktree: %v: %s", err, out)
	}
	pushCmd := exec.Command("git", "-C", wtPath, "push")
	if out, err := pushCmd.CombinedOutput(); err == nil {
		t.Errorf("git push succeeded for deleted-fork PR with no tracking — should have failed: %s", out)
	}
}

// TestCreateForPR_DeletedFork_PushFailsAfterPriorOwnRepoPR locks
// down the regression Copilot's repo-wide-settings approach
// introduced: once any own-repo PR ran on a bare, the bare carried
// remote.pushDefault=origin and push.default=current. A later
// deleted-fork PR (which deliberately skips tracking so push fails
// loudly) would silently resolve `git push` against those inherited
// repo-wide settings and create a stray branch on upstream.
//
// Switching back to per-branch tracking fixes it: the own-repo PR's
// config lives in branch.<headBranch>.* and doesn't leak to other
// branches. This test runs both PRs against the same bare and
// asserts the deleted-fork push still fails.
func TestCreateForPR_DeletedFork_PushFailsAfterPriorOwnRepoPR(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	// First, run an own-repo PR end-to-end so any bare-wide config
	// gets written.
	work := filepath.Join(t.TempDir(), "own-work")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "x@y.z"},
		{"-C", work, "config", "user.name", "x"},
		{"-C", work, "remote", "add", "up", upstream},
		{"-C", work, "fetch", "up", "main"},
		{"-C", work, "checkout", "-b", "real-feature", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "own-repo PR commit"},
		{"-C", work, "push", "up", "real-feature:refs/heads/real-feature"},
		{"-C", work, "push", "up", "real-feature:refs/pull/100/head"},
		{"-C", work, "commit", "--allow-empty", "-m", "deleted-fork PR commit"},
		{"-C", work, "push", "up", "HEAD:refs/pull/200/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}

	ownWtPath, err := CreateForPR(context.Background(), "owner-cross-test", "repo-cross-test", upstream, upstream, "real-feature", 100, "own-cross-run")
	if err != nil {
		t.Fatalf("CreateForPR own-repo: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(ownWtPath, "own-cross-run") })

	// Now stand up a deleted-fork PR (head URL empty) on the SAME bare.
	deletedWtPath, err := CreateForPR(context.Background(), "owner-cross-test", "repo-cross-test", upstream, "", "deleted-feature", 200, "deleted-cross-run")
	if err != nil {
		t.Fatalf("CreateForPR deleted-fork: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(deletedWtPath, "deleted-cross-run") })

	// Per-branch tracking means the own-repo PR's config sits in
	// branch.real-feature.* and does NOT bleed into the deleted-fork
	// worktree. Verify by attempting `git push` and asserting it
	// errors with "no upstream branch". Pre-fix this would silently
	// push to upstream:refs/heads/deleted-feature.
	for _, c := range [][]string{
		{"-C", deletedWtPath, "config", "user.email", "agent@example.com"},
		{"-C", deletedWtPath, "config", "user.name", "Agent"},
		{"-C", deletedWtPath, "commit", "--allow-empty", "-m", "agent fix"},
	} {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	pushCmd := exec.Command("git", "-C", deletedWtPath, "push")
	if out, err := pushCmd.CombinedOutput(); err == nil {
		t.Errorf("git push succeeded for deleted-fork PR after prior own-repo run — should have failed: %s", out)
	}
	// Upstream must NOT have grown a stray deleted-feature branch.
	if out, err := exec.Command("git", "-C", upstream, "show-ref", "--verify", "refs/heads/deleted-feature").CombinedOutput(); err == nil {
		t.Errorf("upstream gained a stray refs/heads/deleted-feature branch — push leaked: %s", out)
	}
}

// TestSweepStaleForkPRConfig_ReclaimsOwnRepoPR is the regression for
// the own-repo leak: when a run's inline cleanup doesn't fire (e.g. a
// cancel above the runAgent defer, or a crash), the bare keeps the
// branch.<headRef>.* tracking config. Once the worktree dir is gone,
// the sweep must reclaim branch.<headRef>.* — which the older sweep
// that walked head-<n> remotes would have missed, since own-repo PRs
// don't have one.
func TestSweepStaleForkPRConfig_ReclaimsOwnRepoPR(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	work := filepath.Join(t.TempDir(), "own-work")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "x@y.z"},
		{"-C", work, "config", "user.name", "x"},
		{"-C", work, "remote", "add", "up", upstream},
		{"-C", work, "fetch", "up", "main"},
		{"-C", work, "checkout", "-b", "stale-feature", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "stale tip"},
		{"-C", work, "push", "up", "stale-feature:refs/heads/stale-feature"},
		{"-C", work, "push", "up", "stale-feature:refs/pull/300/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}

	wtPath, err := CreateForPR(context.Background(), "owner-takeover-test", "repo-takeover-test", upstream, upstream, "stale-feature", 300, "takeover-test-run")
	if err != nil {
		t.Fatalf("CreateForPR: %v", err)
	}

	// Simulate a run whose inline cleanup never ran: the worktree dir
	// gets removed, but the per-PR config cleanup didn't fire. Just
	// remove the worktree.
	if err := RemoveAt(wtPath, "takeover-test-run"); err != nil {
		t.Fatalf("RemoveAt: %v", err)
	}

	bareDir, _ := repoDir("owner-takeover-test", "repo-takeover-test")
	// Sanity: tracking and ref are still in the bare pre-sweep.
	if _, err := exec.Command("git", "-C", bareDir, "config", "--get", "branch.stale-feature.remote").Output(); err != nil {
		t.Fatalf("setup: branch tracking missing pre-sweep: %v", err)
	}
	if out, err := exec.Command("git", "-C", bareDir, "show-ref", "--verify", "refs/heads/stale-feature").CombinedOutput(); err != nil {
		t.Fatalf("setup: stale-feature branch missing pre-sweep: %v / %s", err, out)
	}

	SweepStaleForkPRConfig("owner-takeover-test", "repo-takeover-test")

	if _, err := exec.Command("git", "-C", bareDir, "config", "--get", "branch.stale-feature.remote").Output(); err == nil {
		t.Errorf("branch.stale-feature.remote tracking survived sweep")
	}
	if out, err := exec.Command("git", "-C", bareDir, "show-ref", "--verify", "refs/heads/stale-feature").CombinedOutput(); err == nil {
		t.Errorf("refs/heads/stale-feature ref survived sweep — would poison future Jira delegations: %s", out)
	}
}

// TestCreateForPR_DeletedFork_CleanupRemovesStaleHeadBranch locks
// down issue #2: deleted-fork PRs fetch into refs/heads/<headBranch>
// but inline cleanup historically only deleted the synthetic
// triagefactory/pr-<n> ref. The leftover refs/heads/<headBranch>
// would cause CreateForBranch's branchExists path to reuse the stale
// PR tip on a future Jira delegation that happened to use the same
// branch name.
func TestCreateForPR_DeletedFork_CleanupRemovesStaleHeadBranch(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	work := filepath.Join(t.TempDir(), "deleted-work")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "x@y.z"},
		{"-C", work, "config", "user.name", "x"},
		{"-C", work, "remote", "add", "up", upstream},
		{"-C", work, "fetch", "up", "main"},
		{"-C", work, "checkout", "-b", "feature/SKY-X", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "deleted-fork PR commit"},
		{"-C", work, "push", "up", "HEAD:refs/pull/400/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}

	wtPath, err := CreateForPR(context.Background(), "owner-stale-test", "repo-stale-test", upstream, "", "feature/SKY-X", 400, "stale-test-run")
	if err != nil {
		t.Fatalf("CreateForPR: %v", err)
	}
	bareDir, _ := repoDir("owner-stale-test", "repo-stale-test")
	if out, err := exec.Command("git", "-C", bareDir, "show-ref", "--verify", "refs/heads/feature/SKY-X").CombinedOutput(); err != nil {
		t.Fatalf("setup: feature/SKY-X branch missing pre-cleanup: %v / %s", err, out)
	}

	if err := RemoveAt(wtPath, "stale-test-run"); err != nil {
		t.Fatalf("RemoveAt: %v", err)
	}
	CleanupPRConfig("owner-stale-test", "repo-stale-test", "feature/SKY-X", 400)

	if out, err := exec.Command("git", "-C", bareDir, "show-ref", "--verify", "refs/heads/feature/SKY-X").CombinedOutput(); err == nil {
		t.Errorf("refs/heads/feature/SKY-X survived cleanup — would poison future Jira delegations: %s", out)
	}
}

// TestSweepStaleForkPRConfig_PreservesUserAddedRemotes locks down
// the exact-match check: a user-added remote named like `head-42-mine`
// would parse as head-<42> if Sscanf allowed trailing input. The
// sweep must reject any remote whose canonical name doesn't equal
// "head-<n>" exactly.
func TestSweepStaleForkPRConfig_PreservesUserAddedRemotes(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "owner-strict-test", "repo-strict-test", upstream); err != nil {
		t.Fatalf("EnsureBareClone: %v", err)
	}
	bareDir, _ := repoDir("owner-strict-test", "repo-strict-test")

	// Two user-added remotes that prefix-match but aren't ours.
	for _, name := range []string{"head-42-mine", "head-100x"} {
		if out, err := exec.Command("git", "-C", bareDir, "remote", "add", name, upstream).CombinedOutput(); err != nil {
			t.Fatalf("add %s: %v: %s", name, err, out)
		}
	}

	SweepStaleForkPRConfig("owner-strict-test", "repo-strict-test")

	out, err := exec.Command("git", "-C", bareDir, "remote").Output()
	if err != nil {
		t.Fatalf("list remotes: %v", err)
	}
	for _, name := range []string{"head-42-mine", "head-100x"} {
		if !strings.Contains(string(out), name) {
			t.Errorf("user-added remote %q removed by sweep; remotes: %s", name, out)
		}
	}
}

// TestSweepStaleForkPRConfig_RemovesOrphanedRemotes is the regression
// test for the cancelled/taken-over leak path: when inline cleanup
// in the runAgent defer doesn't fire, head-<n> remotes accumulate
// in the bare. The sweep is the bootstrap-time backstop that walks
// the bare's remotes and removes any whose synthetic branch isn't
// held by a live worktree.
//
// Test setup: configure two fork-PR-like config blocks (head-50 and
// head-51) and a stray non-PR remote (extra), without ever creating
// a worktree. With no live worktrees on triagefactory/pr-50 or
// triagefactory/pr-51, the sweep must remove both PR remotes and
// preserve everything else.
func TestSweepStaleForkPRConfig_RemovesOrphanedRemotes(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "owner-sweep-test", "repo-sweep-test", upstream); err != nil {
		t.Fatalf("EnsureBareClone: %v", err)
	}
	bareDir, _ := repoDir("owner-sweep-test", "repo-sweep-test")

	// Stand up the per-PR config that fork-PR setup would have
	// produced for two PRs that are now orphaned (no worktree).
	// The trackedBranchMarkerKey is what makes the sweep find these
	// generically — without it, own-repo PRs would be invisible to
	// the sweep too.
	for _, n := range []int{50, 51} {
		remote := fmt.Sprintf("head-%d", n)
		branch := fmt.Sprintf("triagefactory/pr-%d", n)
		setup := [][]string{
			{"-C", bareDir, "remote", "add", remote, upstream},
			{"-C", bareDir, "fetch", remote, "main:" + branch},
			{"-C", bareDir, "config", fmt.Sprintf("branch.%s.remote", branch), remote},
			{"-C", bareDir, "config", fmt.Sprintf("branch.%s.merge", branch), "refs/heads/main"},
			{"-C", bareDir, "config", fmt.Sprintf("branch.%s.%s", branch, trackedBranchMarkerKey), fmt.Sprintf("%d", n)},
		}
		for _, c := range setup {
			if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", c, err, out)
			}
		}
	}
	// A stray non-PR remote that the sweep must not touch.
	if out, err := exec.Command("git", "-C", bareDir, "remote", "add", "extra", upstream).CombinedOutput(); err != nil {
		t.Fatalf("add extra remote: %v: %s", err, out)
	}

	SweepStaleForkPRConfig("owner-sweep-test", "repo-sweep-test")

	remotesOut, err := exec.Command("git", "-C", bareDir, "remote").Output()
	if err != nil {
		t.Fatalf("list remotes: %v", err)
	}
	remotes := strings.Fields(string(remotesOut))
	for _, r := range remotes {
		if r == "head-50" || r == "head-51" {
			t.Errorf("orphan remote %q survived sweep", r)
		}
	}
	hasOrigin, hasExtra := false, false
	for _, r := range remotes {
		if r == "origin" {
			hasOrigin = true
		}
		if r == "extra" {
			hasExtra = true
		}
	}
	if !hasOrigin {
		t.Errorf("origin removed by sweep")
	}
	if !hasExtra {
		t.Errorf("non-PR remote 'extra' removed by sweep")
	}
	for _, n := range []int{50, 51} {
		if _, err := exec.Command("git", "-C", bareDir, "config", "--get", fmt.Sprintf("branch.triagefactory/pr-%d.remote", n)).Output(); err == nil {
			t.Errorf("orphan branch tracking config for pr-%d survived sweep", n)
		}
	}
}

// TestSweepStaleForkPRConfig_PreservesLiveWorktree is the safety
// regression: the sweep must NOT remove a head-<n> remote whose
// synthetic branch is checked out by a live worktree (a delegated run
// is still using it for push/pull). With the worktree present, both
// the remote and the branch tracking config must survive the sweep.
func TestSweepStaleForkPRConfig_PreservesLiveWorktree(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	fork := makeTestUpstream(t)

	work := filepath.Join(t.TempDir(), "fork-work")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "fork@example.com"},
		{"-C", work, "config", "user.name", "Forker"},
		{"-C", work, "remote", "add", "fork", fork},
		{"-C", work, "remote", "add", "up", upstream},
		{"-C", work, "fetch", "up", "main"},
		{"-C", work, "checkout", "-b", "feature", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "fork PR commit"},
		{"-C", work, "push", "fork", "feature:refs/heads/feature"},
		{"-C", work, "push", "up", "HEAD:refs/pull/77/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}

	wtPath, err := CreateForPR(context.Background(), "owner-live-test", "repo-live-test", upstream, fork, "feature", 77, "live-test-run")
	if err != nil {
		t.Fatalf("CreateForPR: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtPath, "live-test-run") })

	// Sweep with the worktree still present. head-77 is in use, so
	// the sweep must skip it.
	SweepStaleForkPRConfig("owner-live-test", "repo-live-test")

	bareDir, _ := repoDir("owner-live-test", "repo-live-test")
	out, err := exec.Command("git", "-C", bareDir, "remote").Output()
	if err != nil {
		t.Fatalf("list remotes: %v", err)
	}
	if !strings.Contains(string(out), "head-77") {
		t.Errorf("head-77 remote removed by sweep while worktree live: %s", out)
	}
	if _, err := exec.Command("git", "-C", bareDir, "config", "--get", "branch.triagefactory/pr-77.remote").Output(); err != nil {
		t.Errorf("branch tracking config for live PR removed by sweep: %v", err)
	}
}

// TestCleanupPRConfig_RunsAfterContextCancellation locks down the
// detached-context behavior: the spawner's runAgent defer can fire
// after the agent's parent ctx has been cancelled (run timeout,
// server shutdown). Pre-fix, gitRunCtx with a cancelled ctx would
// short-circuit every cleanup command and leak the per-PR config.
// CleanupPRConfig now uses an internal context.Background() so
// cleanup runs regardless of caller-side cancellation.
func TestCleanupPRConfig_RunsAfterContextCancellation(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	fork := makeTestUpstream(t)

	work := filepath.Join(t.TempDir(), "fork-work")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "fork@example.com"},
		{"-C", work, "config", "user.name", "Forker"},
		{"-C", work, "remote", "add", "fork", fork},
		{"-C", work, "remote", "add", "up", upstream},
		{"-C", work, "fetch", "up", "main"},
		{"-C", work, "checkout", "-b", "feature", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "fork PR commit"},
		{"-C", work, "push", "fork", "feature:refs/heads/feature"},
		{"-C", work, "push", "up", "HEAD:refs/pull/123/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}

	wtPath, err := CreateForPR(context.Background(), "owner-cancel-test", "repo-cancel-test", upstream, fork, "feature", 123, "cancel-test-run")
	if err != nil {
		t.Fatalf("CreateForPR: %v", err)
	}
	if err := RemoveAt(wtPath, "cancel-test-run"); err != nil {
		t.Fatalf("RemoveAt: %v", err)
	}

	// CleanupPRConfig takes no ctx — it constructs its own internal
	// background ctx. Even if the caller's ctx is fully cancelled,
	// cleanup must still run.
	CleanupPRConfig("owner-cancel-test", "repo-cancel-test", "feature", 123)

	bareDir, _ := repoDir("owner-cancel-test", "repo-cancel-test")
	if out, err := exec.Command("git", "-C", bareDir, "remote").Output(); err != nil || strings.Contains(string(out), "head-123") {
		t.Errorf("head-123 remote not cleaned up despite detached context: %v / %s", err, out)
	}
}

// TestCreateForPR_OwnRepoPR_FetchesViaPullRef confirms the
// refs/pull/<n>/head fetch path is also correct for the common case
// where the PR is from a branch on the upstream itself. GitHub
// maintains refs/pull/<n>/head for every PR regardless of fork
// status, so the same code path should work uniformly.
func TestCreateForPR_OwnRepoPR_FetchesViaPullRef(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	// Push a feature branch directly to upstream AND mirror it as
	// refs/pull/7/head — the state GitHub sets up for an own-repo PR.
	work := filepath.Join(t.TempDir(), "work-own")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init work: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "me@example.com"},
		{"-C", work, "config", "user.name", "Me"},
		{"-C", work, "remote", "add", "origin", upstream},
		{"-C", work, "fetch", "origin", "main"},
		{"-C", work, "checkout", "-b", "my-feature", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "own-repo PR commit"},
		{"-C", work, "push", "origin", "my-feature:refs/heads/my-feature"},
		{"-C", work, "push", "origin", "my-feature:refs/pull/7/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	out, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse work HEAD: %v", err)
	}
	expected := strings.TrimSpace(string(out))

	// Own-repo PRs pass the same URL as both upstream and head — the
	// PR's head.repo and base.repo are the same repo. CreateForPR
	// detects this and skips the fork-tracking configuration.
	wtPath, err := CreateForPR(context.Background(), "owner-own-test", "repo-own-test", upstream, upstream, "my-feature", 7, "own-pr-test-run")
	if err != nil {
		t.Fatalf("CreateForPR for own-repo PR: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtPath, "own-pr-test-run") })

	out, err = exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != expected {
		t.Errorf("worktree HEAD = %q, want %q", got, expected)
	}
	out, err = exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse abbrev: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "my-feature" {
		t.Errorf("worktree branch = %q, want %q (git push relies on attached branch, not detached HEAD)", got, "my-feature")
	}

	// Tracking is preconfigured so `git push` (no remote argument)
	// works without -u. Envelope guidance tells agents to use this
	// form; verify it actually lands on upstream's my-feature.
	agentCmds := [][]string{
		{"-C", wtPath, "config", "user.email", "agent@example.com"},
		{"-C", wtPath, "config", "user.name", "Agent"},
		{"-C", wtPath, "commit", "--allow-empty", "-m", "agent fix"},
		{"-C", wtPath, "push"},
	}
	for _, c := range agentCmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	out, err = exec.Command("git", "-C", upstream, "rev-parse", "refs/heads/my-feature").Output()
	if err != nil {
		t.Fatalf("rev-parse upstream my-feature after push: %v", err)
	}
	if newTip := strings.TrimSpace(string(out)); newTip == expected {
		t.Errorf("upstream's my-feature did not advance after agent push (still %q) — tracking didn't take effect", newTip)
	}
}

// addLockedWorktree creates a real worktree on `branch` at the full path
// `checkout` and locks it with `reason` (git's `worktree lock --reason`).
// Returns the checkout path. The caller controls the location (TF's
// ephemeral runs namespace via tfRunCheckout vs. an arbitrary
// user/removable path) and whether to delete the checkout afterward.
func addLockedWorktree(t *testing.T, bareDir, branch, checkout, reason string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(checkout), 0o755); err != nil {
		t.Fatalf("mkdir checkout parent: %v", err)
	}
	cmds := [][]string{
		{"-C", bareDir, "worktree", "add", checkout, branch},
		{"-C", bareDir, "worktree", "lock", "--reason", reason, checkout},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	return checkout
}

// makeGhost is addLockedWorktree + deleting the checkout: the exact
// on-disk state of a cancel/kill mid-`git worktree add` — admin
// registration present and locked, working tree gone. `git worktree
// prune` skips it because it's locked, pinning the branch.
func makeGhost(t *testing.T, bareDir, branch, checkout, reason string) string {
	t.Helper()
	addLockedWorktree(t, bareDir, branch, checkout, reason)
	if err := os.RemoveAll(checkout); err != nil {
		t.Fatalf("remove ghost checkout: %v", err)
	}
	return checkout
}

// tfRunCheckout returns a checkout path inside TF's ephemeral run
// namespace (runDir = <TMPDIR>/triagefactory-runs/<id>) — the only paths
// the startup ghost sweep is allowed to reclaim.
func tfRunCheckout(id string) string {
	return runDir(id)
}

func assertReAddWedged(t *testing.T, bareDir, branch string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", bareDir, "worktree", "prune").CombinedOutput(); err != nil {
		t.Fatalf("worktree prune: %v: %s", err, out)
	}
	dest := filepath.Join(t.TempDir(), "wedged")
	if out, err := exec.Command("git", "-C", bareDir, "worktree", "add", dest, branch).CombinedOutput(); err == nil {
		t.Fatalf("expected re-add of %s to be wedged by the ghost, but it succeeded:\n%s", branch, out)
	}
}

func assertReAddSucceeds(t *testing.T, bareDir, branch string) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "re-add")
	if out, err := exec.Command("git", "-C", bareDir, "worktree", "add", dest, branch).CombinedOutput(); err != nil {
		t.Fatalf("re-add of %s should succeed after clearing the ghost: %v:\n%s", branch, err, out)
	}
}

// TestRemoveWorktreeRegFor_UnwedgesAndIsolated is the core regression for
// the in-process reclaim (CreateForPR's add-failure path): a half-built
// add that plain prune can't reclaim pins its branch and blocks the next
// run for the same PR. removeWorktreeRegFor must drop exactly that run's
// registration — and, crucially, ONLY that one, so it's safe to call
// without lockRepo while a concurrent add is in flight against the same
// bare.
func TestRemoveWorktreeRegFor_UnwedgesAndIsolated(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	bareDir, err := EnsureBareClone(context.Background(), "owner", "repo", upstream)
	if err != nil {
		t.Fatalf("EnsureBareClone: %v", err)
	}
	// A second branch for the "concurrent" live worktree that must survive.
	if out, err := exec.Command("git", "-C", bareDir, "branch", "feature", "main").CombinedOutput(); err != nil {
		t.Fatalf("git branch feature: %v: %s", err, out)
	}

	ghost := makeGhost(t, bareDir, "main", tfRunCheckout("run-ghost"), "initializing")
	live := addLockedWorktree(t, bareDir, "feature", tfRunCheckout("run-live"), "initializing")

	assertReAddWedged(t, bareDir, "main")

	removeWorktreeRegFor(bareDir, ghost)

	// The ghost's branch is reclaimable; the concurrent live worktree's
	// registration is untouched.
	assertReAddSucceeds(t, bareDir, "main")
	if _, err := os.Stat(filepath.Join(bareDir, "worktrees", filepath.Base(live))); err != nil {
		t.Fatalf("removeWorktreeRegFor must not touch the concurrent worktree %q: %v", filepath.Base(live), err)
	}
}

// TestClearStaleLockedWorktrees_ReclaimsLocalizedGhost is the startup
// backstop's regression and the guard for the localized-Git case: git
// writes the add lock reason through gettext (_("initializing")), so
// detection must not depend on the English literal. A ghost locked with a
// translated reason must still be reclaimed because its working tree is
// gone.
func TestClearStaleLockedWorktrees_ReclaimsLocalizedGhost(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	bareDir, err := EnsureBareClone(context.Background(), "owner", "repo", upstream)
	if err != nil {
		t.Fatalf("EnsureBareClone: %v", err)
	}

	makeGhost(t, bareDir, "main", tfRunCheckout("run-loc"), "réinitialisation en cours") // non-English lock reason

	assertReAddWedged(t, bareDir, "main")

	if n := clearStaleLockedWorktrees(bareDir); n != 1 {
		t.Fatalf("clearStaleLockedWorktrees cleared %d ghosts, want 1", n)
	}
	assertReAddSucceeds(t, bareDir, "main")
}

// TestClearStaleLockedWorktrees_PreservesLiveLockedWorktree guards the
// blast-radius: a worktree that's locked but whose working tree still
// exists is a real, in-use worktree and must never be swept.
func TestClearStaleLockedWorktrees_PreservesLiveLockedWorktree(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	bareDir, err := EnsureBareClone(context.Background(), "owner", "repo", upstream)
	if err != nil {
		t.Fatalf("EnsureBareClone: %v", err)
	}

	addLockedWorktree(t, bareDir, "main", tfRunCheckout("run-live"), "held by user for debugging") // checkout kept

	if n := clearStaleLockedWorktrees(bareDir); n != 0 {
		t.Fatalf("clearStaleLockedWorktrees cleared %d, want 0 (live locked worktree must survive)", n)
	}
	if matches, _ := filepath.Glob(filepath.Join(bareDir, "worktrees", "*", "locked")); len(matches) != 1 {
		t.Fatalf("expected the live locked registration to survive, found %d", len(matches))
	}
}

// TestClearStaleLockedWorktrees_PreservesUnmountedUserWorktree is the
// regression for the lock-contract fix: a worktree a user deliberately
// locked because its checkout lives on a removable/network volume that's
// currently unmounted also looks "locked + checkout-missing", but it is
// NOT under TF's runs namespace, so the sweep must leave it alone.
func TestClearStaleLockedWorktrees_PreservesUnmountedUserWorktree(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	bareDir, err := EnsureBareClone(context.Background(), "owner", "repo", upstream)
	if err != nil {
		t.Fatalf("EnsureBareClone: %v", err)
	}

	// A locked worktree at a non-TF path (stands in for /Volumes/USB/...),
	// then its checkout is removed to simulate the volume being unmounted.
	userPath := filepath.Join(t.TempDir(), "removable", "myrepo")
	makeGhost(t, bareDir, "main", userPath, "on the external SSD")

	if n := clearStaleLockedWorktrees(bareDir); n != 0 {
		t.Fatalf("clearStaleLockedWorktrees cleared %d, want 0 (user's locked off-volume worktree must survive)", n)
	}
	if matches, _ := filepath.Glob(filepath.Join(bareDir, "worktrees", "*", "locked")); len(matches) != 1 {
		t.Fatalf("expected the user's locked registration to survive, found %d", len(matches))
	}
}

// TestCleanup_SweepsBareGhostWithoutRunsDir covers the runs-dir early
// return fix: the bare-repo sweep must run at startup even when
// /tmp/triagefactory-runs is absent (e.g. /tmp wiped on reboot) — the
// precise scenario where a ghost survives in the persistent bare.
func TestCleanup_SweepsBareGhostWithoutRunsDir(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	bareDir, err := EnsureBareClone(context.Background(), "owner", "repo", upstream)
	if err != nil {
		t.Fatalf("EnsureBareClone: %v", err)
	}
	makeGhost(t, bareDir, "main", tfRunCheckout("run-ghost"), "initializing")

	// Simulate /tmp being wiped on reboot: remove the whole runs dir so it
	// is absent, while the bare still records the ghost. The admin entry's
	// gitdir keeps the (now non-existent) triagefactory-runs path string,
	// which is what the sweep keys on.
	if err := os.RemoveAll(filepath.Join(os.TempDir(), runsDir)); err != nil {
		t.Fatalf("remove runs dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(os.TempDir(), runsDir)); !os.IsNotExist(err) {
		t.Fatalf("test precondition: runs dir should be absent, stat err = %v", err)
	}

	Cleanup()

	if matches, _ := filepath.Glob(filepath.Join(bareDir, "worktrees", "*", "locked")); len(matches) != 0 {
		t.Fatalf("Cleanup left %d ghost(s) behind when runs dir was absent", len(matches))
	}
	assertReAddSucceeds(t, bareDir, "main")
}

// TestCreateForPR_SnapshotBundleIsBounded pins the refs/remotes mirror written
// at PR fetch time and its consequence. A bare clone keeps everything under
// refs/heads/*, so a repo that only ever hosted PR-review runs had ZERO
// remote-tracking refs — the workspace snapshot's `git bundle <rev> --not
// --remotes` excluded nothing and packed the repo's FULL history (minutes of
// CPU on a real repo). With the mirror recording GitHub's known tip, the
// bundle carries only the agent's local commits: it has prerequisites, so
// verifying it in an empty repo must FAIL, where a self-contained
// full-history bundle would pass.
func TestCreateForPR_SnapshotBundleIsBounded(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	work := filepath.Join(t.TempDir(), "work-bundle")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init work: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "me@example.com"},
		{"-C", work, "config", "user.name", "Me"},
		{"-C", work, "remote", "add", "origin", upstream},
		{"-C", work, "fetch", "origin", "main"},
		{"-C", work, "checkout", "-b", "bundle-feature", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "PR commit"},
		{"-C", work, "push", "origin", "bundle-feature:refs/heads/bundle-feature"},
		{"-C", work, "push", "origin", "bundle-feature:refs/pull/9/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	out, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse work HEAD: %v", err)
	}
	prTip := strings.TrimSpace(string(out))

	wtPath, err := CreateForPR(context.Background(), "owner-bundle-test", "repo-bundle-test", upstream, upstream, "bundle-feature", 9, "bundle-test-run")
	if err != nil {
		t.Fatalf("CreateForPR: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtPath, "bundle-test-run") })

	// The mirror ref records GitHub's known tip in the namespace the
	// snapshot's bundle excludes.
	bareDir, _ := repoDir("owner-bundle-test", "repo-bundle-test")
	out, err = exec.Command("git", "-C", bareDir, "rev-parse", "refs/remotes/origin/bundle-feature").Output()
	if err != nil {
		t.Fatalf("mirror ref refs/remotes/origin/bundle-feature missing from bare: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != prTip {
		t.Errorf("mirror ref = %q, want PR tip %q", got, prTip)
	}

	// Agent makes one local commit; the snapshot delta must carry only it.
	agentCmds := [][]string{
		{"-C", wtPath, "config", "user.email", "agent@example.com"},
		{"-C", wtPath, "config", "user.name", "Agent"},
		{"-C", wtPath, "commit", "--allow-empty", "-m", "agent WIP"},
	}
	for _, c := range agentCmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	delta, err := CaptureWorkspaceGit(context.Background(), wtPath)
	if err != nil {
		t.Fatalf("CaptureWorkspaceGit: %v", err)
	}
	if delta == nil || len(delta.Bundle) == 0 {
		t.Fatal("expected a non-empty bundle for a local-only commit")
	}

	bundleFile := filepath.Join(t.TempDir(), "snap.bundle")
	if err := os.WriteFile(bundleFile, delta.Bundle, 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	emptyRepo := filepath.Join(t.TempDir(), "empty")
	if out, err := exec.Command("git", "init", "-b", "main", emptyRepo).CombinedOutput(); err != nil {
		t.Fatalf("git init empty: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", emptyRepo, "bundle", "verify", bundleFile).CombinedOutput(); err == nil {
		t.Errorf("bundle verified in an empty repo — it is self-contained full history, not a bounded delta:\n%s", out)
	}
}
