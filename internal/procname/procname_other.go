//go:build !linux

package procname

// setTitle is a no-op off Linux: PR_SET_NAME is a Linux-only prctl, and
// the privsep split this legibility aid documents is Linux-only already
// (agentproc.WillSandbox).
func setTitle(name string) {}
