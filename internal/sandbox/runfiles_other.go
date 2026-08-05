//go:build !linux

package sandbox

// The per-run cell exists only where the jail does. Off Linux there is no
// socket root, no credential sidecar and no per-run files under either, so
// both entry points are no-ops — present so the cross-platform bring-up and
// teardown paths that call them compile everywhere, exactly like
// setupRunNetwork's own non-Linux stub.

// ClearRunCellFiles is a no-op off Linux.
func ClearRunCellFiles(_ string) {}

// ReleaseRunCellFiles is a no-op off Linux.
func ReleaseRunCellFiles(_ string, _ uint8) {}
