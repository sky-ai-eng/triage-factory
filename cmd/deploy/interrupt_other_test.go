//go:build !unix

package deploy

import "errors"

// interruptSelf fails off unix: there is no way to deliver SIGINT to the
// current process there, so tests that need it cannot run.
func interruptSelf() error {
	return errors.New("sending SIGINT to the test process is not supported on this platform")
}
