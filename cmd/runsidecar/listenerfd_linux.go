package runsidecar

import "syscall"

// fdIsSocket reports whether fd is an open socket. A descriptor that is not
// open at all is an answer, not an error: the broker passes nothing at the
// shared-origin slot when it has no listener to share.
func fdIsSocket(fd int) (bool, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return false, nil
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFSOCK, nil
}
