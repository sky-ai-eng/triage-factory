//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// stageAllowedSignersFixture writes an allowed-signers file at the path the
// broker derives for conversationID, the way the sidecar does before the
// launch RPC, under a socket root redirected away from the root-owned /run/tf.
func stageAllowedSignersFixture(t *testing.T, conversationID string) string {
	t.Helper()
	restore := trustedAgentHostSocketRoot
	trustedAgentHostSocketRoot = t.TempDir()
	t.Cleanup(func() { trustedAgentHostSocketRoot = restore })
	path := TrustedAllowedSignersPath(conversationID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("stage allowed-signers dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("signer@example.com namespaces=\"git\" ssh-ed25519 AAAA\n"), 0o644); err != nil {
		t.Fatalf("write allowed-signers file: %v", err)
	}
	return path
}

func withAllowedSignersMount(p LaunchParams, source string, options ...string) LaunchParams {
	p.Mounts = append(p.Mounts, Mount{Source: source, Destination: TrustedAllowedSignersDestination, Options: options})
	return p
}

// TestValidateLaunchParams_AllowedSignersMountIsReadOnlyAndTheRunsOwn pins the
// broker's check on the allowed-signers file: this run's own file, read-only,
// is accepted; a writable or option-less mount of it is refused, since the
// jail could then list a key of its own; and another run's file is refused
// however it is mounted.
func TestValidateLaunchParams_AllowedSignersMountIsReadOnlyAndTheRunsOwn(t *testing.T) {
	p := validParams()
	own := stageAllowedSignersFixture(t, p.ConversationID)
	if err := ValidateLaunchParams(withAllowedSignersMount(p, own, "ro")); err != nil {
		t.Fatalf("rejected this run's own read-only allowed-signers file: %v", err)
	}
	for _, opts := range [][]string{{"rw"}, {"ro", "rw"}, nil} {
		if err := ValidateLaunchParams(withAllowedSignersMount(p, own, opts...)); err == nil {
			t.Errorf("accepted an allowed-signers mount with options %v; want rejection", opts)
		}
	}
	foreign := TrustedAllowedSignersPath("some-other-run")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLaunchParams(withAllowedSignersMount(p, foreign, "ro")); err == nil {
		t.Error("accepted another run's allowed-signers file")
	}
	// The CA bundle beside it is a file of the same run's, and still not this
	// one.
	ca := TrustedCABundlePath(p.ConversationID)
	if err := os.WriteFile(ca, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLaunchParams(withAllowedSignersMount(p, ca, "ro")); err == nil {
		t.Error("accepted the run's CA bundle as its allowed-signers file")
	}
}
