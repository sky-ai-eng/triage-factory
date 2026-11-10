//go:build !unix && !windows

package instance

import "os"

// flockEnforced is false here: this build's flockExclusive never blocks a
// second opener, so cross-process lock tests skip rather than assert
// against a guarantee this platform doesn't provide. See lock_unix.go.
const flockEnforced = false

// flockExclusive on platforms with neither flock nor LockFileEx is a no-op:
// the two-process guard degrades gracefully rather than blocking boot. Every
// supported target goes through lock_unix.go or lock_windows.go instead.
func flockExclusive(_ *os.File) error {
	return nil
}
