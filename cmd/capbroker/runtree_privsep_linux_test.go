//go:build linux

package capbroker

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// orchestratorStandInUID plays the dropped orchestrator's uid (10001 in
// the shipped image) for the cross-privilege test below — any non-root,
// non-WorktreeUID value demonstrates the property.
const orchestratorStandInUID = 10001

// TestBrokeredRunTreeOps_AcrossPrivilegeBoundary is the end-to-end shape
// of the production run-tree lifecycle under privsep: a ROOT broker
// serving the REAL hostOps (not a fake) is asked — over the actual RPC —
// to hand a tree owned by the (different, unprivileged) orchestrator uid
// to the sandbox identity, and later to remove it. This is exactly the
// sequence a delegated run drives via agentproc/worktree once the
// exec-time capability drop leaves the orchestrator unable to perform
// either operation itself.
//
// Needs root: the hand-off is a real chown to WorktreeUID.
func TestBrokeredRunTreeOps_AcrossPrivilegeBoundary(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown across uids")
	}

	// Register the stand-in orchestrator uid exactly as runBroker's
	// --orchestrator-uid wiring does.
	sandbox.SetRunTreeOwnerUID(orchestratorStandInUID)
	t.Cleanup(func() { sandbox.SetRunTreeOwnerUID(-1) })

	client := serveTestBroker(t, sandbox.NewHostOps())

	// A run tree as the orchestrator would have created it: owned by the
	// orchestrator uid, contents included.
	tree := filepath.Join(t.TempDir(), "run-root")
	if err := os.MkdirAll(filepath.Join(tree, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "src", "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{tree, filepath.Join(tree, "src"), filepath.Join(tree, "src", "main.go")} {
		if err := os.Lchown(p, orchestratorStandInUID, orchestratorStandInUID); err != nil {
			t.Fatal(err)
		}
	}

	if err := client.ChownRunTree(context.Background(), tree, ""); err != nil {
		t.Fatalf("brokered ChownRunTree: %v", err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(tree, "src", "main.go"), &st); err != nil {
		t.Fatal(err)
	}
	if st.Uid != sandbox.WorktreeUID || st.Gid != sandbox.WorktreeGID {
		t.Errorf("tree entry owned by %d:%d after brokered hand-off, want %d:%d", st.Uid, st.Gid, sandbox.WorktreeUID, sandbox.WorktreeGID)
	}

	// Teardown of the now sandbox-owned tree — the half an unprivileged
	// orchestrator can't do in-process (no write through 10000-owned dirs).
	if err := client.RemoveRunTree(context.Background(), tree); err != nil {
		t.Fatalf("brokered RemoveRunTree: %v", err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Errorf("tree still present after brokered removal (stat err = %v)", err)
	}

	// And the boundary holds over the wire too: a foreign-owned tree is
	// refused end-to-end.
	foreign := t.TempDir()
	if err := os.Chown(foreign, 4321, 4321); err != nil {
		t.Fatal(err)
	}
	if err := client.ChownRunTree(context.Background(), foreign, ""); err == nil {
		t.Error("brokered ChownRunTree accepted a foreign-owned tree; the ownership precondition must hold at the RPC boundary")
	}
	_ = os.Chown(foreign, os.Getuid(), os.Getgid())
}
