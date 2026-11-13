//go:build !unix

package deploy

import (
	"errors"
	"testing"
)

// skipWithoutSelfInterrupt skips a test that needs interruptSelf.
func skipWithoutSelfInterrupt(t *testing.T) {
	t.Helper()
	t.Skip("cannot deliver SIGINT to the test process on this platform")
}

// interruptSelf fails off unix: there is no way to deliver SIGINT to the
// current process there, so tests that need it cannot run.
func interruptSelf() error {
	return errors.New("sending SIGINT to the test process is not supported on this platform")
}
