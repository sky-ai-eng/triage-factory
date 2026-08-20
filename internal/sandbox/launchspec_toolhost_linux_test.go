//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/githooks"
)

// ensureToolHostFixtures points the broker's trusted tool-host binary path at
// a stub a non-root `go test` user can create, and materializes the run's
// socket directory. Both are restored on cleanup.
func ensureToolHostFixtures(t *testing.T, conversationID string) (binPath, sockDir string) {
	t.Helper()

	origBin := trustedToolHostBinaryPath
	binPath = filepath.Join(t.TempDir(), "tf-harness-tools")
	if err := os.WriteFile(binPath, []byte("stub"), 0o755); err != nil {
		t.Fatalf("write tool host stub: %v", err)
	}
	trustedToolHostBinaryPath = binPath
	t.Cleanup(func() { trustedToolHostBinaryPath = origBin })

	origRoot := trustedAgentHostSocketRoot
	trustedAgentHostSocketRoot = t.TempDir()
	t.Cleanup(func() { trustedAgentHostSocketRoot = origRoot })

	sockDir = TrustedToolHostSocketDir(conversationID)
	if err := os.MkdirAll(sockDir, 0o770); err != nil {
		t.Fatalf("mkdir tool host socket dir: %v", err)
	}
	return binPath, sockDir
}

func toolHostArgv() []string {
	return []string{
		TrustedToolHostBinaryDestination, "serve",
		"--socket", filepath.Join(TrustedToolHostSocketDirDestination, "tools.sock"),
		"--cwd", "/work",
	}
}

// TestValidateLaunchParams_AcceptsToolHostEntrypoint pins that the native
// runtime's pinned entrypoint passes the broker's argv gate, with its
// arguments (socket path, cwd) free to vary — the same latitude the SDK
// entrypoint's wrapper arguments have.
func TestValidateLaunchParams_AcceptsToolHostEntrypoint(t *testing.T) {
	p := validParams()
	p.Args = toolHostArgv()
	if err := ValidateLaunchParams(p); err != nil {
		t.Fatalf("rejected the pinned tool-host entrypoint: %v", err)
	}
}

// TestValidateLaunchParams_RejectsToolHostNonServeVerb pins that only the
// resident server may be a jail's main process: the one-shot CLI and the
// definitions dump are the same binary, and pinning the program alone would
// let a compromised orchestrator start either.
func TestValidateLaunchParams_RejectsToolHostNonServeVerb(t *testing.T) {
	for _, argv := range [][]string{
		{TrustedToolHostBinaryDestination, "bash", `{"command":"id"}`},
		{TrustedToolHostBinaryDestination, "--definitions"},
		{TrustedToolHostBinaryDestination},
	} {
		p := validParams()
		p.Args = argv
		if err := ValidateLaunchParams(p); err == nil {
			t.Errorf("accepted tool-host argv %v; only the serve verb may be launched", argv)
		}
	}
}

// TestValidateLaunchParams_AcceptsToolHostMountSet is the native runtime's
// equivalent of the realistic-mount-set case: the mounts a native jail
// actually assembles are accepted unchanged.
func TestValidateLaunchParams_AcceptsToolHostMountSet(t *testing.T) {
	ensureGitHooksFixture(t)
	tfBin, err := TrustedTFBinaryPath()
	if err != nil {
		t.Fatalf("resolve trusted TF binary path: %v", err)
	}
	p := validParams()
	binPath, sockDir := ensureToolHostFixtures(t, p.ConversationID)
	p.Args = toolHostArgv()
	p.Mounts = []Mount{
		{Source: binPath, Destination: TrustedToolHostBinaryDestination, Options: []string{"ro"}},
		{Source: sockDir, Destination: TrustedToolHostSocketDirDestination},
		{Source: tfBin, Destination: TrustedTFBinaryDestination, Options: []string{"ro"}},
		{Source: TrustedGitHooksDir(), Destination: githooks.SandboxDir, Options: []string{"ro"}},
	}
	if err := ValidateLaunchParams(p); err != nil {
		t.Fatalf("rejected a native run's real mount set: %v", err)
	}
}

// TestValidateLaunchParams_RejectsForeignToolHostSources is the negative
// half: neither the binary nor the socket directory may come from anywhere
// but the broker's own derivation — including a sibling run's directory.
func TestValidateLaunchParams_RejectsForeignToolHostSources(t *testing.T) {
	p := validParams()
	_, sockDir := ensureToolHostFixtures(t, p.ConversationID)
	p.Args = toolHostArgv()

	attackerBin := filepath.Join(t.TempDir(), "attacker-tools")
	if err := os.WriteFile(attackerBin, []byte("evil"), 0o755); err != nil {
		t.Fatalf("write attacker binary: %v", err)
	}
	bad := p
	bad.Mounts = []Mount{{Source: attackerBin, Destination: TrustedToolHostBinaryDestination, Options: []string{"ro"}}}
	if err := ValidateLaunchParams(bad); err == nil {
		t.Error("accepted a foreign source for the tool-host binary; want rejection")
	}

	siblingDir := TrustedToolHostSocketDir("some-other-run")
	if err := os.MkdirAll(siblingDir, 0o770); err != nil {
		t.Fatalf("mkdir sibling socket dir: %v", err)
	}
	bad = p
	bad.Mounts = []Mount{{Source: siblingDir, Destination: TrustedToolHostSocketDirDestination}}
	if err := ValidateLaunchParams(bad); err == nil {
		t.Error("accepted a sibling run's tool-host socket dir; want rejection")
	}

	// The run's own directory passes.
	ok := p
	ok.Mounts = []Mount{{Source: sockDir, Destination: TrustedToolHostSocketDirDestination}}
	if err := ValidateLaunchParams(ok); err != nil {
		t.Errorf("rejected the run's own tool-host socket dir: %v", err)
	}
}
