//go:build unix

package deploy

import (
	"os"
	"syscall"
	"testing"
)

// skipWithoutSelfInterrupt is a no-op where interruptSelf works.
func skipWithoutSelfInterrupt(*testing.T) {}

// interruptSelf sends the test process SIGINT, as Ctrl-C would.
func interruptSelf() error {
	return syscall.Kill(os.Getpid(), syscall.SIGINT)
}
