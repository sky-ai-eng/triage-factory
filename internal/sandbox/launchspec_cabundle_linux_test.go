//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// stageCABundleFixture writes a CA bundle at the path the broker derives for
// conversationID, the way the sidecar does before the launch RPC, under a
// socket root redirected away from the root-owned /run/tf.
func stageCABundleFixture(t *testing.T, conversationID string) string {
	t.Helper()
	restore := trustedAgentHostSocketRoot
	trustedAgentHostSocketRoot = t.TempDir()
	t.Cleanup(func() { trustedAgentHostSocketRoot = restore })
	path := TrustedCABundlePath(conversationID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("stage CA bundle dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("-----BEGIN CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatalf("write CA bundle: %v", err)
	}
	return path
}

func withCABundleMount(p LaunchParams, source string, options ...string) LaunchParams {
	p.Mounts = append(p.Mounts, Mount{Source: source, Destination: TrustedCABundleDestination, Options: options})
	return p
}

// TestValidateLaunchParams_CABundleMountIsReadOnlyAndTheRunsOwn pins the
// broker's check on the trust file: this run's own bundle, read-only, is
// accepted; a writable or option-less mount of it is refused, since the jail
// could then rewrite what its TLS clients trust; and another run's bundle is
// refused however it is mounted.
func TestValidateLaunchParams_CABundleMountIsReadOnlyAndTheRunsOwn(t *testing.T) {
	p := validParams()
	own := stageCABundleFixture(t, p.ConversationID)
	if err := ValidateLaunchParams(withCABundleMount(p, own, "ro")); err != nil {
		t.Fatalf("rejected this run's own read-only CA bundle: %v", err)
	}
	for _, opts := range [][]string{{"rw"}, {"ro", "rw"}, nil} {
		if err := ValidateLaunchParams(withCABundleMount(p, own, opts...)); err == nil {
			t.Errorf("accepted a CA bundle mount with options %v; want rejection", opts)
		}
	}
	foreign := TrustedCABundlePath("some-other-run")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLaunchParams(withCABundleMount(p, foreign, "ro")); err == nil {
		t.Error("accepted another run's CA bundle")
	}
}
