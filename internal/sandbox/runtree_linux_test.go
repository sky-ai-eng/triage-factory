//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// foreignUID is an owner no legitimate run tree ever has — the validation
// must refuse to touch trees (or entries) owned by it.
const foreignUID = 12345

func statUIDGID(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return st.Uid, st.Gid
}

// TestChownRunTree_RejectsInvalidRoots pins the boundary gate: paths a
// compromised orchestrator could use to steer a CAP_CHOWN-holding broker
// somewhere surprising are refused before anything is touched.
func TestChownRunTree_RejectsInvalidRoots(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"relative", "relative/tree"},
		{"root", "/"},
		{"one component", "/tmp"},
		{"non-clean", "/tmp/foo/../bar"},
		{"missing", "/nonexistent-tf-runtree-test/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := (hostOps{}).ChownRunTree(context.Background(), tc.path, ""); err == nil {
				t.Errorf("ChownRunTree(%q) = nil error, want validation rejection", tc.path)
			}
		})
	}

	t.Run("file not dir", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "plain-file")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := (hostOps{}).ChownRunTree(context.Background(), f, ""); err == nil {
			t.Error("ChownRunTree on a plain file = nil error, want rejection")
		}
	})

	t.Run("symlinked root", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if err := (hostOps{}).ChownRunTree(context.Background(), link, ""); err == nil {
			t.Error("ChownRunTree through a symlinked root = nil error, want rejection")
		}
	})

	t.Run("subpath escape", func(t *testing.T) {
		root := t.TempDir()
		for _, sub := range []string{"..", "../x", "/abs", "."} {
			if err := (hostOps{}).ChownRunTree(context.Background(), root, sub); err == nil {
				t.Errorf("ChownRunTree(subpath=%q) = nil error, want escape rejection", sub)
			}
		}
	})
}

// TestChownRunTree_RefusesForeignOwnedRoot pins the ownership
// precondition — the real teeth of the boundary: even a validly-shaped
// path can't hand over a tree the orchestrator doesn't already own.
func TestChownRunTree_RefusesForeignOwnedRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to stage a foreign-owned directory")
	}
	dir := t.TempDir()
	if err := os.Chown(dir, foreignUID, foreignUID); err != nil {
		t.Fatal(err)
	}
	if err := (hostOps{}).ChownRunTree(context.Background(), dir, ""); err == nil {
		t.Error("ChownRunTree on a foreign-owned root = nil error, want ownership rejection")
	}
}

// TestChownRunTree_HandsTreeToSandbox exercises the real recursive
// hand-off, including the symlink-safety property inherited from the
// original in-caller implementation: the link's own owner changes, its
// out-of-tree target's does not.
func TestChownRunTree_HandsTreeToSandbox(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (CAP_CHOWN) to change file owners")
	}

	outside := filepath.Join(t.TempDir(), "outside-target")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "f.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	if err := (hostOps{}).ChownRunTree(context.Background(), root, ""); err != nil {
		t.Fatalf("ChownRunTree: %v", err)
	}

	for _, p := range []string{root, filepath.Join(root, "sub"), filepath.Join(root, "sub", "f.txt"), filepath.Join(root, "link")} {
		if uid, gid := statUIDGID(t, p); uid != WorktreeUID || gid != WorktreeGID {
			t.Errorf("%s owned by %d:%d, want %d:%d", p, uid, gid, WorktreeUID, WorktreeGID)
		}
	}
	if uid, _ := statUIDGID(t, outside); uid == WorktreeUID {
		t.Errorf("symlink target OUTSIDE the tree was chowned — the Lchown symlink-safety property regressed")
	}

	// Idempotent: a second pass over the now sandbox-owned tree succeeds
	// (resume re-chowns an already-handed-over tree).
	if err := (hostOps{}).ChownRunTree(context.Background(), root, ""); err != nil {
		t.Errorf("second ChownRunTree over a sandbox-owned tree: %v", err)
	}
}

// TestChownRunTree_SubpathChownsIntermediatesShallow pins the mid-run
// `workspace add` shape: intermediates between root and the checkout are
// handed over shallowly, the checkout tree recursively, and an unrelated
// sibling of an intermediate is left alone.
func TestChownRunTree_SubpathChownsIntermediatesShallow(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (CAP_CHOWN) to change file owners")
	}

	root := t.TempDir()
	checkout := filepath.Join(root, "owner", "repo")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "f.txt"), []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(root, "owner", "sibling-file")
	if err := os.WriteFile(sibling, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (hostOps{}).ChownRunTree(context.Background(), root, filepath.Join("owner", "repo")); err != nil {
		t.Fatalf("ChownRunTree(subpath): %v", err)
	}

	for _, p := range []string{filepath.Join(root, "owner"), checkout, filepath.Join(checkout, "f.txt")} {
		if uid, _ := statUIDGID(t, p); uid != WorktreeUID {
			t.Errorf("%s owned by uid %d, want %d", p, uid, WorktreeUID)
		}
	}
	if uid, _ := statUIDGID(t, sibling); uid == WorktreeUID {
		t.Errorf("sibling file inside an intermediate dir was chowned; the intermediate hand-off must be shallow")
	}
	if uid, _ := statUIDGID(t, root); uid == WorktreeUID {
		t.Errorf("run root itself was chowned by the subpath form; only intermediates below it should be")
	}
}

// TestChownRunTree_FailsClosedOnForeignEntryInside pins the per-entry
// ownership check DURING the walk: a foreign-owned file planted inside an
// otherwise-legitimate tree fails the operation rather than being
// silently re-owned.
func TestChownRunTree_FailsClosedOnForeignEntryInside(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to stage a foreign-owned entry")
	}
	root := t.TempDir()
	planted := filepath.Join(root, "planted")
	if err := os.WriteFile(planted, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(planted, foreignUID, foreignUID); err != nil {
		t.Fatal(err)
	}
	if err := (hostOps{}).ChownRunTree(context.Background(), root, ""); err == nil {
		t.Error("ChownRunTree over a tree containing a foreign-owned entry = nil error, want fail-closed rejection")
	}
}

// TestRemoveRunTree_Behavior covers the teardown half: idempotent on a
// missing path, removes a normal tree, and (as root) removes a tree the
// sandbox identity owns — the exact post-run state the unprivileged
// orchestrator can no longer unlink through itself.
func TestRemoveRunTree_Behavior(t *testing.T) {
	if err := (hostOps{}).RemoveRunTree(context.Background(), "/nonexistent-tf-runtree-test/gone"); err != nil {
		t.Errorf("RemoveRunTree on a missing path = %v, want nil (idempotent)", err)
	}

	base := t.TempDir()
	tree := filepath.Join(base, "run-root")
	if err := os.MkdirAll(filepath.Join(tree, "deep", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "deep", "deeper", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		// The realistic post-run state: the whole tree sandbox-owned.
		if err := (hostOps{}).ChownRunTree(context.Background(), tree, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := (hostOps{}).RemoveRunTree(context.Background(), tree); err != nil {
		t.Fatalf("RemoveRunTree: %v", err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Errorf("tree still present after RemoveRunTree (stat err = %v)", err)
	}
}

// TestRemoveRunTree_RejectsInvalidAndForeign pins the same boundary gate
// on the destructive op: shallow paths, symlinked roots, and trees owned
// by anyone who isn't a legitimate run-tree owner are refused.
func TestRemoveRunTree_RejectsInvalidAndForeign(t *testing.T) {
	for _, path := range []string{"/", "/tmp", "relative", "/tmp/foo/../bar"} {
		if err := (hostOps{}).RemoveRunTree(context.Background(), path); err == nil {
			t.Errorf("RemoveRunTree(%q) = nil error, want rejection", path)
		}
	}

	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := (hostOps{}).RemoveRunTree(context.Background(), link); err == nil {
		t.Error("RemoveRunTree through a symlinked root = nil error, want rejection")
	}

	if os.Geteuid() == 0 {
		foreign := t.TempDir()
		if err := os.Chown(foreign, foreignUID, foreignUID); err != nil {
			t.Fatal(err)
		}
		if err := (hostOps{}).RemoveRunTree(context.Background(), foreign); err == nil {
			t.Error("RemoveRunTree on a foreign-owned tree = nil error, want ownership rejection")
		}
		// Repair ownership so t.TempDir cleanup can remove it.
		_ = os.Chown(foreign, os.Getuid(), os.Getgid())
	}
}

// TestSetRunTreeOwnerUID_WidensValidation pins the broker-boot wiring: a
// tree owned by the registered orchestrator uid — NOT this process's own
// uid — passes the ownership precondition once registered.
func TestSetRunTreeOwnerUID_WidensValidation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to stage a tree owned by another uid")
	}
	dir := t.TempDir()
	if err := os.Chown(dir, foreignUID, foreignUID); err != nil {
		t.Fatal(err)
	}

	orig := runTreeOwnerExtraUID
	t.Cleanup(func() { runTreeOwnerExtraUID = orig })

	if err := (hostOps{}).ChownRunTree(context.Background(), dir, ""); err == nil {
		t.Fatal("pre-registration: foreign-owned tree accepted; the registration test can't prove anything")
	}
	SetRunTreeOwnerUID(foreignUID)
	if err := (hostOps{}).ChownRunTree(context.Background(), dir, ""); err != nil {
		t.Errorf("post-registration ChownRunTree: %v — the registered orchestrator uid should be an accepted owner", err)
	}
}

// TestBrokeredMode_RejectsRootOwnedTree pins the security property the
// ownership check exists for, and the exact hole a prior version had: once
// brokered (SetRunTreeOwnerUID registered a non-root orchestrator uid),
// this code runs inside the root cap-broker, and a ROOT-owned tree must be
// rejected even though the broker process is itself root. Trusting the
// broker's own uid==0 here would accept every root-owned directory in the
// container image (≥2 components deep) as a legitimate "run tree" and hand
// a compromised, capability-less orchestrator — which still owns and dials
// the broker socket — a root-privileged recursive-chown / RemoveAll /
// setuid-capture primitive against host state, reopening exactly the
// boundary this validation exists to close.
//
// Runs only as root, because that is the posture that reproduces the bug:
// os.Getuid() must be 0 (the broker) while a DIFFERENT uid is the
// registered orchestrator. t.TempDir() is already root-owned there,
// standing in for /usr/local/bin, /var/lib/..., etc.
func TestBrokeredMode_RejectsRootOwnedTree(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: the hole only exists when the broker process's own uid is 0")
	}

	orig := runTreeOwnerExtraUID
	t.Cleanup(func() { runTreeOwnerExtraUID = orig })
	// A non-root orchestrator uid, distinct from this (root) process — the
	// production brokered posture.
	SetRunTreeOwnerUID(foreignUID)

	rootOwned := t.TempDir()
	if uid, _ := statUIDGID(t, rootOwned); uid != 0 {
		t.Fatalf("fixture: tree owned by uid %d, want 0 — this test is only meaningful run as root", uid)
	}

	if err := (hostOps{}).ChownRunTree(context.Background(), rootOwned, ""); err == nil {
		t.Error("ChownRunTree accepted a root-owned tree in brokered mode; the broker's own root uid must not be a legitimate run-tree owner")
	}
	if err := (hostOps{}).RemoveRunTree(context.Background(), rootOwned); err == nil {
		t.Error("RemoveRunTree accepted a root-owned tree in brokered mode; same hole via the destructive op")
	}
	if _, err := (hostOps{}).CaptureRunDelta(context.Background(), rootOwned); err == nil {
		t.Error("CaptureRunDelta accepted a root-owned tree in brokered mode; same hole via the capture op")
	}
}
