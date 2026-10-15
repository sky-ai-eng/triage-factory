//go:build linux

package agenthost

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestGrantToJail pins step 3 of StartWithServer's fs-permissions invariant:
// a socket created in a directory setgid to the jail gid comes out in that
// group, and the grant opens it 0660 without touching its owner. A socket that
// came out in any other group — a directory missing its setgid bit — is
// refused with an error naming the directory, rather than served unreachable.
func TestGrantToJail(t *testing.T) {
	jailGID := sandbox.JailGID(4)

	// Not in the jail's group: refused, mode untouched.
	stray := filepath.Join(t.TempDir(), "stray.sock")
	l, err := net.Listen("unix", stray)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	if err := os.Chmod(stray, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := grantToJail(stray, jailGID, 0o660); err == nil {
		t.Error("grantToJail accepted a socket outside the jail's group")
	}
	if fi, err := os.Stat(stray); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("a refused grant changed the socket's mode")
	}

	if os.Geteuid() != 0 {
		t.Skip("the setgid case needs root to give a directory the jail's group")
	}
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, -1, jailGID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(dir, "grant-test.sock")
	l2, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l2.Close()
	if err := grantToJail(sockPath, jailGID, 0o660); err != nil {
		t.Fatalf("grantToJail: %v", err)
	}

	var st syscall.Stat_t
	if err := syscall.Stat(sockPath, &st); err != nil {
		t.Fatal(err)
	}
	if int(st.Gid) != jailGID {
		t.Errorf("socket gid = %d, want the run's jail gid %d", st.Gid, jailGID)
	}
	if int(st.Uid) != os.Getuid() {
		t.Errorf("socket uid = %d, want %d (the grant must not change ownership)", st.Uid, os.Getuid())
	}
	if mode := os.FileMode(st.Mode).Perm(); mode != 0o660 {
		t.Errorf("socket mode = %o, want 0660", mode)
	}
}

// TestGrantToJail_RefusesNonJailGID pins the guard on the group itself: a
// grant to anything outside the jail band — root's group, the orchestrator's,
// a sidecar's — is refused before the file is touched.
func TestGrantToJail_RefusesNonJailGID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, gid := range []int{0, 10001, sandbox.SidecarUID(4), sandbox.JailUIDBase + sandbox.MaxSandboxes} {
		if err := grantToJail(path, gid, 0o640); err == nil {
			t.Errorf("grantToJail(gid %d) = nil error, want refusal", gid)
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("a refused grant changed the file's mode")
	}
}
