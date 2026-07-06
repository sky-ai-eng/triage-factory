package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

// foreignUID is a uid that (almost certainly) differs from the test process's
// euid, used to reproduce the sandbox-chowned run root without depending on any
// real system user existing. Production hits this when the host process (root,
// uid 0) reads a run root that agentproc.chownWorktreeForSandbox has recursively
// chowned to sandbox.WorktreeUID (10000) for the jailed agent.
const foreignUID = 65534

// chownToForeignOwner chowns dir to a uid other than this process's, the shape
// the sandbox chown leaves behind. Skips (rather than fails) when the chown
// itself fails: euid==0 is not sufficient to guarantee CAP_CHOWN in every
// environment (rootless / user-namespaced containers in particular), so
// attempt-then-skip is the robust gate.
func chownToForeignOwner(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chown(dir, foreignUID, foreignUID); err != nil {
		t.Skipf("can't chown %s to a foreign uid (needs root/CAP_CHOWN): %v", dir, err)
	}
}

// TestPushTargetBranch_ForeignOwnedWorktree is the regression for the original
// push-403: on a run root owned by a different uid than the host process, the
// push-gate reads must still succeed, or gitAuthorizeDecision's AllowedRefs is
// starved and every multi-mode push is denied "ref-not-allowed". The reads work
// because they never enter the repository — CurrentBranch reads .git/HEAD as a
// file, the config reads run `git config --file` from outside any repo — so
// git's dubious-ownership check never fires regardless of who owns the tree.
func TestPushTargetBranch_ForeignOwnedWorktree(t *testing.T) {
	dir := ptRepo(t)
	chownToForeignOwner(t, dir)

	if got := PushTargetBranch(dir); got != "work" {
		t.Errorf("PushTargetBranch on a foreign-owned worktree = %q, want work", got)
	}
}

// TestCurrentBranch_ForeignOwnedWorktree pins the same guarantee directly
// against CurrentBranch, the primitive PushTargetBranch calls.
func TestCurrentBranch_ForeignOwnedWorktree(t *testing.T) {
	dir := ptRepo(t)
	chownToForeignOwner(t, dir)

	if got := CurrentBranch(dir); got != "work" {
		t.Errorf("CurrentBranch on a foreign-owned worktree = %q, want work", got)
	}
}

// TestPushGateReads_IgnoreAgentIncludePath is the regression for the
// include.path oracle. The run root's .git/config is agent-writable (chowned to
// the sandbox uid), so a compromised agent can plant an include.path there. The
// push-gate reads run host-side as root and must NOT open+parse whatever that
// include names. Here the include points at a MALFORMED config file: if any
// read entered the repository and followed the config's includes, git would
// error and blank the read. The reads must instead return the real values,
// proving the attacker's include was never consulted.
//
// Needs no root — the include-open is git's config bootstrap, not an ownership
// condition, so it reproduces same-user. This test FAILS against the earlier
// `-c safe.directory` reads (symbolic-ref and `config --get-all` both open the
// include regardless of --no-includes) and passes once the branch read is a
// plain HEAD read and the config reads run from outside the repo.
func TestPushGateReads_IgnoreAgentIncludePath(t *testing.T) {
	dir := ptRepo(t)
	coRun(t, dir, "checkout", "-b", "triagefactory/run-9/pr-7")
	coRun(t, dir, "remote", "add", "tfpush-run-9-7", "https://github.com/acme/repo.git")
	coRun(t, dir, "config", "remote.tfpush-run-9-7.push",
		"refs/heads/triagefactory/run-9/pr-7:refs/heads/aa/real-head")
	coRun(t, dir, "config", "branch.triagefactory/run-9/pr-7.pushRemote", "tfpush-run-9-7")

	// Plant the trap LAST (any git command after this would itself trip on the
	// malformed include): reference an unparseable file via include.path.
	bad := filepath.Join(t.TempDir(), "evil-include")
	if err := os.WriteFile(bad, []byte("THIS IS NOT VALID GIT CONFIG\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	coRun(t, dir, "config", "include.path", bad)

	if got := CurrentBranch(dir); got != "triagefactory/run-9/pr-7" {
		t.Errorf("CurrentBranch = %q, want the branch name; an opened include.path would blank it", got)
	}
	if got := PushTargetBranch(dir); got != "aa/real-head" {
		t.Errorf("PushTargetBranch = %q, want aa/real-head; an opened include.path would error the refspec read and blank it", got)
	}
}

// Note: the parking snapshot's CaptureWorkspaceGit is NOT covered here. Unlike
// the push-gate reads (a HEAD byte read + config read from outside the repo),
// its captureUncommitted step runs `git add -A` / `git diff`, which consult the
// repository's own .gitattributes + .git/config to decide whether to invoke an
// external clean/smudge filter or diff driver — content a compromised sandboxed
// agent can write. Making that path ownership-tolerant is a sandbox escape, not
// a fix; see gitCapture's doc comment in snapshot.go. That capture hardening is
// tracked separately, pending a fix that runs the capture with no more
// privilege than the agent had (or avoids filter-honoring git entirely).
