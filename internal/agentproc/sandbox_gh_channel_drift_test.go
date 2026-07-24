//go:build linux

package agentproc

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestSandboxGHChannel_MatchesBrokerTrustedDestinations is the drift guard for
// the gh-channel mount destinations. The broker validates the gh-binary and
// injector-cert bind mounts against its own exported constants without
// importing this package (a cycle), so the paired literals — this package's
// private sandboxGHBinary / sandboxGHInjectorCert and the broker's exported
// TrustedGHBinaryDestination / TrustedGHInjectorCertDestination — must stay
// equal by hand. A drift would fail every gh-channel run's mount validation.
func TestSandboxGHChannel_MatchesBrokerTrustedDestinations(t *testing.T) {
	if sandboxTFACBinary != sandbox.TrustedTFACBinaryDestination {
		t.Errorf("sandboxTFACBinary = %q, want sandbox.TrustedTFACBinaryDestination %q", sandboxTFACBinary, sandbox.TrustedTFACBinaryDestination)
	}
	if sandboxGHBinary != sandbox.TrustedGHBinaryDestination {
		t.Errorf("sandboxGHBinary = %q, want sandbox.TrustedGHBinaryDestination %q", sandboxGHBinary, sandbox.TrustedGHBinaryDestination)
	}
	if sandboxGHInjectorCert != sandbox.TrustedGHInjectorCertDestination {
		t.Errorf("sandboxGHInjectorCert = %q, want sandbox.TrustedGHInjectorCertDestination %q", sandboxGHInjectorCert, sandbox.TrustedGHInjectorCertDestination)
	}
	// The gh-binary mount SOURCE this package uses must equal the broker's
	// trusted source, or the broker's realPath(source)==realPath(trusted) check
	// fails and every gh-channel run's binary mount is rejected.
	if hostGHBinaryPath != sandbox.TrustedGHBinaryPath() {
		t.Errorf("hostGHBinaryPath = %q, want sandbox.TrustedGHBinaryPath() %q", hostGHBinaryPath, sandbox.TrustedGHBinaryPath())
	}
}
