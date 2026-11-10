//go:build unix

package deploy

import (
	"os"
	"syscall"
)

// interruptSelf sends the test process SIGINT, as Ctrl-C would.
func interruptSelf() error {
	return syscall.Kill(os.Getpid(), syscall.SIGINT)
}
