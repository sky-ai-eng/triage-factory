//go:build linux

package sandbox

import (
	"context"
	"os"
	"testing"
)

// fakePrivilegedOps is a minimal PrivilegedOps double distinguishable from
// hostOps by its EnsureRootfs return value, so SetPrivilegedOps's effect on
// defaultOps is observable rather than assumed.
type fakePrivilegedOps struct{ tag string }

func (f fakePrivilegedOps) SetupNetwork(ctx context.Context, runID string, subnetIdx uint8) (NetworkState, error) {
	return NetworkState{}, nil
}
func (f fakePrivilegedOps) TeardownNetwork(ctx context.Context, state NetworkState) error {
	return nil
}
func (f fakePrivilegedOps) EnsureRootfs(ctx context.Context, selector RootfsSelector) (string, error) {
	return f.tag, nil
}
func (f fakePrivilegedOps) SetupRunCgroup(name string, limitMB int) (string, *os.File, error) {
	return "", nil, nil
}
func (f fakePrivilegedOps) LaunchRun(ctx context.Context, p LaunchParams) (LaunchedRun, error) {
	return nil, nil
}
func (f fakePrivilegedOps) RemoveRunCgroup(dir string) error      { return nil }
func (f fakePrivilegedOps) ReapOrphans(ctx context.Context) error { return nil }

var _ PrivilegedOps = fakePrivilegedOps{}

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
	orig := defaultOps
	t.Cleanup(func() { defaultOps = orig })

	SetPrivilegedOps(fakePrivilegedOps{tag: "swapped"})
	path, err := defaultOps.EnsureRootfs(context.Background(), RootfsSelector{})
	if err != nil {
		t.Fatalf("EnsureRootfs: %v", err)
	}
	if path != "swapped" {
		t.Errorf("defaultOps.EnsureRootfs = %q, want the fake's sentinel — SetPrivilegedOps did not take effect", path)
	}
}
