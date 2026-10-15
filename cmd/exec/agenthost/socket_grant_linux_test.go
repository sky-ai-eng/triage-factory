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
// the socket keeps its owner and becomes group jailGID, mode 0660 — the
// owner-legal group grant a sidecar carrying that gid makes. A process that
// neither is root nor carries the gid gets a clear error naming the
// group-membership requirement, rather than a silently unreachable socket.
func TestGrantToJail(t *testing.T) {
	jailGID := sandbox.JailGID(4)
	sockPath := filepath.Join(t.TempDir(), "grant-test.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	member := os.Getgid() == jailGID
	if groups, err := os.Getgroups(); err == nil {
		for _, g := range groups {
			if g == jailGID {
				member = true
			}
		}
	}

	err = grantToJail(sockPath, jailGID, 0o660)
	if os.Getuid() != 0 && !member {
		if err == nil {
			t.Fatal("grantToJail succeeded for a process that neither is root nor carries the jail gid; the chgrp should have been refused")
		}
		return
	}
	if err != nil {
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
