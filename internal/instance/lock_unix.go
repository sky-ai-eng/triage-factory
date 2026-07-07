//go:build unix

package instance

import (
	"fmt"
	"os"
	"syscall"
)

// flockExclusive takes a non-blocking exclusive advisory lock on f's
// underlying fd, released when f is closed (or the process exits). Returns
// ErrLocked (wrapped) when another process already holds it.
func flockExclusive(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%w: %v", ErrLocked, err)
	}
	return nil
}
