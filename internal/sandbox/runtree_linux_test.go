//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"golang.org/x/sys/unix"
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

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// TestChownRunTree_GroupTargetAndScaffoldMode pins the run-tree ownership model
// that lets the orchestrator materialize checkouts into a run root it has handed
// to the agent WITHOUT opening that tree to the per-run sidecar. With the
// orchestrator's gid registered, the run root and the owner/repo scaffold
// intermediates become owner=agent, group=orchestrator, mode 0770 (agent-owner
// and orchestrator-group writable, no access for anyone else — critically not
// WorktreeGID, which every sidecar carries). A checkout's OWN tree keeps the
// modes git wrote (only its group moves), since the orchestrator only needs to
// reach it through the scaffold, not re-mode it.
func TestChownRunTree_GroupTargetAndScaffoldMode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (CAP_CHOWN) to change file owners")
	}
	const orchGID = 10001 // distinct from WorktreeGID; carried by nothing sandboxed
	SetRunTreeGroupGID(orchGID)
	t.Cleanup(func() { SetRunTreeGroupGID(WorktreeGID) })

	// Run-start form (subpath==""): a lazy run root with a _scratch child.
	root := t.TempDir()
	scratch := filepath.Join(root, "_scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(scratch, 0o700); err != nil { // defeat umask so the "preserved" assertion is exact
		t.Fatal(err)
	}
	if err := (hostOps{}).ChownRunTree(context.Background(), root, ""); err != nil {
		t.Fatalf("ChownRunTree(root): %v", err)
	}
	// The root is scaffold: agent-owner, orchestrator-group, 0770.
	if uid, gid := statUIDGID(t, root); uid != WorktreeUID || gid != orchGID {
		t.Errorf("root owned %d:%d, want %d:%d (agent owner, orchestrator group)", uid, gid, WorktreeUID, orchGID)
	}
	if m := statMode(t, root); m != 0o770 {
		t.Errorf("root mode = %o, want 0770 (agent+orchestrator writable, no other access)", m)
	}
	// A non-scaffold child keeps its creation mode; only its group moves.
	if m := statMode(t, scratch); m != 0o700 {
		t.Errorf("_scratch mode = %o, want its 0700 preserved (children are not re-moded)", m)
	}
	if _, gid := statUIDGID(t, scratch); gid != orchGID {
		t.Errorf("_scratch gid = %d, want %d", gid, orchGID)
	}

	// Mid-run form (subpath!=""): owner/repo intermediates become scaffold-mode;
	// the checkout leaf keeps its own dir mode.
	root2 := t.TempDir()
	if err := (hostOps{}).ChownRunTree(context.Background(), root2, ""); err != nil {
		t.Fatalf("ChownRunTree(root2): %v", err)
	}
	leaf := filepath.Join(root2, "sky-ai-eng", "triage-factory", "main")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(leaf, 0o755); err != nil { // defeat umask for the exact-mode assertion
		t.Fatal(err)
	}
	rel := filepath.Join("sky-ai-eng", "triage-factory", "main")
	if err := (hostOps{}).ChownRunTree(context.Background(), root2, rel); err != nil {
		t.Fatalf("ChownRunTree(subpath): %v", err)
	}
	for _, d := range []string{filepath.Join(root2, "sky-ai-eng"), filepath.Join(root2, "sky-ai-eng", "triage-factory")} {
		if m := statMode(t, d); m != 0o770 {
			t.Errorf("intermediate %s mode = %o, want 0770", d, m)
		}
		if uid, gid := statUIDGID(t, d); uid != WorktreeUID || gid != orchGID {
			t.Errorf("intermediate %s owned %d:%d, want %d:%d", d, uid, gid, WorktreeUID, orchGID)
		}
	}
	// The checkout leaf keeps the mode it was created with (not forced to 0770).
	if m := statMode(t, leaf); m != 0o755 {
		t.Errorf("checkout leaf mode = %o, want its 0755 preserved (checkouts are not scaffold)", m)
	}
	if uid, gid := statUIDGID(t, leaf); uid != WorktreeUID || gid != orchGID {
		t.Errorf("checkout leaf owned %d:%d, want %d:%d", uid, gid, WorktreeUID, orchGID)
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

// requireOpenat2Supported skips the test if this kernel can't do
// RESOLVE_NO_SYMLINKS — the openat2-specific tests below assert on the
// kernel-enforced code path, not the fallback, so they need a direct probe
// of the REAL kernel capability. The shared runTreeOpenat2Unsupported latch
// is the wrong thing to check here: it only reflects "has some run-tree op
// already observed ENOSYS", so on a kernel that genuinely lacks openat2 it
// still reads false (and this helper would wrongly say "supported") until
// some earlier test happens to trip it — order-dependent and easy to get
// wrong. A direct probe has no such dependency.
func requireOpenat2Supported(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	fd, err := unix.Openat2(unix.AT_FDCWD, dir, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skip("this kernel doesn't support openat2(RESOLVE_NO_SYMLINKS)")
		}
		t.Fatalf("probing openat2 support: %v", err)
	}
	unix.Close(fd)
}

// TestChownRunTree_RejectsSymlinkedParentComponent pins the openat2 half
// of the boundary directly: a run tree whose path resolves THROUGH a
// symlinked ancestor (not the root argument itself, which the pre-existing
// "symlinked root" case above already covers) is refused by the kernel at
// the anchor open, before any entry inside is ever touched.
func TestChownRunTree_RejectsSymlinkedParentComponent(t *testing.T) {
	requireOpenat2Supported(t)

	base := t.TempDir()
	realGrandparent := filepath.Join(base, "real-gp")
	root := filepath.Join(realGrandparent, "mid", "run-root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedGrandparent := filepath.Join(base, "linked-gp")
	if err := os.Symlink(realGrandparent, linkedGrandparent); err != nil {
		t.Fatal(err)
	}
	rootViaSymlink := filepath.Join(linkedGrandparent, "mid", "run-root")

	if err := (hostOps{}).ChownRunTree(context.Background(), rootViaSymlink, ""); err == nil {
		t.Error("ChownRunTree through a symlinked ancestor component = nil error, want kernel refusal")
	}
	if err := (hostOps{}).RemoveRunTree(context.Background(), rootViaSymlink); err == nil {
		t.Error("RemoveRunTree through a symlinked ancestor component = nil error, want kernel refusal")
	}
}

// TestRemoveRunTree_SymlinkInsideTree_UnlinksLinkNotTarget mirrors
// TestChownRunTree_HandsTreeToSandbox's symlink-safety property for the
// destructive op: a symlink planted inside the tree pointing outside it is
// unlinked as itself — its out-of-tree target is left untouched — rather
// than being followed. Needs no root: removal never chowns.
func TestRemoveRunTree_SymlinkInsideTree_UnlinksLinkNotTarget(t *testing.T) {
	requireOpenat2Supported(t)

	outside := filepath.Join(t.TempDir(), "outside-target")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	if err := (hostOps{}).RemoveRunTree(context.Background(), root); err != nil {
		t.Fatalf("RemoveRunTree: %v", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Errorf("tree still present after RemoveRunTree (stat err = %v)", err)
	}
	if _, err := os.Lstat(outside); err != nil {
		t.Errorf("symlink target OUTSIDE the tree was removed by RemoveRunTree: %v", err)
	}
}

// TestRemoveRunTree_MissingLeafUnderExistingParent_IsNoop pins the other
// half of the idempotent-missing-tree contract: not just a wholly missing
// path (TestRemoveRunTree_Behavior), but a missing leaf directory whose
// parent DOES exist — the shape that exercises the second (Openat, not
// openat2) ENOENT in the fd-relative implementation.
func TestRemoveRunTree_MissingLeafUnderExistingParent_IsNoop(t *testing.T) {
	requireOpenat2Supported(t)

	parent := t.TempDir()
	missing := filepath.Join(parent, "gone")
	if err := (hostOps{}).RemoveRunTree(context.Background(), missing); err != nil {
		t.Errorf("RemoveRunTree on a missing leaf under an existing parent = %v, want nil (idempotent)", err)
	}
}

// withForcedOpenat2Unsupported points openat2Syscall at a stub that always
// returns ENOSYS — simulating a pre-5.6 kernel without needing one — and
// resets both the seam and the process-wide fallback latch on cleanup so
// later tests still exercise the real kernel-enforced path.
func withForcedOpenat2Unsupported(t *testing.T) (calls *int) {
	t.Helper()
	n := 0
	origSyscall := openat2Syscall
	openat2Syscall = func(dirfd int, path string, how *unix.OpenHow) (int, error) {
		n++
		return -1, unix.ENOSYS
	}
	t.Cleanup(func() {
		openat2Syscall = origSyscall
		runTreeOpenat2Unsupported.Store(false)
	})
	return &n
}

// TestRunTreeOps_FallBackWhenOpenat2Unsupported pins the decided ENOSYS
// contract via the test seam: when openat2 itself reports unsupported, both
// ops still succeed (through the untouched EvalSymlinks-based fallback) and
// the decision latches process-wide — a second call never even attempts
// openat2 again.
func TestRunTreeOps_FallBackWhenOpenat2Unsupported(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (CAP_CHOWN) to change file owners")
	}
	calls := withForcedOpenat2Unsupported(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (hostOps{}).ChownRunTree(context.Background(), root, ""); err != nil {
		t.Fatalf("ChownRunTree with openat2 unsupported: %v", err)
	}
	if uid, _ := statUIDGID(t, filepath.Join(root, "sub")); uid != WorktreeUID {
		t.Errorf("fallback did not chown %s to the sandbox identity", filepath.Join(root, "sub"))
	}
	if !runTreeOpenat2Unsupported.Load() {
		t.Fatal("fallback latch not set after an ENOSYS openat2 response")
	}

	if err := (hostOps{}).RemoveRunTree(context.Background(), root); err != nil {
		t.Fatalf("RemoveRunTree with openat2 unsupported: %v", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Errorf("tree still present after fallback RemoveRunTree")
	}

	callsAfterLatch := *calls
	root2 := t.TempDir()
	if err := (hostOps{}).ChownRunTree(context.Background(), root2, ""); err != nil {
		t.Fatalf("ChownRunTree after latch: %v", err)
	}
	if *calls != callsAfterLatch {
		t.Errorf("openat2Syscall invoked again after the fallback latch was set (calls %d -> %d); it should skip straight to the fallback", callsAfterLatch, *calls)
	}
}

// TestRunTreeOps_WarnsOnceWhenOpenat2Unsupported pins the decided logging
// contract: the WARN naming the kernel-version cause fires exactly once
// process-wide, not once per call.
func TestRunTreeOps_WarnsOnceWhenOpenat2Unsupported(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (CAP_CHOWN) to change file owners")
	}
	withForcedOpenat2Unsupported(t)

	var buf bytes.Buffer
	restore := logging.SetOutput(&buf)
	defer restore()

	root1 := t.TempDir()
	root2 := t.TempDir()
	if err := (hostOps{}).ChownRunTree(context.Background(), root1, ""); err != nil {
		t.Fatalf("ChownRunTree: %v", err)
	}
	if err := (hostOps{}).ChownRunTree(context.Background(), root2, ""); err != nil {
		t.Fatalf("ChownRunTree: %v", err)
	}

	// "openat2" itself appears twice PER RECORD (once in msg, once in the
	// wrapped err attribute), so count records via level=WARN instead.
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Errorf("openat2-unsupported warning logged %d times, want exactly 1:\n%s", n, buf.String())
	}
}

// TestOpenEntryPinned_ImmuneToNameSwapAfterOpen pins the actual
// TOCTOU-closure property, deterministically rather than via a real race:
// openEntryPinned resolves a name exactly once; every operation after
// that (the owner check against st, and chownPinnedEntry) acts on the
// pinned fd, not a fresh lookup of the name. So even if the directory
// entry is swapped for a foreign-owned one AFTER the fd is pinned but
// BEFORE the chown runs — exactly the window a "Fstatat(name) then
// Fchownat(name)" pair would race — the chown still lands on the
// originally-checked inode, and the swapped-in file is left untouched.
func TestOpenEntryPinned_ImmuneToNameSwapAfterOpen(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (CAP_CHOWN) to change file owners")
	}

	dir := t.TempDir()
	victim := filepath.Join(dir, "entry")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirFd, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFd(dirFd)

	fd, st, isPathFd, err := openEntryPinned(dirFd, "entry")
	if err != nil {
		t.Fatalf("openEntryPinned: %v", err)
	}
	defer closeFd(fd)
	if isPathFd {
		t.Fatal("expected a plain (non-O_PATH) fd for a regular file")
	}
	if !allowedRunTreeOwner(st.Uid) {
		t.Fatalf("fixture: entry not initially owned by an allowed uid (got %d)", st.Uid)
	}

	// Simulate the race: after the fd is pinned but before chownPinnedEntry
	// runs, "entry" gets renamed out from under it to a foreign-owned file.
	foreign := filepath.Join(dir, "attacker-planted")
	if err := os.WriteFile(foreign, []byte("swapped"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(foreign, foreignUID, foreignUID); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(foreign, victim); err != nil {
		t.Fatal(err)
	}

	if err := chownPinnedEntry(fd); err != nil {
		t.Fatalf("chownPinnedEntry: %v", err)
	}

	var afterStat unix.Stat_t
	if err := unix.Fstat(fd, &afterStat); err != nil {
		t.Fatal(err)
	}
	if afterStat.Uid != WorktreeUID {
		t.Errorf("originally pinned inode owned by %d after chown, want %d — chown followed the renamed name instead of the pinned fd", afterStat.Uid, WorktreeUID)
	}
	if swappedUID, _ := statUIDGID(t, victim); swappedUID != foreignUID {
		t.Errorf("the swapped-in foreign-owned file's owner changed to %d — the chown followed the name instead of staying pinned to the original inode", swappedUID)
	}
}
