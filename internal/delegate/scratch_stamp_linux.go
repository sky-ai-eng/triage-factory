//go:build linux

package delegate

import (
	"os"
	"strconv"
	"syscall"
)

// scratchChangeStamp is a scratch file's inode and status-change time, which
// the checkpoint fingerprint takes beside its size and modification time.
// Neither can be set from user space: a tool that writes same-sized content
// and puts the old modification time back (cp -p, an archive extraction)
// still moves the change time, and one that replaces the file makes a new
// inode.
func scratchChangeStamp(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return strconv.FormatUint(st.Ino, 10) + "/" + strconv.FormatInt(st.Ctim.Nano(), 10)
}
