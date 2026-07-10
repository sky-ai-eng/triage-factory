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

// TestGrantSocketToSandbox pins step 3 of Start's fs-permissions
// invariant in whichever shape this process is allowed to perform it:
//
//   - root (TF_PRIVSEP=0 rollback): the socket ends up
//     WorktreeUID:WorktreeGID mode 0600 — the original arrangement.
//   - unprivileged member of the sandbox group (the default post-drop
//     orchestrator, as the setpriv non-root harness runs this test):
//     ownership is unchanged (no CAP_CHOWN), group becomes WorktreeGID,
//     mode 0660 — the owner-legal group grant.
//   - unprivileged NON-member: a clear error naming the group-membership
//     requirement, rather than a silently unreachable socket.
func TestGrantSocketToSandbox(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "grant-test.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	memberOfSandboxGroup := false
	if groups, err := os.Getgroups(); err == nil {
		for _, g := range groups {
			if g == sandbox.WorktreeGID {
				memberOfSandboxGroup = true
			}
		}
	}
	if os.Getgid() == sandbox.WorktreeGID {
		memberOfSandboxGroup = true
	}

	err = grantSocketToSandbox(sockPath)

	if os.Getuid() != 0 && !memberOfSandboxGroup {
		if err == nil {
			t.Fatal("grantSocketToSandbox succeeded for an unprivileged non-member of the sandbox group; the chgrp should have been refused")
		}
		return
	}
	if err != nil {
		t.Fatalf("grantSocketToSandbox: %v", err)
	}

	var st syscall.Stat_t
	if err := syscall.Stat(sockPath, &st); err != nil {
		t.Fatal(err)
	}
	if st.Gid != sandbox.WorktreeGID {
		t.Errorf("socket gid = %d, want %d", st.Gid, sandbox.WorktreeGID)
	}
	mode := os.FileMode(st.Mode).Perm()
	if os.Getuid() == 0 {
		if st.Uid != sandbox.WorktreeUID {
			t.Errorf("socket uid = %d, want %d (root path chowns to the sandbox uid)", st.Uid, sandbox.WorktreeUID)
		}
		if mode != 0o600 {
			t.Errorf("socket mode = %o, want 0600 on the root path", mode)
		}
	} else {
		if int(st.Uid) != os.Getuid() {
			t.Errorf("socket uid = %d, want %d (the unprivileged path must not change ownership)", st.Uid, os.Getuid())
		}
		if mode != 0o660 {
			t.Errorf("socket mode = %o, want 0660 on the group-grant path", mode)
		}
	}
}
