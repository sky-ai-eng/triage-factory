//go:build linux

package agentproc

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestSandboxTFBinary_MatchesBrokerTrustedDestination is the drift guard
// for the broker's global-mount pinning: the broker validates the
// TF-binary bind mount's destination against sandbox.TrustedTFBinaryDestination
// without importing this package (a cycle: this package imports
// internal/sandbox), so the two literals — this package's private
// sandboxTFBinary and the broker's exported constant — must be kept in
// sync by hand. A drift here would make every real sandboxed run's TF
// binary mount fail the broker's launch validation.
func TestSandboxTFBinary_MatchesBrokerTrustedDestination(t *testing.T) {
	if sandboxTFBinary != sandbox.TrustedTFBinaryDestination {
		t.Errorf("sandboxTFBinary = %q, want sandbox.TrustedTFBinaryDestination %q", sandboxTFBinary, sandbox.TrustedTFBinaryDestination)
	}
}
