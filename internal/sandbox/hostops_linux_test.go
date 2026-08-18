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
func (f fakePrivilegedOps) ChownRunTree(ctx context.Context, root, subpath string) error {
	return nil
}
func (f fakePrivilegedOps) RemoveRunTree(ctx context.Context, path string) error { return nil }
func (f fakePrivilegedOps) CaptureRunDelta(ctx context.Context, worktree, sessionID string) ([]byte, error) {
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
