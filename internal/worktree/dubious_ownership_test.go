package worktree

import (
	"os"
	"testing"
)

// foreignUID is a uid that (almost certainly) differs from the test
// process's euid, used to reproduce git's dubious-ownership refusal without
// depending on any real system user existing — the check only compares
// numeric owner against euid, it never resolves the uid to a name. TFAC-558:
// production hits this exact condition when the host process (root, uid 0)
// reads a run root that agentproc.chownWorktreeForSandbox has recursively
// chowned to sandbox.WorktreeUID (10000) for the jailed agent.
const foreignUID = 65534

// chownToForeignOwner reproduces the sandbox chown's effect on git's
// ownership check by chowning dir to a uid other than this process's. Git's
// dubious-ownership guard compares only the repository's TOP-LEVEL working
// directory owner against the running euid, so chowning just dir (not
// recursively) reproduces it exactly — chownWorktreeForSandbox's recursive
// Lchown necessarily covers dir itself as one of the walked entries.
//
// Skips (rather than fails) when the chown itself fails: euid==0 is not
// sufficient to guarantee CAP_CHOWN in every environment (rootless /
// user-namespaced containers in particular), so attempt-then-skip is the
// robust gate — same pattern as internal/sandbox/integration_linux_test.go's
// minimalConfig, which tries os.Chown and skips on error rather than
// pre-checking os.Geteuid().
func chownToForeignOwner(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chown(dir, foreignUID, foreignUID); err != nil {
		t.Skipf("can't chown %s to a foreign uid (needs root/CAP_CHOWN): %v", dir, err)
	}
}

// TestPushTargetBranch_DubiousOwnership is the TFAC-558 regression: a
// worktree owned by a different uid than the running process (the shape
// agentproc.chownWorktreeForSandbox leaves behind for every multi-mode run)
// must not make PushTargetBranch silently return "" — that's exactly what
// starved the push-authorization gate's AllowedRefs and turned every
// multi-mode push into a "ref-not-allowed" 403 (gitAuthorizeDecision treats
// an unreadable branch identically to a detached HEAD: nothing authorized).
func TestPushTargetBranch_DubiousOwnership(t *testing.T) {
	dir := ptRepo(t)
	chownToForeignOwner(t, dir)

	if got := PushTargetBranch(dir); got != "work" {
		t.Errorf("PushTargetBranch on a foreign-owned worktree = %q, want work (dubious ownership must not blank the read)", got)
	}
}

// TestCurrentBranch_DubiousOwnership pins the same guarantee directly against
// CurrentBranch, the lower-level primitive PushTargetBranch itself calls.
func TestCurrentBranch_DubiousOwnership(t *testing.T) {
	dir := ptRepo(t)
	chownToForeignOwner(t, dir)

	if got := CurrentBranch(dir); got != "work" {
		t.Errorf("CurrentBranch on a foreign-owned worktree = %q, want work", got)
	}
}

// Note: the parking snapshot's CaptureWorkspaceGit is NOT covered here.
// Unlike CurrentBranch/PushTargetBranch (pure config/ref reads), its
// captureUncommitted step runs `git add -A` / `git diff`, which consult the
// repository's own .gitattributes + .git/config to decide whether to invoke
// an external clean/smudge filter or diff driver — content a compromised
// sandboxed agent can write. Bypassing dubious-ownership there is a sandbox
// escape, not a fix; see gitCapture's doc comment in snapshot.go. That half
// of TFAC-558 stays open pending a fix that resolves such config from a
// source the agent can't write.
