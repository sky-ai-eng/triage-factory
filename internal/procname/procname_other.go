//go:build !linux

package procname

// setTitle is a no-op off Linux: /proc/<pid>/comm is Linux-only, and
// the privsep split this legibility aid documents is Linux-only already
// (agentproc.WillSandbox).
func setTitle(name string) {}
