//go:build linux

package capbroker

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// fakeOps is a scriptable sandbox.PrivilegedOps double so the protocol/
// dispatch round trip can be tested without real netns/iptables/cgroup
// syscalls (which need CAP_NET_ADMIN/CAP_SYS_ADMIN and real kernel state).
type fakeOps struct {
	setupNetworkFn    func(ctx context.Context, runID string, subnetIdx uint8) (sandbox.NetworkState, error)
	teardownNetworkFn func(ctx context.Context, state sandbox.NetworkState) error
	ensureRootfsFn    func(ctx context.Context, selector sandbox.RootfsSelector) (string, error)
	setupRunCgroupFn  func(name string, limitMB int) (string, *os.File, error)
	launchRunFn       func(ctx context.Context, p sandbox.LaunchParams) (sandbox.LaunchedRun, error)
	removeRunCgroupFn func(dir string) error
	reapOrphansFn     func(ctx context.Context) error
}

func (f *fakeOps) SetupNetwork(ctx context.Context, runID string, subnetIdx uint8) (sandbox.NetworkState, error) {
	return f.setupNetworkFn(ctx, runID, subnetIdx)
}
func (f *fakeOps) TeardownNetwork(ctx context.Context, state sandbox.NetworkState) error {
	return f.teardownNetworkFn(ctx, state)
}
func (f *fakeOps) EnsureRootfs(ctx context.Context, selector sandbox.RootfsSelector) (string, error) {
	return f.ensureRootfsFn(ctx, selector)
}
func (f *fakeOps) SetupRunCgroup(name string, limitMB int) (string, *os.File, error) {
	return f.setupRunCgroupFn(name, limitMB)
}
func (f *fakeOps) LaunchRun(ctx context.Context, p sandbox.LaunchParams) (sandbox.LaunchedRun, error) {
	if f.launchRunFn == nil {
		return nil, nil
	}
	return f.launchRunFn(ctx, p)
}
func (f *fakeOps) RemoveRunCgroup(dir string) error {
	return f.removeRunCgroupFn(dir)
}
func (f *fakeOps) ReapOrphans(ctx context.Context) error {
	return f.reapOrphansFn(ctx)
}

var _ sandbox.PrivilegedOps = (*fakeOps)(nil)

// serveTestBroker starts a Server backed by ops on a temp-dir unix socket
// and returns a dialed IPCClient plus a cleanup func. Isolated from the
// production /run/tf path so tests don't need root or step on a real
// broker's socket.
func serveTestBroker(t *testing.T, ops sandbox.PrivilegedOps) *IPCClient {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(ops)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() {
		_ = l.Close()
	})
	return Dial(sockPath)
}

func TestIPCRoundTrip_Ping(t *testing.T) {
	client := serveTestBroker(t, &fakeOps{})
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestIPCRoundTrip_SetupAndTeardownNetwork(t *testing.T) {
	wantState := sandbox.NetworkState{
		Subnet:    "10.42.7.0/24",
		HostIP:    "10.42.7.1",
		NetnsPath: "/var/run/netns/tf-abc123-7",
	}
	var gotRunID string
	var gotSubnetIdx uint8
	var teardownState sandbox.NetworkState
	client := serveTestBroker(t, &fakeOps{
		setupNetworkFn: func(ctx context.Context, runID string, subnetIdx uint8) (sandbox.NetworkState, error) {
			gotRunID, gotSubnetIdx = runID, subnetIdx
			return wantState, nil
		},
		teardownNetworkFn: func(ctx context.Context, state sandbox.NetworkState) error {
			teardownState = state
			return nil
		},
	})

	got, err := client.SetupNetwork(context.Background(), "run-abc123", 7)
	if err != nil {
		t.Fatalf("SetupNetwork: %v", err)
	}
	if !reflect.DeepEqual(got, wantState) {
		t.Errorf("SetupNetwork state = %+v, want %+v", got, wantState)
	}
	if gotRunID != "run-abc123" || gotSubnetIdx != 7 {
		t.Errorf("broker saw runID=%q subnetIdx=%d, want run-abc123/7", gotRunID, gotSubnetIdx)
	}

	if err := client.TeardownNetwork(context.Background(), got); err != nil {
		t.Fatalf("TeardownNetwork: %v", err)
	}
	if !reflect.DeepEqual(teardownState, wantState) {
		t.Errorf("broker received teardown state %+v, want %+v", teardownState, wantState)
	}
}

func TestIPCRoundTrip_EnsureRootfs(t *testing.T) {
	var gotSelector sandbox.RootfsSelector
	client := serveTestBroker(t, &fakeOps{
		ensureRootfsFn: func(ctx context.Context, selector sandbox.RootfsSelector) (string, error) {
			gotSelector = selector
			return "/var/lib/tf/rootfs/alpine", nil
		},
	})

	path, err := client.EnsureRootfs(context.Background(), sandbox.RootfsSelector{Name: "alpine"})
	if err != nil {
		t.Fatalf("EnsureRootfs: %v", err)
	}
	if path != "/var/lib/tf/rootfs/alpine" {
		t.Errorf("path = %q, want /var/lib/tf/rootfs/alpine", path)
	}
	if gotSelector.Name != "alpine" {
		t.Errorf("broker saw selector %+v, want Name=alpine", gotSelector)
	}
}

func TestIPCRoundTrip_RemoveRunCgroup(t *testing.T) {
	var gotDir string
	client := serveTestBroker(t, &fakeOps{
		removeRunCgroupFn: func(dir string) error {
			gotDir = dir
			return nil
		},
	})
	if err := client.RemoveRunCgroup("/sys/fs/cgroup/tf-runs/run-1"); err != nil {
		t.Fatalf("RemoveRunCgroup: %v", err)
	}
	if gotDir != "/sys/fs/cgroup/tf-runs/run-1" {
		t.Errorf("broker saw dir %q", gotDir)
	}
}

func TestIPCRoundTrip_ReapOrphans(t *testing.T) {
	called := false
	client := serveTestBroker(t, &fakeOps{
		reapOrphansFn: func(ctx context.Context) error {
			called = true
			return nil
		},
	})
	if err := client.ReapOrphans(context.Background()); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if !called {
		t.Error("broker's ReapOrphans was never invoked")
	}
}

// TestIPCRoundTrip_ErrorPropagates pins that a broker-side error surfaces
// to the client as a plain error with the same message — the only
// consumers are internal/sandbox's PrivilegedOps call sites, which just
// wrap it with fmt.Errorf.
func TestIPCRoundTrip_ErrorPropagates(t *testing.T) {
	client := serveTestBroker(t, &fakeOps{
		ensureRootfsFn: func(ctx context.Context, selector sandbox.RootfsSelector) (string, error) {
			return "", errors.New("rootfs bake failed: disk full")
		},
	})
	_, err := client.EnsureRootfs(context.Background(), sandbox.RootfsSelector{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != "rootfs bake failed: disk full" {
		t.Errorf("error = %q, want the broker's message verbatim", err.Error())
	}
}

// TestIPCRoundTrip_SetupRunCgroup pins the fd-crossing workaround: the
// broker RPCs back only the directory path (a raw fd can't cross the
// wire without SCM_RIGHTS, deliberately not introduced), and IPCClient
// reopens that same path locally to hand a usable *os.File to its own
// exec.Cmd.CgroupFD caller.
func TestIPCRoundTrip_SetupRunCgroup(t *testing.T) {
	dir := t.TempDir()
	var gotName string
	var gotLimit int
	client := serveTestBroker(t, &fakeOps{
		setupRunCgroupFn: func(name string, limitMB int) (string, *os.File, error) {
			gotName, gotLimit = name, limitMB
			f, err := os.Open(dir)
			if err != nil {
				return "", nil, err
			}
			return dir, f, nil
		},
	})

	gotDir, f, err := client.SetupRunCgroup("run-1", 512)
	if err != nil {
		t.Fatalf("SetupRunCgroup: %v", err)
	}
	defer f.Close()
	if gotDir != dir {
		t.Errorf("dir = %q, want %q", gotDir, dir)
	}
	if gotName != "run-1" || gotLimit != 512 {
		t.Errorf("broker saw name=%q limitMB=%d, want run-1/512", gotName, gotLimit)
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat reopened fd: %v", err)
	}
	if !fi.IsDir() {
		t.Error("reopened fd is not the cgroup directory")
	}
}

// TestServer_UnknownMethod pins that a version-matched request naming an
// unrecognized method gets a clean error response rather than a hang or
// panic — the same defensive behavior agenthost's dispatch has.
func TestServer_UnknownMethod(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(&fakeOps{})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := writeFrame(conn, request{Version: ProtocolVersion, Method: "NotAMethod"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp response
	if err := readFrame(conn, &resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected an error response for an unknown method")
	}
}

// TestServer_VersionMismatch pins the version handshake guard.
func TestServer_VersionMismatch(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(&fakeOps{})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := writeFrame(conn, request{Version: ProtocolVersion + 1, Method: methodPing}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp response
	if err := readFrame(conn, &resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected an error response for a mismatched protocol version")
	}
}

// TestServer_ShutdownCancelsInFlightDispatch pins that Shutdown actually
// unwinds an in-flight, ctx-aware privileged op rather than just waiting
// out its full callTimeout budget — otherwise a broker asked to shut down
// mid-RPC can blow past runBroker's bounded shutdownDrain.
func TestServer_ShutdownCancelsInFlightDispatch(t *testing.T) {
	unblocked := make(chan struct{})
	ops := &fakeOps{
		reapOrphansFn: func(ctx context.Context) error {
			<-ctx.Done()
			close(unblocked)
			return ctx.Err()
		},
	}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(ops)
	go func() { _ = srv.Serve(l) }()

	// Fire a ReapOrphans RPC that blocks inside the fake op until Shutdown
	// cancels its dispatch context.
	go func() {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = writeFrame(conn, request{Version: ProtocolVersion, Method: methodReapOrphans})
		var resp response
		_ = readFrame(conn, &resp)
	}()

	// Give the dispatch goroutine time to actually enter the blocking op
	// before shutting down, so this test exercises real in-flight
	// cancellation rather than racing Shutdown against dial.
	select {
	case <-unblocked:
		t.Fatal("ops unblocked before Shutdown ran — the fake fired early")
	case <-time.After(100 * time.Millisecond):
	}

	_ = l.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-unblocked:
	default:
		t.Error("Shutdown returned but the in-flight dispatch's context was never canceled")
	}
}
