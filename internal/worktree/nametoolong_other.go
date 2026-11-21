//go:build !windows

package worktree

import (
	"errors"
	"syscall"
)

// isNameTooLong reports whether err means a path exceeds what the filesystem
// can name.
func isNameTooLong(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG)
}
