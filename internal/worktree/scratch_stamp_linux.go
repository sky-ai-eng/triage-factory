//go:build linux

package worktree

import (
	"os"
	"strconv"
	"syscall"
)

// scratchChangeStamp is a scratch file's status-change time, which the scratch
// digest takes beside its size and modification time. It cannot be set from
// user space: a tool that writes same-sized content and puts the old
// modification time back (cp -p, an archive extraction) still moves it.
//
// The inode is left out on purpose. The digest is taken in a jail, whose
// filesystem numbers inodes as it first meets files, so the same file need not
// carry the same number in two captures and a digest that read it would call
// an unchanged tree changed.
func scratchChangeStamp(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return strconv.FormatInt(st.Ctim.Nano(), 10)
}
