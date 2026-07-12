//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// withStubSidecarBinary points sidecarBinaryPath at a small shell script
// that reports `id` — a stand-in for the real triagefactory binary's
// run-sidecar subcommand, so the setuid + stdio-handoff plumbing is
// exercised without needing a built binary or a live broker socket.
//
// Deliberately uses its own os.MkdirTemp rather than t.TempDir(): the
// testing package's own TempDir nests the returned leaf under an
// intermediate directory it creates at mode 0700 — fine for same-uid
// access, but this test execs the script AS a different, unprivileged uid
// (via syscall.Credential), and the kernel resolves that path AFTER the
// credential switch, so a 0700 ancestor anywhere in the chain would EACCES
// the whole exec regardless of the leaf directory's or script's own mode.
func withStubSidecarBinary(t *testing.T, script string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "tf-sidecar-stub-")
	if err != nil {
		t.Fatalf("mkdir stub sidecar dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod stub sidecar dir: %v", err)
	}
	path := filepath.Join(dir, "fake-sidecar.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write stub sidecar script: %v", err)
	}
	orig := sidecarBinaryPath
	sidecarBinaryPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { sidecarBinaryPath = orig })
}

// socketpairFiles returns a connected pair of *os.File wrapping an
// AF_UNIX SOCK_STREAM socketpair — a self-contained stand-in for the
// broker-dialed connection LaunchSidecarProcess receives in production, so
// these tests don't need a real listener.
func socketpairFiles(t *testing.T) (parent, child *os.File) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	return os.NewFile(uintptr(fds[0]), "parent"), os.NewFile(uintptr(fds[1]), "child")
}

// TestLaunchSidecarProcess_DropsToRequestedUID pins the whole point of the
// sidecar harness: the broker execs it at the validated per-run uid/gid, a
// real setuid+setgid via syscall.Credential. Needs root (CAP_SETUID) like
// every other privilege-drop test in this package.
func TestLaunchSidecarProcess_DropsToRequestedUID(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to setuid a per-run sidecar credential")
	}
	withStubSidecarBinary(t, "id\n")

	parent, child := socketpairFiles(t)
	defer parent.Close()

	const testUID = SidecarUIDBase + 1
	var stderr bytes.Buffer
	rt, err := LaunchSidecarProcess(context.Background(), "test-sc", testUID, testUID, child, &stderr)
	if err != nil {
		t.Fatalf("LaunchSidecarProcess: %v", err)
	}

	out, readErr := io.ReadAll(parent)
	if readErr != nil {
		t.Fatalf("read child stdout: %v", readErr)
	}
	if _, err := rt.Wait(); err != nil {
		t.Fatalf("Wait: %v; stderr=%s", err, stderr.String())
	}

	got := string(out)
	wantUID := fmt.Sprintf("uid=%d", testUID)
	wantGID := fmt.Sprintf("gid=%d", testUID)
	if !strings.Contains(got, wantUID) || !strings.Contains(got, wantGID) {
		t.Errorf("child did not run as uid/gid %d:\n%s", testUID, got)
	}
}

// TestLaunchSidecarProcess_KillTerminatesAndWaitReturns pins the
// Kill/Wait lifecycle the broker's runEntry machinery drives generically
// (the same shape a runsc SupervisedRuntime satisfies): Kill SIGKILLs a
// still-running sidecar and Wait then returns promptly with a non-nil exit
// error. Root-gated like the uid-drop test above: even a same-uid
// Credential makes Go's os/exec call setgroups() in the forked child
// (to clear supplementary groups) unless Credential.NoSetGroups is set,
// and that syscall needs CAP_SETGID regardless of the target uid/gid.
func TestLaunchSidecarProcess_KillTerminatesAndWaitReturns(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: even a same-uid Credential triggers setgroups(), which needs CAP_SETGID")
	}
	withStubSidecarBinary(t, "sleep 60\n")

	parent, child := socketpairFiles(t)
	defer parent.Close()

	uid := os.Getuid()
	rt, err := LaunchSidecarProcess(context.Background(), "test-sc-kill", uid, uid, child, io.Discard)
	if err != nil {
		t.Fatalf("LaunchSidecarProcess: %v", err)
	}

	if err := rt.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, werr := rt.Wait()
		done <- werr
	}()
	select {
	case werr := <-done:
		if werr == nil {
			t.Error("Wait returned nil after Kill; want a non-nil exit (SIGKILL)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Kill")
	}
}

// TestLaunchSidecarProcess_KillOnAlreadyExitedIsSafe pins idempotence: a
// Kill racing (or arriving after) a natural, already-reaped exit must not
// panic or hang — matching SupervisedRuntime.Kill's own contract, which
// this mirrors exactly (both are a bare cmd.Process.Signal(SIGKILL), no
// extra bookkeeping). Go's os.Process.Signal on an already-Wait()ed
// process returns a plain "already finished" error rather than a
// kernel ESRCH — same as it always has for the runsc path — so this pins
// "safe to call," not "returns nil"; every real caller (the broker's
// killRun handler, sidecarHandle.Close) discards the result either way.
// Root-gated for the same setgroups()/CAP_SETGID reason as the test above.
func TestLaunchSidecarProcess_KillOnAlreadyExitedIsSafe(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: even a same-uid Credential triggers setgroups(), which needs CAP_SETGID")
	}
	withStubSidecarBinary(t, "true\n")

	parent, child := socketpairFiles(t)
	defer parent.Close()

	uid := os.Getuid()
	rt, err := LaunchSidecarProcess(context.Background(), "test-sc-exit", uid, uid, child, io.Discard)
	if err != nil {
		t.Fatalf("LaunchSidecarProcess: %v", err)
	}
	if _, err := rt.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	_ = rt.Kill() // must not panic
}
