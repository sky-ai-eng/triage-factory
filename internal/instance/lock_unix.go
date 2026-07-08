//go:build unix

package instance

import (
	"fmt"
	"os"
	"syscall"
)

// flockEnforced reports whether flockExclusive actually enforces exclusion
// on this platform. true here (unix); lock_other.go's !unix build reports
// false, since that no-op build never blocks a second opener. Tests that
// assert cross-process lock behavior gate on this so they degrade to a
// skip, rather than a false pass or a false failure, on platforms without
// real advisory locking.
const flockEnforced = true

// flockExclusive takes a non-blocking exclusive advisory lock on f's
// underlying fd, released when f is closed (or the process exits). Returns
// ErrLocked (wrapped) when another process already holds it.
func flockExclusive(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%w: %v", ErrLocked, err)
	}
	return nil
}
