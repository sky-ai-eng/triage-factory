//go:build linux

package sandbox

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// recordingNetOps is a SandboxOps double that counts network setup/teardown
// and returns a populated NetworkState, so the RunNetwork ownership split —
// who builds the veth and who tears it down — is observable rather than
// assumed. It embeds fakePrivilegedOps for every other method and overrides
// only the network ops (pointer receiver, for the counters). It also records
// each port admission and the state the last teardown was handed, so what a
// teardown removes is observable too.
type recordingNetOps struct {
	fakePrivilegedOps
	setups    atomic.Int32
	teardowns atomic.Int32

	mu           sync.Mutex
	admitted     [][]int
	tornDownWith NetworkState
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

func (r *recordingNetOps) TeardownNetwork(_ context.Context, state NetworkState) error {
	r.teardowns.Add(1)
	r.mu.Lock()
	r.tornDownWith = state
	r.mu.Unlock()
	return nil
}

func (r *recordingNetOps) AdmitJailPorts(_ context.Context, _ string, idx uint8, ports []int) (JailIngress, error) {
	r.mu.Lock()
	r.admitted = append(r.admitted, append([]int(nil), ports...))
	r.mu.Unlock()
	var in JailIngress
	for _, p := range ports {
		in.Rules = append(in.Rules, iptablesRule{Table: "filter", Chain: "INPUT", Args: []string{"-d", hostIP(idx), "--dport", strconv.Itoa(p), "-j", "ACCEPT"}})
	}
	return in, nil
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
		ConversationID: "borrowed",
		SubnetIdx:      borrowedIdx,
		teardown:       &teardownState{subnetIdx: borrowedIdx, ownsNetwork: false},
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
		ConversationID: "owned",
		SubnetIdx:      ownedIdx,
		teardown:       &teardownState{subnetIdx: ownedIdx, ownsNetwork: true, netSt: NetworkState{}},
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

// TestRunNetwork_AdmitJailPorts pins the admission bookkeeping: each port is
// asked for once however many launches name it, the rules the privileged side
// installed are handed to the network's teardown, and a closed network admits
// nothing.
func TestRunNetwork_AdmitJailPorts(t *testing.T) {
	rec := installRecordingNetOps(t)
	ctx := context.Background()

	net, err := SetupRunNetwork(ctx, "run-admit")
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	if err := net.AdmitJailPorts(ctx, []int{443, 34567}); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	// A relaunch into the same network asks only for what is new.
	if err := net.AdmitJailPorts(ctx, []int{34567, 40001}); err != nil {
		t.Fatalf("second admission: %v", err)
	}
	if err := net.AdmitJailPorts(ctx, []int{443}); err != nil {
		t.Fatalf("repeat admission: %v", err)
	}
	if err := net.AdmitJailPorts(ctx, []int{0}); err == nil {
		t.Error("admitted port 0")
	}

	rec.mu.Lock()
	admitted := rec.admitted
	rec.mu.Unlock()
	if want := [][]int{{443, 34567}, {40001}}; !slices.EqualFunc(admitted, want, slices.Equal[[]int]) {
		t.Fatalf("privileged admissions = %v, want %v", admitted, want)
	}

	if err := net.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rec.mu.Lock()
	rules := rec.tornDownWith.EgressRules
	rec.mu.Unlock()
	if len(rules) != 3 {
		t.Fatalf("teardown was handed %d rules, want the 3 admitted ones: %v", len(rules), rules)
	}
	if err := net.AdmitJailPorts(ctx, []int{50000}); err == nil {
		t.Error("a closed network admitted a port")
	}
}

// TestRunNetwork_AdmitJailPortsCap: the cap counts every port admitted over
// the network's life, not just the ports of one request.
func TestRunNetwork_AdmitJailPortsCap(t *testing.T) {
	installRecordingNetOps(t)
	ctx := context.Background()

	net, err := SetupRunNetwork(ctx, "run-admit-cap")
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	t.Cleanup(func() { _ = net.Close() })

	first := make([]int, MaxJailPorts)
	for i := range first {
		first[i] = 2000 + i
	}
	if err := net.AdmitJailPorts(ctx, first); err != nil {
		t.Fatalf("admission up to the cap: %v", err)
	}
	if err := net.AdmitJailPorts(ctx, []int{5000}); err == nil {
		t.Error("admitted a port past the cap")
	}
}
