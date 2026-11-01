//go:build !unix

package db

import "os"

// lockImageCache takes no lock here: the standard library has no portable
// file lock off unix. Concurrent binaries may then each build the image,
// and the rename in writeHeadImage keeps every reader on a whole file.
func lockImageCache(_ *os.File) error {
	return nil
}
