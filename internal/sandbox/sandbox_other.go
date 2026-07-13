//go:build !linux

package sandbox

import (
	"context"
)

// wrap on non-Linux returns ErrUnsupportedPlatform. The agentproc
// caller gates on runtime.GOOS == "linux" so this should never fire
// in production — it's the safety net for misconfigured deployment
// or developer-machine multi-mode testing.
func wrap(_ context.Context, _ Config) (LaunchedRun, *Sandbox, error) {
	return nil, nil, ErrUnsupportedPlatform
}

// Close on non-Linux is a no-op (nothing was created). Defensive:
// callers may defer Close() even when Wrap errored out, and the
// non-Linux path needs to be safe to call against a nil-zero
// Sandbox.
func (s *Sandbox) Close() error {
	return nil
}

// setupRunNetwork on non-Linux returns ErrUnsupportedPlatform, mirroring
// wrap — the credential-isolation path this supports is multi-mode + Linux
// only. The delegate caller gates on GOOS before reaching here.
func setupRunNetwork(_ context.Context, _ string) (*RunNetwork, error) {
	return nil, ErrUnsupportedPlatform
}

// Close on non-Linux is a no-op — setupRunNetwork never returns a non-nil
// RunNetwork off Linux, so this only guards a defensive defer.
func (n *RunNetwork) Close() error {
	return nil
}
