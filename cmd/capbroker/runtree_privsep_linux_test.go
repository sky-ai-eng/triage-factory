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
// the shipped image) for the cross-privilege test below — any non-root
// value outside the per-run bands demonstrates the property.
const orchestratorStandInUID = 10001

// TestBrokeredRunTreeOps_AcrossPrivilegeBoundary is the end-to-end shape
// of the production run-tree lifecycle under privsep: a ROOT broker
// serving the REAL hostOps (not a fake) is asked — over the actual RPC —
// to hand a tree owned by the (different, unprivileged) orchestrator uid
// to a jail identity, to park it when that engagement ends, to hand it to
// the next engagement's identity, and later to remove it. This is exactly the
// sequence a delegated run drives via agentproc/worktree once the
// exec-time capability drop leaves the orchestrator unable to perform
// either operation itself.
//
// Needs root: the hand-off is a real chown to a jail uid.
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

	ownerOf := func(p string) (uint32, uint32) {
		t.Helper()
		var st syscall.Stat_t
		if err := syscall.Lstat(p, &st); err != nil {
			t.Fatal(err)
		}
		return st.Uid, st.Gid
	}
	ctx := context.Background()
	first, second := sandbox.JailUID(3), sandbox.JailUID(9)
	if err := client.ChownRunTree(ctx, tree, "", first); err != nil {
		t.Fatalf("brokered ChownRunTree: %v", err)
	}
	if uid, gid := ownerOf(filepath.Join(tree, "src", "main.go")); int(uid) != first || int(gid) != os.Getgid() {
		t.Errorf("tree entry owned by %d:%d after brokered hand-off, want %d:%d", uid, gid, first, os.Getgid())
	}

	// The engagement ends: its root is parked, so its jail identity holds
	// nothing a later engagement drawing the same index could reach.
	if err := client.ParkRunTree(ctx, tree, first); err != nil {
		t.Fatalf("brokered ParkRunTree: %v", err)
	}
	if uid, _ := ownerOf(tree); int(uid) != sandbox.ParkedRunTreeUID {
		t.Errorf("root owned by %d after park, want the parked owner %d", uid, sandbox.ParkedRunTreeUID)
	}

	// The next claim holds another index, and the whole tree follows it.
	if err := client.ChownRunTree(ctx, tree, "", second); err != nil {
		t.Fatalf("brokered ChownRunTree to the next engagement: %v", err)
	}
	for _, p := range []string{tree, filepath.Join(tree, "src", "main.go")} {
		if uid, _ := ownerOf(p); int(uid) != second {
			t.Errorf("%s owned by %d after the second hand-off, want %d", p, uid, second)
		}
	}

	// An identity outside the jail band is refused before anything is touched.
	if err := client.ChownRunTree(ctx, tree, "", 0); err == nil {
		t.Error("brokered ChownRunTree accepted uid 0 as a jail identity")
	}

	// Teardown of the now jail-owned tree — the half an unprivileged
	// orchestrator can't do in-process (no write through jail-owned dirs).
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
	if err := client.ChownRunTree(context.Background(), foreign, "", first); err == nil {
		t.Error("brokered ChownRunTree accepted a foreign-owned tree; the ownership precondition must hold at the RPC boundary")
	}
	_ = os.Chown(foreign, os.Getuid(), os.Getgid())
}
