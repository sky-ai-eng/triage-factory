//go:build !unix

package instance

import "os"

// flockEnforced is false here: this build's flockExclusive never blocks a
// second opener, so cross-process lock tests skip rather than assert
// against a guarantee this platform doesn't provide. See lock_unix.go.
const flockEnforced = false

// flockExclusive on non-unix platforms is a no-op: syscall.Flock has no
// portable Windows equivalent in the standard library, and the two-process
// guard degrades gracefully rather than blocking boot — the worst case is
// the pre-existing behavior (two processes could already race the same
// SQLite file on Windows before this package existed). Every real
// deployment target (Linux executors/control pods, macOS/Linux dev) goes
// through lock_unix.go instead.
func flockExclusive(_ *os.File) error {
	return nil
}
