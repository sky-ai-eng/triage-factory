//go:build windows

package worktree

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isNameTooLong reports whether err means a path exceeds what the filesystem
// can name. Windows reports it as ERROR_FILENAME_EXCED_RANGE.
//
// ERROR_PATH_NOT_FOUND is deliberately not included: Go already maps it to
// fs.ErrNotExist, which the scratch walk treats as "removed under us" and skips
// silently. Counting it as too long would instead list entries that simply
// vanished among the unreadable ones.
func isNameTooLong(err error) bool {
	return errors.Is(err, windows.ERROR_FILENAME_EXCED_RANGE)
}
