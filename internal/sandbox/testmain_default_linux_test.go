//go:build linux && !integration

package sandbox

import (
	"os"
	"testing"
)

// TestMain for the default (non-integration) sandbox test binary. It only
// has to run the sidecar-reap re-exec helper before the suite; the
// integration build tag carries its own TestMain that additionally
// pre-warms the rootfs and installs the in-process launcher. Splitting them
// on !integration / integration is what keeps exactly one TestMain compiled
// into each binary — both `go test ./internal/sandbox` and
// `go test -tags integration ./internal/sandbox` build cleanly.
func TestMain(m *testing.M) {
	reExecAsSidecarHelperIfRequested()
	os.Exit(m.Run())
}
