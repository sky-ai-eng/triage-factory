//go:build linux

package delegate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestCaptureWorkspaceGit_LocalModeSkipsIsolation pins the dispatcher's routing:
// in local mode captureWorkspaceGit captures in-process and must NOT route
// through the dropped-privilege child (which needs root and a chowned tree local
// mode never produces). Guards against inverting the mode gate. captureCommand
// is stubbed to fail loudly if the isolated path is taken.
func TestCaptureWorkspaceGit_LocalModeSkipsIsolation(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	called := false
	orig := captureCommand
	captureCommand = func(ctx context.Context, wtPath string) (*exec.Cmd, error) {
		called = true
		return nil, fmt.Errorf("isolated child spawned in local mode")
	}
	t.Cleanup(func() { captureCommand = orig })

	dir := newCaptureTestRepo(t)
	delta, err := captureWorkspaceGit(context.Background(), dir)
	if err != nil {
		t.Fatalf("captureWorkspaceGit (local): %v", err)
	}
	if called {
		t.Error("local mode routed through the isolated child; want in-process capture")
	}
	if delta == nil || delta.Head == "" {
		t.Errorf("local capture returned %+v, want a delta with a head", delta)
	}
}

// newCaptureTestRepo makes a git repo with one commit and an uncommitted change,
// enough for CaptureWorkspaceGit to produce a non-empty delta.
func newCaptureTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "work")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "seed")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

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

	// A fresh CLONE_NEWNET namespace contains ONLY loopback. Assert exactly that
	// — the positive property — rather than blocklisting known host interface
	// names, so an interface with any name (enpXsY, wlan0, …) leaking in fails.
	ifaces := parseProcNetDevIfaces(netPart)
	if len(ifaces) == 0 {
		t.Fatalf("could not parse any interface from /proc/net/dev:\n%s", netPart)
	}
	for _, name := range ifaces {
		if name != "lo" {
			t.Errorf("child network namespace not isolated (interface %q present, want only lo): %v", name, ifaces)
		}
	}
}

// parseProcNetDevIfaces extracts interface names from a /proc/net/dev dump.
// Data lines are "  <name>: <counters...>"; the two header lines have no
// pre-colon interface token, so keying on the colon-delimited first field and
// dropping empties yields exactly the interface set.
func parseProcNetDevIfaces(procNetDev string) []string {
	var names []string
	for _, line := range strings.Split(procNetDev, "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		if name := strings.TrimSpace(line[:i]); name != "" && !strings.Contains(name, "|") {
			names = append(names, name)
		}
	}
	return names
}
