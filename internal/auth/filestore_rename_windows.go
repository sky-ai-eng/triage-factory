//go:build windows

package auth

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isTransientRenameErr reports whether a rename failed because another process
// briefly holds the destination open: ERROR_SHARING_VIOLATION or
// ERROR_ACCESS_DENIED, which MoveFileEx returns for a replace-onto-open-file.
func isTransientRenameErr(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
