//go:build linux

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// withTempSocketRoot points the per-run socket root at a fresh directory, as
// the broker's listen would leave it.
func withTempSocketRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "run-tf")
	if err := os.Mkdir(root, 0o711); err != nil {
		t.Fatal(err)
	}
	orig := trustedAgentHostSocketRoot
	trustedAgentHostSocketRoot = root
	t.Cleanup(func() { trustedAgentHostSocketRoot = orig })
	return root
}

func statOwnerMode(t *testing.T, path string) (uid, gid uint32, mode os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return st.Uid, st.Gid, fi.Mode()
}

// TestPrepareRunSockets_OwnersAndModes pins the layout every engagement's
// sockets live in: the sidecar's directory owned by its uid with the run's
// jail gid and no access for others; the tool host's owned by the
// orchestrator, setgid to the same jail gid so the socket bound inside takes
// it; and a predecessor's leftovers at either path replaced, not inherited.
func TestPrepareRunSockets_OwnersAndModes(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to give the directories away")
	}
	withTempSocketRoot(t)
	const conv, idx = "conv-prep", 6

	// A predecessor engagement at another index left both directories, with
	// something inside each.
	for _, d := range []string{RunSocketDir(conv), TrustedToolHostSocketDir(conv)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "stale"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(d, SidecarUID(2), JailGID(2)); err != nil {
			t.Fatal(err)
		}
	}

	if err := (hostOps{}).PrepareRunSockets(context.Background(), conv, idx); err != nil {
		t.Fatalf("PrepareRunSockets: %v", err)
	}

	uid, gid, mode := statOwnerMode(t, RunSocketDir(conv))
	if int(uid) != SidecarUID(idx) || int(gid) != JailGID(idx) || mode.Perm() != 0o750 || mode&(os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("sidecar socket dir = %d:%d %v, want %d:%d drwxr-x---", uid, gid, mode, SidecarUID(idx), JailGID(idx))
	}
	uid, gid, mode = statOwnerMode(t, TrustedToolHostSocketDir(conv))
	if int(uid) != os.Getuid() || int(gid) != JailGID(idx) || mode.Perm() != 0o750 || mode&os.ModeSetgid == 0 {
		t.Errorf("tool host socket dir = %d:%d %v, want %d:%d setgid 0750", uid, gid, mode, os.Getuid(), JailGID(idx))
	}
	for _, d := range []string{RunSocketDir(conv), TrustedToolHostSocketDir(conv)} {
		if _, err := os.Lstat(filepath.Join(d, "stale")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still holds the predecessor's entry (lstat err %v)", d, err)
		}
	}

	// A socket bound inside the tool host's directory takes the jail gid at
	// birth, from a process that does not carry it.
	sock := filepath.Join(TrustedToolHostSocketDir(conv), "probe.sock")
	listenForProbes(t, sock)
	if _, gid, _ := statOwnerMode(t, sock); int(gid) != JailGID(idx) {
		t.Errorf("socket bound in the tool host dir has gid %d, want the jail gid %d", gid, JailGID(idx))
	}
}

// TestPrepareRunSockets_RefusesPathShapedRunID pins that a run id cannot
// steer the root-privileged remove-and-recreate anywhere but its own entry.
func TestPrepareRunSockets_RefusesPathShapedRunID(t *testing.T) {
	root := withTempSocketRoot(t)
	sentinel := filepath.Join(root, "keep")
	if err := os.WriteFile(sentinel, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../escape", "a/b", ".."} {
		if err := (hostOps{}).PrepareRunSockets(context.Background(), id, 1); err == nil {
			t.Errorf("PrepareRunSockets(%q) = nil error, want refusal", id)
		}
	}
	if _, err := os.Lstat(sentinel); err != nil {
		t.Errorf("a refused prepare touched the socket root: %v", err)
	}
}

// TestReleaseRunSockets_OnlyThisEngagements pins the guard: the directories
// go only while they still carry the releasing engagement's identity, so a
// successor that already replaced them keeps its live sockets.
func TestReleaseRunSockets_OnlyThisEngagements(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to give the directories away")
	}
	withTempSocketRoot(t)
	ctx := context.Background()
	const conv = "conv-rel"

	// A successor at index 4 replaced the directories of the engagement at 2.
	if err := (hostOps{}).PrepareRunSockets(ctx, conv, 4); err != nil {
		t.Fatal(err)
	}
	if err := (hostOps{}).ReleaseRunSockets(ctx, conv, 2); err != nil {
		t.Fatalf("ReleaseRunSockets for the predecessor: %v", err)
	}
	for _, d := range []string{RunSocketDir(conv), TrustedToolHostSocketDir(conv)} {
		if _, err := os.Lstat(d); err != nil {
			t.Errorf("the predecessor's release removed the successor's %s: %v", d, err)
		}
	}

	// The successor's own release removes them; a second is a no-op.
	for i := 0; i < 2; i++ {
		if err := (hostOps{}).ReleaseRunSockets(ctx, conv, 4); err != nil {
			t.Fatalf("ReleaseRunSockets (pass %d): %v", i, err)
		}
	}
	for _, d := range []string{RunSocketDir(conv), TrustedToolHostSocketDir(conv)} {
		if _, err := os.Lstat(d); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived its own engagement's release (lstat err %v)", d, err)
		}
	}
}

// TestReapOrphanRunSockets_KeepsOnlyTheBroker pins the boot sweep: every
// per-run entry a crashed process left goes — they carry identities this boot
// will hand out again — and the broker's own socket stays.
func TestReapOrphanRunSockets_KeepsOnlyTheBroker(t *testing.T) {
	root := withTempSocketRoot(t)
	broker := filepath.Join(root, "cap-broker.sock")
	for _, p := range []string{broker, filepath.Join(root, "conv-x-stdio.sock")} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{RunSocketDir("conv-x"), TrustedToolHostSocketDir("conv-x")} {
		if err := os.MkdirAll(filepath.Join(d, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	reapOrphanRunSockets()

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "cap-broker.sock" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("socket root after the boot sweep holds %v, want only cap-broker.sock", names)
	}
}

// TestGrantRunStaging_Boundary pins what the privileged chgrp will touch: a
// real directory exactly one level under a staging base, owned by the
// orchestrator — and that it narrows the directory to the jail gid with no
// access for others.
func TestGrantRunStaging_Boundary(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chgrp to a jail gid")
	}
	t.Setenv("TMPDIR", t.TempDir())
	ctx := context.Background()
	if err := os.MkdirAll(MemoryStagingBase(), 0o700); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(MemoryStagingBase(), "conv-g")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (hostOps{}).GrantRunStaging(ctx, dir, 8); err != nil {
		t.Fatalf("GrantRunStaging: %v", err)
	}
	if uid, gid, mode := statOwnerMode(t, dir); int(uid) != os.Getuid() || int(gid) != JailGID(8) || mode.Perm() != 0o750 {
		t.Errorf("granted staging = %d:%d %v, want %d:%d 0750", uid, gid, mode, os.Getuid(), JailGID(8))
	}
	if err := revokeRunStaging(dir); err != nil {
		t.Fatalf("revokeRunStaging: %v", err)
	}
	if _, gid, _ := statOwnerMode(t, dir); int(gid) != os.Getgid() {
		t.Errorf("revoked staging has gid %d, want this process's own %d", gid, os.Getgid())
	}

	outside := t.TempDir()
	link := filepath.Join(MemoryStagingBase(), "conv-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(MemoryStagingBase(), "conv-foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(foreign, foreignUID, foreignUID); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"outside the staging bases": outside,
		"two levels down":           filepath.Join(dir, "nested"),
		"a symlink":                 link,
		"not the orchestrator's":    foreign,
		"relative":                  "triagefactory-memory/conv-g",
	} {
		if err := (hostOps{}).GrantRunStaging(ctx, p, 8); err == nil {
			t.Errorf("%s: GrantRunStaging(%q) = nil error, want refusal", name, p)
		}
	}
	if _, gid, _ := statOwnerMode(t, outside); int(gid) == JailGID(8) {
		t.Error("a refused grant chgrp'd the symlink's target")
	}
}

// TestParkRunTree pins the end-of-engagement re-own: the root alone goes to
// the parked owner with its mode reset — whatever the agent left it at — and
// everything below it stays as it was; a root no jail holds, and a missing
// one, are left alone.
func TestParkRunTree(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to change owners")
	}
	ctx := context.Background()
	root := t.TempDir()
	inner := filepath.Join(root, "made-by-the-agent")
	if err := os.WriteFile(inner, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (hostOps{}).ChownRunTree(ctx, root, "", testJailUID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o777); err != nil { // the agent owned it, and opened it up
		t.Fatal(err)
	}

	if err := (hostOps{}).ParkRunTree(ctx, root); err != nil {
		t.Fatalf("ParkRunTree: %v", err)
	}
	if uid, gid, mode := statOwnerMode(t, root); int(uid) != ParkedRunTreeUID || int(gid) != os.Getgid() || mode.Perm() != 0o770 {
		t.Errorf("parked root = %d:%d %v, want %d:%d 0770", uid, gid, mode, ParkedRunTreeUID, os.Getgid())
	}
	if uid, _, _ := statOwnerMode(t, inner); int(uid) != testJailUID {
		t.Errorf("park touched an entry below the root (owner %d)", uid)
	}
	// Idempotent once parked.
	if err := (hostOps{}).ParkRunTree(ctx, root); err != nil {
		t.Errorf("second ParkRunTree: %v", err)
	}

	unheld := t.TempDir()
	if err := (hostOps{}).ParkRunTree(ctx, unheld); err != nil {
		t.Errorf("ParkRunTree on a tree no jail holds: %v", err)
	}
	if uid, _, _ := statOwnerMode(t, unheld); int(uid) != os.Getuid() {
		t.Errorf("ParkRunTree re-owned a tree no jail held (owner now %d)", uid)
	}
	if err := (hostOps{}).ParkRunTree(ctx, filepath.Join(unheld, "gone")); err != nil {
		t.Errorf("ParkRunTree on a missing tree: %v", err)
	}
	if err := (hostOps{}).ParkRunTree(ctx, "/tmp"); err == nil {
		t.Error("ParkRunTree accepted a path too shallow to be a run tree")
	}
}

// recordingOps is a PrivilegedOps double that records the order of the
// grant-release calls RunNetwork.Close makes and fails the ones it is told to.
type recordingOps struct {
	fakePrivilegedOps
	calls    *[]string
	failPark bool
}

func (r recordingOps) PrepareRunSockets(_ context.Context, _ string, _ uint8) error {
	*r.calls = append(*r.calls, "prepare")
	return nil
}
func (r recordingOps) ChownRunTree(_ context.Context, _, _ string, _ int) error {
	*r.calls = append(*r.calls, "handoff")
	return nil
}
func (r recordingOps) GrantRunStaging(_ context.Context, _ string, _ uint8) error {
	*r.calls = append(*r.calls, "grant")
	return nil
}
func (r recordingOps) ReleaseRunSockets(_ context.Context, _ string, _ uint8) error {
	*r.calls = append(*r.calls, "release-sockets")
	return nil
}
func (r recordingOps) ParkRunTree(_ context.Context, _ string) error {
	*r.calls = append(*r.calls, "park")
	if r.failPark {
		return errors.New("park failed")
	}
	return nil
}
func (r recordingOps) TeardownNetwork(_ context.Context, _ NetworkState) error {
	*r.calls = append(*r.calls, "teardown")
	return nil
}

// TestRunNetworkClose_ReleasesGrantsBeforeTheIndex pins the release rule for a
// jail identity: Close takes back every grant made under it before the index
// goes back to the allocator, and keeps the index out of the allocator when a
// grant could not be taken back.
func TestRunNetworkClose_ReleasesGrantsBeforeTheIndex(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failPark     bool
		wantReleased bool
	}{
		{"every grant released", false, true},
		{"a park fails", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			orig := defaultOps
			defaultOps = recordingOps{calls: &calls, failPark: tc.failPark}
			t.Cleanup(func() { defaultOps = orig })

			const idx = 200
			defaultAllocator().MarkInUse(idx)
			t.Cleanup(func() { defaultAllocator().Release(idx) })
			n := &RunNetwork{Idx: idx, netSt: NetworkState{}}

			ctx := context.Background()
			if err := n.PrepareSockets(ctx, "conv-close"); err != nil {
				t.Fatal(err)
			}
			if err := n.HandOffRunTree(ctx, "/tmp/runs/task"); err != nil {
				t.Fatal(err)
			}
			if err := n.GrantStaging(ctx, filepath.Join(t.TempDir(), "gone")); err != nil {
				t.Fatal(err)
			}
			_ = n.Close()

			want := []string{"prepare", "handoff", "grant", "release-sockets", "park", "teardown"}
			if len(calls) != len(want) {
				t.Fatalf("calls = %v, want %v", calls, want)
			}
			for i := range want {
				if calls[i] != want[i] {
					t.Fatalf("calls = %v, want %v", calls, want)
				}
			}
			if got := defaultAllocator().IsFree(idx); got != tc.wantReleased {
				t.Errorf("index free after Close = %v, want %v", got, tc.wantReleased)
			}

			// Closed is closed: no grant can be made under an index that may
			// already belong to another run.
			if err := n.HandOffRunTree(ctx, "/tmp/runs/task"); err == nil {
				t.Error("HandOffRunTree on a closed network = nil error")
			}
		})
	}
}
