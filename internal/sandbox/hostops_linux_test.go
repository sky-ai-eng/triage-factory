//go:build linux

package sandbox

import (
	"context"
	"errors"
	"testing"
)

// fakePrivilegedOps is a minimal SandboxOps double distinguishable from
// hostOps by its EnsureRootfs return value, so SetPrivilegedOps's effect on
// defaultOps is observable rather than assumed. It also implements the
// RunLauncher half so it satisfies the SandboxOps that SetPrivilegedOps
// installs.
type fakePrivilegedOps struct{ tag string }

func (f fakePrivilegedOps) SetupNetwork(ctx context.Context, conversationID string, subnetIdx uint8) (NetworkState, error) {
	return NetworkState{}, nil
}
func (f fakePrivilegedOps) TeardownNetwork(ctx context.Context, state NetworkState) error {
	return nil
}
func (f fakePrivilegedOps) AdmitJailPorts(ctx context.Context, conversationID string, subnetIdx uint8, ports []int) error {
	return nil
}
func (f fakePrivilegedOps) EnsureRootfs(ctx context.Context, selector RootfsSelector) (string, error) {
	return f.tag, nil
}
func (f fakePrivilegedOps) LaunchRun(ctx context.Context, p LaunchParams) (LaunchedRun, error) {
	// Present only to satisfy SandboxOps (SetPrivilegedOps installs this as
	// both defaultOps and runLauncher). No test drives a launch through it;
	// return an error rather than a nil LaunchedRun so an accidental use
	// fails loudly instead of nil-panicking in the caller.
	return nil, errors.New("fakePrivilegedOps.LaunchRun: not implemented for this test double")
}
func (f fakePrivilegedOps) LaunchSidecar(ctx context.Context, p SidecarLaunchParams) (LaunchedSidecar, error) {
	// Present only to satisfy SandboxOps (SetPrivilegedOps installs this as
	// both defaultOps and sidecarLauncher too). Same not-implemented
	// contract as LaunchRun above.
	return nil, errors.New("fakePrivilegedOps.LaunchSidecar: not implemented for this test double")
}
func (f fakePrivilegedOps) ReapOrphans(ctx context.Context) error { return nil }
func (f fakePrivilegedOps) ChownRunTree(ctx context.Context, root, subpath string, jailUID int) error {
	return nil
}
func (f fakePrivilegedOps) ParkRunTree(ctx context.Context, root string, jailUID int) error {
	return nil
}
func (f fakePrivilegedOps) PrepareRunSockets(ctx context.Context, conversationID string, subnetIdx uint8) error {
	return nil
}
func (f fakePrivilegedOps) ReleaseRunSockets(ctx context.Context, conversationID string, subnetIdx uint8) error {
	return nil
}
func (f fakePrivilegedOps) GrantRunStaging(ctx context.Context, dir string, subnetIdx uint8) error {
	return nil
}
func (f fakePrivilegedOps) RemoveRunTree(ctx context.Context, path string) error { return nil }
func (f fakePrivilegedOps) CaptureRunTree(ctx context.Context, p CaptureParams) ([]byte, error) {
	return nil, nil
}

var _ SandboxOps = fakePrivilegedOps{}

// TestSetPrivilegedOps_NilPanics pins the defensive nil guard: a nil
// defaultOps would otherwise fail confusingly later, as a nil-interface
// panic deep inside the first real Wrap, instead of at the misconfigured
// call site.
func TestSetPrivilegedOps_NilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected SetPrivilegedOps(nil) to panic")
		}
	}()
	SetPrivilegedOps(nil)
}

// TestSetPrivilegedOps_ReplacesDefault confirms the swap actually takes
// effect — defaultOps routes to the new implementation, not silently to
// the original hostOps{}.
func TestSetPrivilegedOps_ReplacesDefault(t *testing.T) {
	// SetPrivilegedOps mutates all three launcher globals, so restore all
	// three — otherwise the fake run/sidecar launcher leaks into every later
	// test in the binary (the integration suite launches real runs through
	// runLauncher and would fail on the fake's not-implemented stub).
	origOps, origRun, origSidecar := defaultOps, runLauncher, sidecarLauncher
	t.Cleanup(func() { defaultOps, runLauncher, sidecarLauncher = origOps, origRun, origSidecar })

	SetPrivilegedOps(fakePrivilegedOps{tag: "swapped"})
	path, err := defaultOps.EnsureRootfs(context.Background(), RootfsSelector{})
	if err != nil {
		t.Fatalf("EnsureRootfs: %v", err)
	}
	if path != "swapped" {
		t.Errorf("defaultOps.EnsureRootfs = %q, want the fake's sentinel — SetPrivilegedOps did not take effect", path)
	}
}

// TestHostOpsAdmitJailPorts_RefusesUnknownNetworks: the privileged side opens
// ports only on a network it built for the conversation asking, and only on a
// veth that exists. Both refusals come before any iptables call, so this runs
// without root.
func TestHostOpsAdmitJailPorts_RefusesUnknownNetworks(t *testing.T) {
	ctx := context.Background()
	const idx = 213

	if err := (hostOps{}).AdmitJailPorts(ctx, "conv-never-built", idx, []int{40001}); err == nil {
		t.Error("admitted ports on a network this process never built")
	}

	// A network live for one conversation does not admit another's request.
	name := NetnsNameForRun("conv-owner", idx)
	markNetnsLive(name, "conv-owner")
	t.Cleanup(func() { forgetNetns(name) })
	if err := (hostOps{}).AdmitJailPorts(ctx, "conv-someone-else", idx, []int{40001}); err == nil {
		t.Error("admitted ports on another conversation's network")
	}

	// The owner's request, on a network whose veth does not exist here.
	if err := (hostOps{}).AdmitJailPorts(ctx, "conv-owner", idx, []int{40001}); err == nil {
		t.Error("admitted ports on a veth that does not exist")
	}
}

// TestTeardownTarget_AcceptsWhatSetupReports: the state SetupNetwork reports
// for a network, whole, is the one state TeardownNetwork acts on, and the
// network it acts on is that one.
func TestTeardownTarget_AcceptsWhatSetupReports(t *testing.T) {
	const idx = 9
	built := runNetNames(netnsIDFrag("conv-a"), idx)
	netSt, gotIdx, err := teardownTarget(networkStateFrom(built, idx))
	if err != nil {
		t.Fatalf("teardownTarget refused the state setup reports: %v", err)
	}
	if gotIdx != idx || *netSt != *built {
		t.Errorf("teardownTarget = %+v at %d, want %+v at %d", *netSt, gotIdx, *built, idx)
	}
	if netSt, _, err := teardownTarget(NetworkState{}); err != nil || netSt != nil {
		t.Errorf("a zero state, which a setup that built nothing reports, = %v, %v; want nothing to remove", netSt, err)
	}
}

// TestTeardownTarget_RefusesStatesSetupDidNotBuild: the state is the caller's,
// so a teardown whose names are not exactly what SetupNetwork builds is
// refused. TeardownNetwork refuses on this before any `ip` or `iptables` call;
// the cases run against teardownTarget rather than TeardownNetwork, so a
// regression run as root fails here instead of deleting the host's eth0.
func TestTeardownTarget_RefusesStatesSetupDidNotBuild(t *testing.T) {
	const idx = 9
	good := networkStateFrom(runNetNames(netnsIDFrag("conv-a"), idx), idx)
	other := networkStateFrom(runNetNames(netnsIDFrag("conv-b"), idx+1), idx+1)
	with := func(f func(*NetworkState)) NetworkState {
		s := good
		f(&s)
		return s
	}
	for name, state := range map[string]NetworkState{
		"a host interface":               with(func(s *NetworkState) { s.VethHost = "eth0" }),
		"another network's veth":         with(func(s *NetworkState) { s.VethHost = other.VethHost }),
		"another network's gateway":      with(func(s *NetworkState) { s.HostIP = other.HostIP }),
		"another network's subnet":       with(func(s *NetworkState) { s.Subnet = other.Subnet }),
		"a namespace setup never builds": {NetnsName: "default"},
		"a path out of the netns dir":    {NetnsName: "../../etc/tf-deadbeef-9"},
		"a non-canonical index":          with(func(s *NetworkState) { s.NetnsName = "tf-" + netnsIDFrag("conv-a") + "-009" }),
		"a veth with no namespace":       {VethHost: "eth0"},
	} {
		if _, _, err := teardownTarget(state); err == nil {
			t.Errorf("%s: teardownTarget accepted %+v", name, state)
		}
	}
}

// TestArmJailNetwork_RefusesUnknownNetworks: the jail side is re-armed only
// on a namespace shaped and named for the conversation asking, and only on a
// network this process built for it. Every refusal comes before any ip call,
// so this runs without root.
func TestArmJailNetwork_RefusesUnknownNetworks(t *testing.T) {
	ctx := context.Background()
	const idx = 214

	if err := ArmJailNetwork(ctx, "conv-owner", "/tmp/not-a-netns"); err == nil {
		t.Error("armed a namespace outside /var/run/netns")
	}
	if err := ArmJailNetwork(ctx, "conv-never-built", "/var/run/netns/"+NetnsNameForRun("conv-never-built", idx)); err == nil {
		t.Error("armed a network this process never built")
	}

	name := NetnsNameForRun("conv-owner", idx)
	markNetnsLive(name, "conv-owner")
	t.Cleanup(func() { forgetNetns(name) })
	if err := ArmJailNetwork(ctx, "conv-someone-else", "/var/run/netns/"+name); err == nil {
		t.Error("armed another conversation's network")
	}
}
