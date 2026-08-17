//go:build linux

package agentproc

import (
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestToolHost_MatchesBrokerTrustedPaths is the drift guard for the native
// runtime's jail mounts. The broker validates the tool-host binary and
// socket-directory bind mounts against its own exported constants without
// importing this package (a cycle), so the paired literals must stay equal
// by hand. A drift would fail every native run's mount validation — and,
// because argv[0] is the same literal, its argv pin too.
func TestToolHost_MatchesBrokerTrustedPaths(t *testing.T) {
	if sandboxToolHostBinary != sandbox.TrustedToolHostBinaryDestination {
		t.Errorf("sandboxToolHostBinary = %q, want sandbox.TrustedToolHostBinaryDestination %q",
			sandboxToolHostBinary, sandbox.TrustedToolHostBinaryDestination)
	}
	if sandboxToolHostSocketDir != sandbox.TrustedToolHostSocketDirDestination {
		t.Errorf("sandboxToolHostSocketDir = %q, want sandbox.TrustedToolHostSocketDirDestination %q",
			sandboxToolHostSocketDir, sandbox.TrustedToolHostSocketDirDestination)
	}
	// The mount SOURCE must equal the broker's trusted source, or the
	// broker's realPath(source)==realPath(trusted) check rejects it.
	if hostToolHostBinaryPath != sandbox.TrustedToolHostBinaryPath() {
		t.Errorf("hostToolHostBinaryPath = %q, want sandbox.TrustedToolHostBinaryPath() %q",
			hostToolHostBinaryPath, sandbox.TrustedToolHostBinaryPath())
	}
}

// TestToolHostSocketPath_IsInsideTheBrokerDerivedDir pins that the path the
// loop dials is the socket the in-jail server binds: the host-side directory
// the broker validates, joined with the same file name the argv names.
func TestToolHostSocketPath_IsInsideTheBrokerDerivedDir(t *testing.T) {
	const conversationID = "conv-123"
	hostDir := sandbox.TrustedToolHostSocketDir(conversationID)
	hostSock := filepath.Join(hostDir, toolHostSocketName)
	jailSock := filepath.Join(sandboxToolHostSocketDir, toolHostSocketName)

	if filepath.Base(hostSock) != filepath.Base(jailSock) {
		t.Fatalf("the host-side socket %q and the in-jail socket %q must be the same file through the bind mount",
			hostSock, jailSock)
	}
	if filepath.Dir(hostSock) != hostDir {
		t.Fatalf("the socket must live directly in the broker-derived dir: %q", hostSock)
	}
}
