// Package fsutil holds small file-system helpers shared across packages.
package fsutil

import (
	"os"
	"time"
)

// renameAttempts and renameRetryDelay bound the retry of a rename the OS
// refused for a transient reason (see isTransientRenameErr): five tries, about
// 250ms in total.
const (
	renameAttempts   = 5
	renameRetryDelay = 60 * time.Millisecond
)

// renameFile and renameTransient are vars so a test can inject failures.
var (
	renameFile      = os.Rename
	renameTransient = isTransientRenameErr
)

// RenameReplace moves oldPath over newPath, retrying briefly when the failure
// is one a short wait can clear. On Windows a rename onto a file another
// process has open (a virus scanner, an indexer, a concurrent reader) fails
// with a sharing violation or access denied and succeeds a moment later; no
// other platform reports transient errors, so there it is a plain rename.
// The last error is returned unchanged.
func RenameReplace(oldPath, newPath string) error {
	var err error
	for attempt := 0; attempt < renameAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(renameRetryDelay)
		}
		if err = renameFile(oldPath, newPath); err == nil || !renameTransient(err) {
			return err
		}
	}
	return err
}
