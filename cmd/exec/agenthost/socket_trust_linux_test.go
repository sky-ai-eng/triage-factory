//go:build linux

package agenthost

import (
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestSocketPath_MatchesBrokerTrustedDerivation is the drift guard for the
// broker's per-run agenthost-socket pinning: the broker validates a
// launch's agenthost mount source against sandbox.TrustedAgentHostSocketPath,
// and creates the directory it lives in at sandbox.RunSocketDir, without
// importing this package (a cycle: this package imports internal/sandbox).
// So the two independent implementations — this package's hostSocketRoot +
// sanitizeSocketName + file names, and sandbox's — must be kept byte-for-byte
// in sync by hand; this test is what catches a future edit to either side
// falling out of step, which would otherwise surface as every real sandboxed
// run's sidecar failing to create its socket, or its mount being rejected.
func TestSocketPath_MatchesBrokerTrustedDerivation(t *testing.T) {
	for _, conversationID := range []string{
		"conv-1", "itestSomeTestName", "00000000-0000-0000-0000-0000000000aa",
		"weird/../chars\x00 here!", "",
	} {
		if got, want := SocketPathFor(conversationID), sandbox.TrustedAgentHostSocketPath(conversationID); got != want {
			t.Errorf("SocketPathFor(%q) = %q, want %q (broker's trusted derivation)", conversationID, got, want)
		}
		if got, want := runSocketDir(conversationID), sandbox.RunSocketDir(conversationID); got != want {
			t.Errorf("runSocketDir(%q) = %q, want %q (the directory the broker creates)", conversationID, got, want)
		}
		if got := filepath.Dir(SocketPathFor(conversationID)); got != runSocketDir(conversationID) {
			t.Errorf("SocketPathFor(%q) is not inside the run's socket directory %q", conversationID, runSocketDir(conversationID))
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
	for _, conversationID := range []string{
		"conv-1", "itestSomeTestName", "00000000-0000-0000-0000-0000000000aa",
		"weird/../chars\x00 here!", "",
	} {
		got := CertPathFor(conversationID)
		want := sandbox.TrustedGHInjectorCertPath(conversationID)
		if got != want {
			t.Errorf("CertPathFor(%q) = %q, want %q (broker's trusted derivation)", conversationID, got, want)
		}
	}
}
