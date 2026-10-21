//go:build linux

package agentproc

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestSandboxGHChannel_MatchesBrokerTrustedDestinations is the drift guard for
// the TF-side mount destinations. The broker validates the gh-binary and CA
// bundle bind mounts against its own exported constants without importing
// this package (a cycle), so the paired literals — this package's private
// sandboxGHBinary / sandboxCABundle and the broker's exported
// TrustedGHBinaryDestination / TrustedCABundleDestination — must stay equal by
// hand. A drift would fail every launch's mount validation.
func TestSandboxGHChannel_MatchesBrokerTrustedDestinations(t *testing.T) {
	if sandboxTFACBinary != sandbox.TrustedTFACBinaryDestination {
		t.Errorf("sandboxTFACBinary = %q, want sandbox.TrustedTFACBinaryDestination %q", sandboxTFACBinary, sandbox.TrustedTFACBinaryDestination)
	}
	if sandboxGHBinary != sandbox.TrustedGHBinaryDestination {
		t.Errorf("sandboxGHBinary = %q, want sandbox.TrustedGHBinaryDestination %q", sandboxGHBinary, sandbox.TrustedGHBinaryDestination)
	}
	if sandboxCABundle != sandbox.TrustedCABundleDestination {
		t.Errorf("sandboxCABundle = %q, want sandbox.TrustedCABundleDestination %q", sandboxCABundle, sandbox.TrustedCABundleDestination)
	}
	// The gh-binary mount SOURCE this package uses must equal the broker's
	// trusted source, or the broker's realPath(source)==realPath(trusted) check
	// fails and every gh-channel run's binary mount is rejected.
	if hostGHBinaryPath != sandbox.TrustedGHBinaryPath() {
		t.Errorf("hostGHBinaryPath = %q, want sandbox.TrustedGHBinaryPath() %q", hostGHBinaryPath, sandbox.TrustedGHBinaryPath())
	}
}
