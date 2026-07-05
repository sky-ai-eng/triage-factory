package worktree

import (
	"context"
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

// requireChownCapable skips the test when the process can't chown a path to
// an arbitrary uid (needs root / CAP_CHOWN) — same gate the sandbox
// integration tests use (see internal/sandbox/integration_linux_test.go's
// minimalConfig).
func requireChownCapable(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("chown to a foreign uid needs root; skipping dubious-ownership regression test")
	}
}

// chownToForeignOwner reproduces the sandbox chown's effect on git's
// ownership check. Git's dubious-ownership guard compares only the
// repository's TOP-LEVEL working directory owner against the running euid,
// so chowning just dir (not recursively) reproduces it exactly — verified
// against chownWorktreeForSandbox's recursive Lchown, which necessarily
// covers dir itself as one of the walked entries.
func chownToForeignOwner(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chown(dir, foreignUID, foreignUID); err != nil {
		t.Fatalf("chown %s to foreign uid: %v", dir, err)
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
	requireChownCapable(t)
	dir := ptRepo(t)
	chownToForeignOwner(t, dir)

	if got := PushTargetBranch(dir); got != "work" {
		t.Errorf("PushTargetBranch on a foreign-owned worktree = %q, want work (dubious ownership must not blank the read)", got)
	}
}

// TestCurrentBranch_DubiousOwnership pins the same guarantee directly against
// CurrentBranch, the lower-level primitive PushTargetBranch itself calls.
func TestCurrentBranch_DubiousOwnership(t *testing.T) {
	requireChownCapable(t)
	dir := ptRepo(t)
	chownToForeignOwner(t, dir)

	if got := CurrentBranch(dir); got != "work" {
		t.Errorf("CurrentBranch on a foreign-owned worktree = %q, want work", got)
	}
}

// TestCaptureWorkspaceGit_DubiousOwnership is the parking-snapshot half of the
// TFAC-558 regression: `rev-parse HEAD` against a foreign-owned run root (the
// "detected dubious ownership" WARN from the bug report) must succeed so the
// snapshot captures the agent's work instead of failing outright.
func TestCaptureWorkspaceGit_DubiousOwnership(t *testing.T) {
	requireChownCapable(t)
	dir := ptRepo(t)
	wantHead := coOut(t, dir, "rev-parse", "HEAD")
	chownToForeignOwner(t, dir)

	delta, err := CaptureWorkspaceGit(context.Background(), dir)
	if err != nil {
		t.Fatalf("CaptureWorkspaceGit on a foreign-owned worktree: %v", err)
	}
	if delta == nil {
		t.Fatal("CaptureWorkspaceGit returned nil delta")
	}
	if delta.Head != wantHead {
		t.Errorf("delta.Head = %q, want %q", delta.Head, wantHead)
	}
	if delta.Branch != "work" {
		t.Errorf("delta.Branch = %q, want work", delta.Branch)
	}
}
