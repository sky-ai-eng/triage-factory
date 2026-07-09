//go:build linux

package capbroker

import (
	"os"
	"path/filepath"
	"testing"
)

// TestListen_Permissions pins the socket hygiene invariant: 0700 dir, 0600
// socket file. Unlike agenthost's per-run socket, there is no chown step
// here — the broker socket is host-only and never bind-mounted into a
// sandbox, so it stays owned by whichever user started the broker (the
// orchestrator itself). Uses an isolated temp directory rather than the
// production socketDir (/run/tf, root-only on most distros) so this runs
// on an unprivileged CI runner too.
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
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("socket dir mode = %o, want 0700", got)
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
