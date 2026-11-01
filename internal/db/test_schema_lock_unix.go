//go:build unix

package db

import (
	"os"
	"syscall"
)

// lockImageCache blocks until it holds an exclusive advisory lock on f,
// released when f is closed or the process exits, so a binary that dies
// mid-build never leaves the others waiting.
func lockImageCache(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}
