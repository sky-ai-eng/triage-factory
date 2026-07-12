//go:build linux

package sandbox

import (
	"context"
	"strings"
	"testing"
)

// fakeSidecarLauncher is a scriptable SidecarLauncher double so
// launchSidecar's translation from SidecarConfig to SidecarLaunchParams can
// be pinned without a real broker.
type fakeSidecarLauncher struct {
	gotParams SidecarLaunchParams
	err       error
}

func (f *fakeSidecarLauncher) LaunchSidecar(ctx context.Context, p SidecarLaunchParams) (LaunchedSidecar, error) {
	f.gotParams = p
	if f.err != nil {
		return nil, f.err
	}
	return noopLaunchedSidecar{}, nil
}

type noopLaunchedSidecar struct{}

func (noopLaunchedSidecar) Close() error { return nil }

func withFakeSidecarLauncher(t *testing.T, f *fakeSidecarLauncher) {
	t.Helper()
	orig := sidecarLauncher
	sidecarLauncher = f
	t.Cleanup(func() { sidecarLauncher = orig })
}

// TestLaunchSidecar_DerivesUIDFromSubnetIdx pins the translation LaunchSidecar
// performs before crossing the broker RPC: the uid/gid the broker will be
// asked to setuid to comes from SidecarUID(cfg.SubnetIdx), the SAME index
// Wrap allocated for this run's own netns — never a value invented here.
func TestLaunchSidecar_DerivesUIDFromSubnetIdx(t *testing.T) {
	fake := &fakeSidecarLauncher{}
	withFakeSidecarLauncher(t, fake)

	if _, err := LaunchSidecar(context.Background(), SidecarConfig{RunID: "run-abc123", SubnetIdx: 7}); err != nil {
		t.Fatalf("LaunchSidecar: %v", err)
	}

	want := SidecarUID(7)
	if fake.gotParams.UID != want || fake.gotParams.GID != want {
		t.Errorf("uid/gid = %d/%d, want %d/%d", fake.gotParams.UID, fake.gotParams.GID, want, want)
	}
	if fake.gotParams.ContainerID == "" {
		t.Error("empty sidecar container id")
	}
}

// TestLaunchSidecar_ContainerIDDistinctFromRunContainerID pins that the
// sidecar's registry key can never collide with the run's own — both live
// in the SAME broker-side map (cmd/capbroker's s.runs), keyed by
// ContainerID, so a shared key would let one silently clobber the other.
func TestLaunchSidecar_ContainerIDDistinctFromRunContainerID(t *testing.T) {
	fake := &fakeSidecarLauncher{}
	withFakeSidecarLauncher(t, fake)

	const runID = "run-xyz"
	const idx = 3
	if _, err := LaunchSidecar(context.Background(), SidecarConfig{RunID: runID, SubnetIdx: idx}); err != nil {
		t.Fatalf("LaunchSidecar: %v", err)
	}

	runContainerID := "tf-" + truncate(runID, 11) + "-3" // mirrors wrap()'s own naming
	if fake.gotParams.ContainerID == runContainerID {
		t.Errorf("sidecar container id %q collides with the run's own %q", fake.gotParams.ContainerID, runContainerID)
	}
	if !strings.HasSuffix(fake.gotParams.ContainerID, "-sc") {
		t.Errorf("sidecar container id %q missing the -sc suffix", fake.gotParams.ContainerID)
	}
}

// TestLaunchSidecar_NoLauncherInstalledErrors pins the boot-order guard:
// reaching LaunchSidecar before SetPrivilegedOps installed the broker
// client is a boot-order bug that must fail loudly, exactly like wrap()'s
// own nil-runLauncher check.
func TestLaunchSidecar_NoLauncherInstalledErrors(t *testing.T) {
	orig := sidecarLauncher
	sidecarLauncher = nil
	t.Cleanup(func() { sidecarLauncher = orig })

	if _, err := LaunchSidecar(context.Background(), SidecarConfig{RunID: "run-x", SubnetIdx: 0}); err == nil {
		t.Fatal("expected an error when no sidecar launcher is installed")
	}
}
