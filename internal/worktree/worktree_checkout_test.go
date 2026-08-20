package worktree

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// coRun runs a git command and fails the test on error.
func coRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	if dir != "" {
		c.Dir = dir
	}
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// coOut runs a git command and returns trimmed stdout.
func coOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	if dir != "" {
		c.Dir = dir
	}
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// coPushBranch clones the upstream, creates branch with one commit, pushes it,
// and returns the branch tip SHA.
func coPushBranch(t *testing.T, upstream, branch string) string {
	t.Helper()
	work := t.TempDir()
	coRun(t, "", "clone", upstream, work)
	coRun(t, work, "config", "user.email", "t@example.com")
	coRun(t, work, "config", "user.name", "T")
	coRun(t, work, "checkout", "-b", branch)
	coRun(t, work, "commit", "--allow-empty", "-m", branch+" commit")
	coRun(t, work, "push", "origin", branch)
	return coOut(t, work, "rev-parse", "HEAD")
}

// TestCreateForCheckoutInRoot_DefaultBranchDetached pins the generalized default
// `workspace add`: the worktree lands at {runRoot}/{owner}/{repo}/default,
// checked out DETACHED at the repo's default-branch tip (no branch claimed), and
// the agent can then create its own branch — which CurrentBranch (the push
// gate's source) reports.
func TestCreateForCheckoutInRoot_DefaultBranchDetached(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t) // default branch "main", one commit
	runRoot := t.TempDir()

	wt, err := CreateForCheckoutInRoot(context.Background(), "owner", "repo", upstream, "", "run-1", runRoot)
	if err != nil {
		t.Fatalf("CreateForCheckoutInRoot: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wt, "run-1") })

	if want := filepath.Join(runRoot, "owner", "repo", "default"); wt != want {
		t.Errorf("path = %q, want %q", wt, want)
	}
	// HEAD is detached → no live branch → push gate authorizes nothing yet.
	if b := CurrentBranch(wt); b != "" {
		t.Errorf("CurrentBranch on a fresh default checkout = %q, want empty (detached)", b)
	}
	// HEAD resolves to the upstream's main tip.
	if got, want := coOut(t, wt, "rev-parse", "HEAD"), coOut(t, upstream, "rev-parse", "refs/heads/main"); got != want {
		t.Errorf("worktree HEAD = %q, want upstream main %q", got, want)
	}
	// The agent creates its own working branch → CurrentBranch reports it.
	coRun(t, wt, "checkout", "-b", "tfac/SKY-1")
	if b := CurrentBranch(wt); b != "tfac/SKY-1" {
		t.Errorf("CurrentBranch after checkout -b = %q, want tfac/SKY-1", b)
	}
}

// TestCreateForCheckoutInRoot_Ref pins `workspace add --ref <branch>`: the
// worktree lands detached at the named branch's tip (not the default branch).
func TestCreateForCheckoutInRoot_Ref(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	featureTip := coPushBranch(t, upstream, "feature-x")
	runRoot := t.TempDir()

	wt, err := CreateForCheckoutInRoot(context.Background(), "owner", "repo", upstream, "feature-x", "run-2", runRoot)
	if err != nil {
		t.Fatalf("CreateForCheckoutInRoot(--ref): %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wt, "run-2") })

	if got := coOut(t, wt, "rev-parse", "HEAD"); got != featureTip {
		t.Errorf("worktree HEAD = %q, want feature-x tip %q", got, featureTip)
	}
	if b := CurrentBranch(wt); b != "" {
		t.Errorf("CurrentBranch on a fresh --ref checkout = %q, want empty (detached)", b)
	}
}

// TestCreateForPRInRoot_OwnRepoPR pins `workspace add --pr N` for an own-repo
// PR: the worktree lands at {runRoot}/{owner}/{repo}/pr-<N> on the PR head (via
// the upstream's refs/pull/<n>/head), checked out on the per-run branch so
// CurrentBranch reports it (and the push gate authorizes it).
func TestCreateForPRInRoot_OwnRepoPR(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	// Seed refs/pull/7/head on the upstream (GitHub's server-side mirror of the
	// PR head). Own-repo PR: head clone URL == upstream.
	work := t.TempDir()
	coRun(t, "", "clone", upstream, work)
	coRun(t, work, "config", "user.email", "t@example.com")
	coRun(t, work, "config", "user.name", "T")
	coRun(t, work, "checkout", "-b", "pr-head")
	coRun(t, work, "commit", "--allow-empty", "-m", "pr commit")
	prTip := coOut(t, work, "rev-parse", "HEAD")
	coRun(t, work, "push", "origin", fmt.Sprintf("HEAD:refs/pull/%d/head", 7))

	runRoot := t.TempDir()
	wt, err := CreateForPRInRoot(context.Background(), "owner", "repo", upstream, upstream, "pr-head", 7, "run-3", runRoot)
	if err != nil {
		t.Fatalf("CreateForPRInRoot: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wt, "run-3") })

	if want := filepath.Join(runRoot, "owner", "repo", "pr-7"); wt != want {
		t.Errorf("path = %q, want %q", wt, want)
	}
	if got := coOut(t, wt, "rev-parse", "HEAD"); got != prTip {
		t.Errorf("worktree HEAD = %q, want PR head %q", got, prTip)
	}
	// The PR checks out on the run-namespaced local branch → the push gate
	// authorizes it (and it's per-run-unique so concurrent same-PR runs don't
	// collide).
	if b := CurrentBranch(wt); b != "triagefactory/run-3/pr-7" {
		t.Errorf("CurrentBranch = %q, want triagefactory/run-3/pr-7", b)
	}
}

// TestCreateForCheckoutInRoot_RequiresRunRoot pins the in-root contract guard.
func TestCreateForCheckoutInRoot_RequiresRunRoot(t *testing.T) {
	if _, err := CreateForCheckoutInRoot(context.Background(), "o", "r", "url", "", "run", ""); err == nil {
		t.Fatal("expected error when runRoot is empty")
	}
}
