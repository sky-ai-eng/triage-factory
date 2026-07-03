//go:build !linux

package sandbox

import (
	"context"
	"os/exec"
)

// wrap on non-Linux returns ErrUnsupportedPlatform. The agentproc
// caller gates on runtime.GOOS == "linux" so this should never fire
// in production — it's the safety net for misconfigured deployment
// or developer-machine multi-mode testing.
func wrap(_ context.Context, _ Config) (*exec.Cmd, *Sandbox, error) {
	return nil, nil, ErrUnsupportedPlatform
}

// Close on non-Linux is a no-op (nothing was created). Defensive:
// callers may defer Close() even when Wrap errored out, and the
// non-Linux path needs to be safe to call against a nil-zero
// Sandbox.
func (s *Sandbox) Close() error {
	return nil
}

// OOMKilled on non-Linux is always false — no sandbox, no cgroup.
func (s *Sandbox) OOMKilled() bool {
	return false
}
