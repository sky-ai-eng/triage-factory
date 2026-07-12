//go:build !linux

package sandbox

import "context"

// launchSidecar on non-Linux returns ErrUnsupportedPlatform. Nothing was
// started, so there is no LaunchedSidecar to clean up — mirrors wrap's own
// non-Linux stub.
func launchSidecar(_ context.Context, _ SidecarConfig) (LaunchedSidecar, error) {
	return nil, ErrUnsupportedPlatform
}
