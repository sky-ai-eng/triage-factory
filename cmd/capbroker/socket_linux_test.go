//go:build linux

package capbroker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestListen_Permissions pins the socket hygiene invariant: 01731 dir
// (traversable by any uid via a known path, group-writable so a run's
// credential sidecar can create its own socket here, but not listable, and
// STICKY so no sidecar can delete/replace another entry — see listen's doc),
// 0600 socket file. The dir's group chgrp to WorktreeGID only lands under a
// real root broker; this test runs unprivileged (so the chgrp EPERMs and is
// tolerated), which is why it asserts the mode, not the group. Uses an isolated
// temp directory rather than the production socketDir (/run/tf, root-only on
// most distros) so this runs on an unprivileged CI runner too.
func TestListen_Permissions(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "tf-sock-dir", "test-hygiene.sock")

	l, err := listen(sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	dirInfo, err := os.Stat(filepath.Dir(sockPath))
	if err != nil {
		t.Fatalf("stat socket dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o731 {
		t.Errorf("socket dir mode = %o, want 0731", got)
	}
	// Mode().Perm() masks the sticky bit, so assert it separately: without it,
	// group-write + the shared WorktreeGID would let one run's sidecar unlink or
	// replace the broker's or a sibling run's socket.
	if dirInfo.Mode()&os.ModeSticky == 0 {
		t.Errorf("socket dir mode = %v, want the sticky bit set (cross-run unlink protection)", dirInfo.Mode())
	}

	fileInfo, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat socket file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("socket file mode = %o, want 0600", got)
	}
}

// TestListen_RemovesStaleSocket pins that a leftover socket file from a
// previous crashed process doesn't EADDRINUSE the next listen.
func TestListen_RemovesStaleSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "tf-sock-dir", "test-stale.sock")

	l1, err := listen(sockPath)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	_ = l1.Close() // socket file remains on disk, listener is gone

	l2, err := listen(sockPath)
	if err != nil {
		t.Fatalf("second listen (stale socket present): %v", err)
	}
	defer l2.Close()
}

// TestListen_RefusesNonSocketFile pins that a misconfigured --socket path
// pointing at an unrelated file is rejected rather than silently deleted.
// listen only ever removes an actual leftover unix socket.
func TestListen_RefusesNonSocketFile(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(sockPath, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("seed non-socket file: %v", err)
	}

	if _, err := listen(sockPath); err == nil {
		t.Fatal("expected listen to refuse a socketPath pointing at a non-socket file")
	}

	// The file must survive untouched.
	data, err := os.ReadFile(sockPath)
	if err != nil {
		t.Fatalf("read back %s: %v", sockPath, err)
	}
	if string(data) != "not a socket" {
		t.Error("listen must not have modified or removed the non-socket file, but its contents changed")
	}
}

// TestBrokerSocket_IsReservedAgainstPerRunSweeps is the drift guard on the one
// literal that keeps a per-run cleanup from unlinking this broker's control
// socket. Both live in the same directory, and the per-run paths are derived
// from a run id by a sanitizer that maps any input onto a flat name there — so
// internal/sandbox's sweeps refuse a derived path whose basename is this
// socket's. That refusal is spelled as a duplicated literal, because this
// package imports internal/sandbox and the dependency cannot run the other
// way; nothing but this test keeps the two spellings together.
//
// The failure it catches is quiet and total: rename the broker socket without
// updating the reserved set, and the sweeps go back to being permitted to
// delete it — with no compile error, and no symptom until a run id happens to
// derive onto the new name and takes every subsequent run on the host down.
func TestBrokerSocket_IsReservedAgainstPerRunSweeps(t *testing.T) {
	if !sandbox.IsReservedSocketRootPath(DefaultSocketPath) {
		t.Errorf("sandbox's per-run sweeps do not treat %q as reserved; a run id deriving onto it would be permitted to unlink the broker's control socket", DefaultSocketPath)
	}
	// The reservation must be narrow: an ordinary per-run socket in the same
	// directory is exactly what those sweeps exist to remove.
	if perRun := sandbox.TrustedAgentHostSocketPath("some-run-id"); sandbox.IsReservedSocketRootPath(perRun) {
		t.Errorf("%q is reserved, but it is per-run state the cell teardown has to be able to clear", perRun)
	}
}
