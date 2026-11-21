//go:build !linux

package runsidecar

import (
	"errors"
	"runtime"
)

// fdIsSocket always fails off Linux: the sidecar runs only inside the
// multi-mode executor, which is Linux-only, so there is no descriptor
// inheritance to inspect here.
func fdIsSocket(int) (bool, error) {
	return false, errors.New("shared-origin listener inheritance is not supported on " + runtime.GOOS)
}
