package worktree

import (
	"testing"
)

// ptRepo initializes a plain repo with one commit on branch "work" and returns
// its path. Enough state for PushTargetBranch's config resolution — no bare or
// upstream needed, since the function only reads HEAD and local config.
func ptRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	coRun(t, "", "init", "-b", "work", dir)
	coRun(t, dir, "config", "user.email", "t@example.com")
	coRun(t, dir, "config", "user.name", "T")
	coRun(t, dir, "commit", "--allow-empty", "-m", "seed")
	return dir
}

// TestPushTargetBranch_IdentityWithoutRefspec pins the branch-worktree case
// (TFAC-498 unchanged): no push refspec configured, so a bare `git push` from
// branch B lands on B — the gate authorizes the checkout's own name.
func TestPushTargetBranch_IdentityWithoutRefspec(t *testing.T) {
	dir := ptRepo(t)
	if got := PushTargetBranch(dir); got != "work" {
		t.Errorf("PushTargetBranch = %q, want work (identity when no refspec maps the branch)", got)
	}
}

// TestPushTargetBranch_DetachedHEAD pins that a detached checkout (the fresh
// default / --ref `workspace add` state) resolves to "" — nothing pushable
// until the agent creates a branch.
func TestPushTargetBranch_DetachedHEAD(t *testing.T) {
	dir := ptRepo(t)
	coRun(t, dir, "checkout", "--detach")
	if got := PushTargetBranch(dir); got != "" {
		t.Errorf("PushTargetBranch on detached HEAD = %q, want empty", got)
	}
}

// TestPushTargetBranch_RefspecMapsToRemoteHead pins the PR-worktree shape that
// broke the multi-mode push gate: the checkout is a run-namespaced local branch
// and configurePRPushTracking's per-run remote + explicit refspec map it to the
// PR's real head. The gate must authorize the REMOTE head (what receive-pack
// carries), not the local name.
func TestPushTargetBranch_RefspecMapsToRemoteHead(t *testing.T) {
	dir := ptRepo(t)
	coRun(t, dir, "checkout", "-b", "triagefactory/run-1/pr-58")
	coRun(t, dir, "remote", "add", "tfpush-run-1-58", "https://github.com/acme/repo.git")
	coRun(t, dir, "config", "remote.tfpush-run-1-58.push", "refs/heads/triagefactory/run-1/pr-58:refs/heads/aa/SKY-101")
	coRun(t, dir, "config", "branch.triagefactory/run-1/pr-58.pushRemote", "tfpush-run-1-58")

	if got := PushTargetBranch(dir); got != "aa/SKY-101" {
		t.Errorf("PushTargetBranch = %q, want aa/SKY-101 (the refspec's remote head)", got)
	}
}

// TestPushTargetBranch_BranchRemoteFallback pins the remote-resolution order's
// tail: with no pushRemote / pushDefault, branch.<b>.remote names the push
// remote and its refspec still applies.
func TestPushTargetBranch_BranchRemoteFallback(t *testing.T) {
	dir := ptRepo(t)
	coRun(t, dir, "remote", "add", "fork", "https://github.com/fork/repo.git")
	coRun(t, dir, "config", "remote.fork.push", "refs/heads/work:refs/heads/contrib-work")
	coRun(t, dir, "config", "branch.work.remote", "fork")

	if got := PushTargetBranch(dir); got != "contrib-work" {
		t.Errorf("PushTargetBranch = %q, want contrib-work (branch.<b>.remote fallback)", got)
	}
}

// TestPushTargetBranch_ForeignRefspecIgnored pins that a refspec whose source
// is a DIFFERENT branch doesn't capture the current one — the mapping falls
// back to identity.
func TestPushTargetBranch_ForeignRefspecIgnored(t *testing.T) {
	dir := ptRepo(t)
	coRun(t, dir, "remote", "add", "origin", "https://github.com/acme/repo.git")
	coRun(t, dir, "config", "remote.origin.push", "refs/heads/other:refs/heads/elsewhere")

	if got := PushTargetBranch(dir); got != "work" {
		t.Errorf("PushTargetBranch = %q, want work (a foreign-source refspec must not apply)", got)
	}
}

// TestPushTargetBranch_NonBranchDestination pins the fail-closed case: a
// refspec that maps the branch into a non-branch namespace (refs/for/...,
// Gerrit-style) yields "" — the branch gate has nothing it can authorize.
func TestPushTargetBranch_NonBranchDestination(t *testing.T) {
	dir := ptRepo(t)
	coRun(t, dir, "remote", "add", "origin", "https://github.com/acme/repo.git")
	coRun(t, dir, "config", "remote.origin.push", "refs/heads/work:refs/for/main")

	if got := PushTargetBranch(dir); got != "" {
		t.Errorf("PushTargetBranch = %q, want empty (non-branch destination namespace)", got)
	}
}
