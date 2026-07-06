//go:build linux

package delegate

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCaptureIsolated_DropsPrivilegeAndNetns pins the containment captureIsolated
// relies on: the capture child runs as the sandbox uid/gid with the parent
// root's supplementary groups shed, inside an empty network namespace. That is
// what confines a filter a hostile .git/config could trigger to the agent's own
// privilege instead of host root (the escape this capture path closes).
//
// It overrides captureCommand with a shell that reports `id` and /proc/net/dev
// to a side file (stdout stays clean — it echoes the "null" delta), then asserts
// on that file. Needs root to setuid/setgroups/unshare, so it skips otherwise —
// the same gate as the sandbox integration tests.
func TestCaptureIsolated_DropsPrivilegeAndNetns(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to setuid/setgroups/unshare a network namespace")
	}

	diag, err := os.CreateTemp("", "tf-capdrop-*")
	if err != nil {
		t.Fatal(err)
	}
	diagPath := diag.Name()
	_ = diag.Close()
	// The child runs as uid 10000; it must be able to write the diag file.
	if err := os.Chmod(diagPath, 0o666); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(diagPath) })

	orig := captureCommand
	captureCommand = func(ctx context.Context, wtPath string) (*exec.Cmd, error) {
		script := "{ id; echo ==NET==; cat /proc/net/dev; } > " + diagPath + " 2>&1; echo null"
		return exec.CommandContext(ctx, "/bin/sh", "-c", script), nil
	}
	t.Cleanup(func() { captureCommand = orig })

	delta, err := captureIsolated(context.Background(), "/unused-worktree")
	if err != nil {
		t.Fatalf("captureIsolated: %v", err)
	}
	if delta != nil {
		t.Errorf("delta = %+v, want nil (child echoed the null delta)", delta)
	}

	b, err := os.ReadFile(diagPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	idLine, netPart := out, ""
	if i := strings.Index(out, "==NET=="); i >= 0 {
		idLine, netPart = out[:i], out[i:]
	}

	if !strings.Contains(idLine, "uid=10000") || !strings.Contains(idLine, "gid=10000") {
		t.Errorf("child did not drop to uid/gid 10000:\n%s", idLine)
	}
	if strings.Contains(idLine, "(root)") || strings.Contains(idLine, "groups=0") {
		t.Errorf("child retained a root supplementary group:\n%s", idLine)
	}
	// An empty network namespace has only loopback in /proc/net/dev; a host
	// interface leaking in means CLONE_NEWNET did not take.
	for _, iface := range []string{"eth0", "ens", "eno", "docker0", "veth"} {
		if strings.Contains(netPart, iface) {
			t.Errorf("child network namespace not isolated (saw %q):\n%s", iface, netPart)
		}
	}
}
