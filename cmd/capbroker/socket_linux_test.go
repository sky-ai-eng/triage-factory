//go:build linux

package capbroker

import (
	"os"
	"path/filepath"
	"testing"
)

// TestListen_Permissions pins the socket hygiene invariant: 0711 dir
// (traversable by any uid via a known path, but not listable — see
// listen's doc), 0600 socket file. Unlike agenthost's per-run socket,
// there is no chown step in listen() itself — the caller (runBroker, via
// --orchestrator-uid) chowns the file separately once it knows the
// dropped-privilege orchestrator's target uid. Uses an isolated temp
// directory rather than the production socketDir (/run/tf, root-only on
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
	if got := dirInfo.Mode().Perm(); got != 0o711 {
		t.Errorf("socket dir mode = %o, want 0711", got)
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
