//go:build !linux

package sandbox

import (
	"context"
	"os"
)

// chownRunTree off Linux is a no-op — the sandbox path isn't reachable
// there (shouldSandbox gates on Linux), matching the runtime.GOOS check
// the previous in-caller chown had.
func chownRunTree(_ context.Context, _, _ string) error { return nil }

// removeRunTree off Linux is the plain removal the callers used before
// this seam existed — no sandbox identity ever owns these trees there.
func removeRunTree(_ context.Context, path string) error {
	return os.RemoveAll(path)
}

// captureRunDelta has no non-Linux implementation; the delegate caller
// routes to the in-process capture before ever reaching this.
func captureRunDelta(_ context.Context, _, _ string) ([]byte, error) {
	return nil, ErrUnsupportedPlatform
}

// runTreeHandedOff is always false off Linux: no sandbox identity ever takes
// ownership of a run tree there, so the orchestrator can always write into it.
func runTreeHandedOff(_ string) bool { return false }
