//go:build linux

package agenthost

import (
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestSocketPath_MatchesBrokerTrustedDerivation is the drift guard for the
// broker's per-run agenthost-socket pinning: the broker validates a
// launch's agenthost mount source against sandbox.TrustedAgentHostSocketPath
// without importing this package (a cycle: this package imports
// internal/sandbox). So the two independent implementations — this
// package's hostSocketRoot + sanitizeSocketName, and sandbox's
// trustedAgentHostSocketRoot + sanitizeRunIDForSocket — must be kept
// byte-for-byte in sync by hand; this test is what catches a future edit to
// either side falling out of step, which would otherwise surface as every
// real sandboxed run's agenthost mount being rejected by the broker.
func TestSocketPath_MatchesBrokerTrustedDerivation(t *testing.T) {
	for _, runID := range []string{
		"run-1", "itestSomeTestName", "00000000-0000-0000-0000-0000000000aa",
		"weird/../chars\x00 here!", "",
	} {
		got := filepath.Join(hostSocketRoot, sanitizeSocketName(runID)+".sock")
		want := sandbox.TrustedAgentHostSocketPath(runID)
		if got != want {
			t.Errorf("sockPath(%q) = %q, want %q (broker's trusted derivation)", runID, got, want)
		}
	}
}

// TestDefaultSocketPath_MatchesBrokerTrustedDestination guards the other
// literal duplication: the in-sandbox mount destination this package binds
// the socket at (DefaultSocketPath) must equal the broker's own
// TrustedAgentHostSocketDestination, which validateWorktreeAndMounts
// switches on to recognize this as a per-run, broker-validated mount.
func TestDefaultSocketPath_MatchesBrokerTrustedDestination(t *testing.T) {
	if DefaultSocketPath != sandbox.TrustedAgentHostSocketDestination {
		t.Errorf("DefaultSocketPath = %q, want sandbox.TrustedAgentHostSocketDestination %q", DefaultSocketPath, sandbox.TrustedAgentHostSocketDestination)
	}
}

// TestCertPath_MatchesBrokerTrustedDerivation is the same drift guard for the
// per-run gh-injector cert: the sidecar writes it (WriteInjectorCert →
// CertPathFor) and the orchestrator mounts it by that path, while the broker
// independently re-derives and validates the source via
// sandbox.TrustedGHInjectorCertPath. The two derivations must agree byte-for-
// byte or every gh-channel run's cert mount is rejected.
func TestCertPath_MatchesBrokerTrustedDerivation(t *testing.T) {
	for _, runID := range []string{
		"run-1", "itestSomeTestName", "00000000-0000-0000-0000-0000000000aa",
		"weird/../chars\x00 here!", "",
	} {
		got := CertPathFor(runID)
		want := sandbox.TrustedGHInjectorCertPath(runID)
		if got != want {
			t.Errorf("CertPathFor(%q) = %q, want %q (broker's trusted derivation)", runID, got, want)
		}
	}
}
