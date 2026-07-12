//go:build linux

package sandbox

import (
	"context"
	"fmt"
)

// launchSidecar is the Linux entry point LaunchSidecar dispatches to. It
// translates the cross-platform SidecarConfig into the broker-RPC-shaped
// SidecarLaunchParams — deriving the uid/gid from the run's own subnet
// index — and routes through sidecarLauncher exactly as wrap() routes
// LaunchRun through runLauncher.
func launchSidecar(ctx context.Context, cfg SidecarConfig) (LaunchedSidecar, error) {
	if sidecarLauncher == nil {
		return nil, fmt.Errorf("sandbox: no sidecar launcher installed (cap-broker not started)")
	}
	uid := SidecarUID(cfg.SubnetIdx)
	// SidecarContainerIDSuffix keeps this key distinct from the run's own
	// ContainerID (tf-<runIDfrag>-<idx>, no suffix) in the broker's shared
	// run registry — enforced again at the RPC boundary
	// (ValidateSidecarLaunchParams), not just upheld here by convention.
	containerID := fmt.Sprintf("tf-%s-%d%s", truncate(cfg.RunID, 11), cfg.SubnetIdx, SidecarContainerIDSuffix)
	sc, err := sidecarLauncher.LaunchSidecar(ctx, SidecarLaunchParams{
		ContainerID: containerID,
		UID:         uid,
		GID:         uid,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	return sc, nil
}
