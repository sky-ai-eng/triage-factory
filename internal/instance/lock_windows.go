//go:build windows

package instance

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// flockEnforced is true here: LockFileEx is mandatory, so a second opener is
// refused. See lock_unix.go.
const flockEnforced = true

// lockOffset is where the one locked byte sits, far past the end of the
// 36-byte id file. Windows byte-range locks are mandatory: they block reads
// and writes of the locked range through every other handle, so locking
// offset 0 would make the id unreadable to anything but the holder, such as
// an operator inspecting it or a second process reading it to report why it
// is being refused.
const lockOffset = 1 << 30

// flockExclusive takes a non-blocking exclusive lock on one byte of f at
// lockOffset, released when f is closed (or the process exits). Returns
// ErrLocked (wrapped) when another handle already holds it.
func flockExclusive(f *os.File) error {
	ol := &windows.Overlapped{
		Offset:     lockOffset & 0xFFFFFFFF,
		OffsetHigh: lockOffset >> 32,
	}
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol,
	)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return fmt.Errorf("%w: %v", ErrLocked, err)
	}
	return fmt.Errorf("lock %s: %w", f.Name(), err)
}
