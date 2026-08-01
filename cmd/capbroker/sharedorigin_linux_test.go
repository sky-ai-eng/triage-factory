//go:build linux

package capbroker

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestSharedOriginListenAddr_DerivedFromSubnetIndex pins the address the broker
// binds a run's shared origin on. It is derived, not caller-named, and it is on
// the fixed shared-origin port — a ported address here would silently cost gh's
// repo inference in the jail, which is the entire reason the broker binds this
// listener rather than the sidecar.
func TestSharedOriginListenAddr_DerivedFromSubnetIndex(t *testing.T) {
	for _, c := range []struct {
		idx  uint8
		want string
	}{
		{0, "10.42.0.1:443"},
		{7, "10.42.7.1:443"},
		{255, "10.42.255.1:443"},
	} {
		if got := sharedOriginListenAddr(c.idx); got != c.want {
			t.Errorf("sharedOriginListenAddr(%d) = %q, want %q", c.idx, got, c.want)
		}
	}
	if sandbox.SharedOriginPort != 443 {
		t.Errorf("SharedOriginPort = %d; gh drops the port when matching a remote against GH_HOST, so only the https default resolves a repo from a worktree", sandbox.SharedOriginPort)
	}
}

// TestSidecarLaunch_HandsSharedOriginListenerDown is the fd-passing contract:
// the broker binds the listener and the sidecar process receives it as its one
// extra descriptor, live and accepting. The broker keeps no copy — it is
// granting a privileged resource, not taking a traffic role.
func TestSidecarLaunch_HandsSharedOriginListenerDown(t *testing.T) {
	// Loopback:0 stands in for the veth IP on 443, which needs the run's network
	// and root. Everything else on the path is the production one.
	orig := sharedOriginListenAddr
	sharedOriginListenAddr = func(uint8) string { return "127.0.0.1:0" }
	t.Cleanup(func() { sharedOriginListenAddr = orig })

	var gotExtra []*os.File
	withStubSidecarProcessCapturingExtra(t, &gotExtra)
	withTempSidecarStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	sc, err := client.LaunchSidecar(context.Background(), validSidecarLaunchParams("scorigin"))
	if err != nil {
		t.Fatalf("LaunchSidecar: %v", err)
	}
	defer func() { _ = sc.Close() }()

	if len(gotExtra) != 1 {
		t.Fatalf("sidecar received %d extra descriptors, want exactly 1 (the shared-origin listener)", len(gotExtra))
	}
	// The descriptor the child got is a live listening socket: rebuild a
	// listener from it and dial through.
	ln, err := net.FileListener(gotExtra[0])
	if err != nil {
		t.Fatalf("the extra descriptor is not a listener: %v", err)
	}
	defer func() { _ = ln.Close() }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial the handed-down listener: %v", err)
	}
	_ = conn.Close()
	accepted, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept on the handed-down listener: %v", err)
	}
	_ = accepted.Close()
}

// TestSidecarLaunch_SurvivesUnbindableSharedOrigin pins the degradation: a bind
// that fails (the veth is gone, the port is taken, the broker lost the
// privilege) costs the run its gh repo inference and nothing else. The launch
// still succeeds and the sidecar still comes up.
func TestSidecarLaunch_SurvivesUnbindableSharedOrigin(t *testing.T) {
	orig := sharedOriginListenAddr
	// A syntactically valid address no test host can bind.
	sharedOriginListenAddr = func(uint8) string { return "192.0.2.1:443" }
	t.Cleanup(func() { sharedOriginListenAddr = orig })

	var gotExtra []*os.File
	withStubSidecarProcessCapturingExtra(t, &gotExtra)
	withTempSidecarStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	sc, err := client.LaunchSidecar(context.Background(), validSidecarLaunchParams("scnobind"))
	if err != nil {
		t.Fatalf("LaunchSidecar failed on an unbindable shared origin; the listener is best-effort: %v", err)
	}
	defer func() { _ = sc.Close() }()
	if len(gotExtra) != 0 {
		t.Errorf("sidecar received %d extra descriptors after a failed bind, want 0", len(gotExtra))
	}
}

// withStubSidecarProcessCapturingExtra is withStubSidecarProcess plus a dup of
// each extra descriptor, so a test can inspect what the child received after the
// launch path has closed the broker's own copies.
func withStubSidecarProcessCapturingExtra(t *testing.T, into *[]*os.File) {
	t.Helper()
	orig := launchSidecarProcess
	launchSidecarProcess = func(ctx context.Context, containerID string, uid, gid int, stdio *os.File, extra []*os.File, stderr io.Writer) (supervisedRuntime, error) {
		for _, f := range extra {
			fd, err := syscall.Dup(int(f.Fd()))
			if err != nil {
				t.Errorf("dup extra descriptor: %v", err)
				continue
			}
			dup := os.NewFile(uintptr(fd), f.Name())
			t.Cleanup(func() { _ = dup.Close() })
			*into = append(*into, dup)
		}
		cmd := exec.CommandContext(ctx, "cat")
		cmd.Stdin = stdio
		cmd.Stdout = stdio
		cmd.Stderr = stderr
		cmd.ExtraFiles = extra
		err := cmd.Start()
		_ = stdio.Close()
		for _, f := range extra {
			_ = f.Close()
		}
		if err != nil {
			return nil, err
		}
		return &stubRuntime{cmd: cmd}, nil
	}
	t.Cleanup(func() { launchSidecarProcess = orig })
}
