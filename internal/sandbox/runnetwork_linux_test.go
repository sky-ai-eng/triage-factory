//go:build linux

package sandbox

import (
	"context"
	"sync/atomic"
	"testing"
)

// recordingNetOps is a SandboxOps double that counts network setup/teardown
// and returns a populated NetworkState, so the RunNetwork ownership split —
// who builds the veth and who tears it down — is observable rather than
// assumed. It embeds fakePrivilegedOps for every other method and overrides
// only the two network ops (pointer receiver, for the atomic counters).
type recordingNetOps struct {
	fakePrivilegedOps
	setups    atomic.Int32
	teardowns atomic.Int32
}

func (r *recordingNetOps) SetupNetwork(_ context.Context, _ string, idx uint8) (NetworkState, error) {
	r.setups.Add(1)
	return NetworkState{
		Subnet:    subnetCIDR(idx),
		HostIP:    hostIP(idx),
		NetnsPath: "/run/netns/tf-test",
		NetnsName: "tf-test",
	}, nil
}

func (r *recordingNetOps) TeardownNetwork(_ context.Context, _ NetworkState) error {
	r.teardowns.Add(1)
	return nil
}

var _ SandboxOps = (*recordingNetOps)(nil)

// installRecordingNetOps swaps the recording double in for the run's lifetime
// and restores all three launcher globals on cleanup — SetPrivilegedOps
// mutates defaultOps/runLauncher/sidecarLauncher together, so leaking any one
// would break later tests in the binary (the integration suite launches real
// runs through runLauncher).
func installRecordingNetOps(t *testing.T) *recordingNetOps {
	t.Helper()
	rec := &recordingNetOps{fakePrivilegedOps: fakePrivilegedOps{tag: "rec"}}
	origOps, origRun, origSidecar := defaultOps, runLauncher, sidecarLauncher
	t.Cleanup(func() { defaultOps, runLauncher, sidecarLauncher = origOps, origRun, origSidecar })
	SetPrivilegedOps(rec)
	return rec
}

// TestSetupRunNetwork_BuildsAndReleases pins the standalone bring-up: the
// subnet index is allocated and the network built on setup, and RunNetwork.Close
// tears it down and hands the index back — idempotently.
func TestSetupRunNetwork_BuildsAndReleases(t *testing.T) {
	rec := installRecordingNetOps(t)

	net, err := SetupRunNetwork(context.Background(), "run-a")
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	if net == nil {
		t.Fatal("SetupRunNetwork returned nil network")
	}
	if got := rec.setups.Load(); got != 1 {
		t.Fatalf("SetupNetwork called %d times, want 1", got)
	}
	if net.HostIP != hostIP(net.Idx) {
		t.Fatalf("HostIP = %q, want %q", net.HostIP, hostIP(net.Idx))
	}
	if defaultAllocator().IsFree(net.Idx) {
		t.Fatalf("subnet idx %d should be taken after SetupRunNetwork", net.Idx)
	}

	if err := net.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := rec.teardowns.Load(); got != 1 {
		t.Fatalf("TeardownNetwork called %d times, want 1", got)
	}
	if !defaultAllocator().IsFree(net.Idx) {
		t.Fatalf("subnet idx %d should be free after Close", net.Idx)
	}

	// Idempotent: a second Close neither tears down again nor double-releases.
	if err := net.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := rec.teardowns.Load(); got != 1 {
		t.Fatalf("second Close tore down again (count %d)", got)
	}
}

// TestSandboxClose_RespectsNetworkOwnership is the load-bearing guarantee of
// the split: a Sandbox launched into a caller-owned network (ownsNetwork
// false) must NOT tear that network down or release its subnet index — the
// delegate's RunNetwork.Close owns that, ordered after the sidecar exits. A
// Sandbox that built its own network (ownsNetwork true, the all/local path)
// still tears down and releases on Close, unchanged.
func TestSandboxClose_RespectsNetworkOwnership(t *testing.T) {
	rec := installRecordingNetOps(t)

	// Caller-owned: Close must leave the network and the idx alone.
	const borrowedIdx = 200
	defaultAllocator().MarkInUse(borrowedIdx)
	t.Cleanup(func() { defaultAllocator().Release(borrowedIdx) })
	sbBorrowed := &Sandbox{
		RunID:     "borrowed",
		SubnetIdx: borrowedIdx,
		teardown:  &teardownState{subnetIdx: borrowedIdx, ownsNetwork: false},
	}
	if err := sbBorrowed.Close(); err != nil {
		t.Fatalf("Close (borrowed): %v", err)
	}
	if got := rec.teardowns.Load(); got != 0 {
		t.Fatalf("caller-owned Close tore the network down (count %d) — must leave it to RunNetwork.Close", got)
	}
	if defaultAllocator().IsFree(borrowedIdx) {
		t.Fatalf("caller-owned Close released idx %d out from under the owner", borrowedIdx)
	}

	// Self-built: Close tears down and releases, as before the split.
	const ownedIdx = 201
	defaultAllocator().MarkInUse(ownedIdx)
	sbOwned := &Sandbox{
		RunID:     "owned",
		SubnetIdx: ownedIdx,
		teardown:  &teardownState{subnetIdx: ownedIdx, ownsNetwork: true, netSt: NetworkState{}},
	}
	if err := sbOwned.Close(); err != nil {
		t.Fatalf("Close (owned): %v", err)
	}
	if got := rec.teardowns.Load(); got != 1 {
		t.Fatalf("self-built Close teardown count = %d, want 1", got)
	}
	if !defaultAllocator().IsFree(ownedIdx) {
		t.Fatalf("self-built Close should have released idx %d", ownedIdx)
	}
}

// TestRunNetworkClose_ReleasesIndexAfterCancelledRun pins the stop path. A stop
// cancels the run's context, and the cell teardown that follows runs entirely
// under that cancelled context — so if Close threaded it through, the brokered
// teardown would fail immediately and the subnet index would be handed back
// only if the failure path happens to release it. Both halves matter to the
// executor, not just to this run: the index is a MaxSandboxes-wide slot, and it
// is what the successor engagement's netns name and sidecar uid are derived
// from, so an index held past the stop narrows concurrency and pushes the
// successor onto a different uid than the files it inherits were written by.
func TestRunNetworkClose_ReleasesIndexAfterCancelledRun(t *testing.T) {
	rec := installRecordingNetOps(t)

	ctx, cancel := context.WithCancel(context.Background())
	net, err := SetupRunNetwork(ctx, "run-stopped")
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	cancel() // the stop

	if err := net.Close(); err != nil {
		t.Fatalf("Close after the run's context was cancelled: %v", err)
	}
	if got := rec.teardowns.Load(); got != 1 {
		t.Fatalf("TeardownNetwork called %d times after a cancelled run, want 1 — teardown must not inherit the run's cancellation", got)
	}
	if !defaultAllocator().IsFree(net.Idx) {
		t.Fatalf("subnet idx %d is still held after the stop's teardown; it must come back at teardown, not at the next boot", net.Idx)
	}
}
