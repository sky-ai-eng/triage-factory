//go:build !linux

package delegate

import "os"

// scratchChangeStamp reads Linux's stat, so elsewhere the checkpoint
// fingerprint has a scratch file's size and modification time alone.
// Checkpoints run only in multi mode, which deploys on Linux.
func scratchChangeStamp(os.FileInfo) string { return "" }
