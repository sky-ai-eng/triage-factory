//go:build linux

package capbroker

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestListen_Permissions pins the socket hygiene invariant: a 0711 dir
// (traversable by any uid via a known path, but listable by nobody else and
// writable by nobody but its owner — so no sidecar or jail identity on the
// host can create, unlink or rename an entry here; see listen's doc) and a
// 0600 socket file. Uses an isolated temp directory rather than the
// production socketDir (/run/tf, root-only on most distros) so this runs on an
// unprivileged CI runner too.
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
	if got := dirInfo.Mode() & (os.ModePerm | os.ModeSticky | os.ModeSetgid | os.ModeSetuid); got != 0o711 {
		t.Errorf("socket dir mode = %v, want 0711", dirInfo.Mode())
	}
	if st, ok := dirInfo.Sys().(*syscall.Stat_t); ok && int(st.Gid) != os.Getgid() {
		t.Errorf("socket dir gid = %d, want this process's own %d (no group a sandboxed principal carries)", st.Gid, os.Getgid())
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
// literal that keeps a per-run removal from taking this broker's control
// socket. Both live in the same directory, and internal/sandbox's per-run
// prepare, release and boot sweep refuse to remove an entry whose basename is
// this socket's. That refusal is spelled as a duplicated literal, because this
// package imports internal/sandbox and the dependency cannot run the other
// way; nothing but this test keeps the two spellings together.
//
// The failure it catches is quiet and total: rename the broker socket without
// updating the reserved set, and the boot sweep goes back to being permitted
// to delete it — with no compile error, and no symptom until the next boot
// takes every subsequent run on the host down.
func TestBrokerSocket_IsReservedAgainstPerRunSweeps(t *testing.T) {
	if !sandbox.IsReservedSocketRootPath(DefaultSocketPath) {
		t.Errorf("sandbox's per-run sweeps do not treat %q as reserved; they would be permitted to remove the broker's control socket", DefaultSocketPath)
	}
	// The reservation must be narrow: a run's socket directories in the same
	// directory are exactly what those sweeps exist to remove.
	for _, perRun := range []string{sandbox.RunSocketDir("some-run-id"), sandbox.TrustedToolHostSocketDir("some-run-id")} {
		if sandbox.IsReservedSocketRootPath(perRun) {
			t.Errorf("%q is reserved, but it is per-run state the cell teardown has to be able to clear", perRun)
		}
	}
}
