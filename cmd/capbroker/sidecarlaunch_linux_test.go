//go:build linux

package capbroker

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// withStubSidecarProcess points launchSidecarProcess at a stand-in built by
// mkCmd, the sidecar analog of withStubRuntime: it wires the passed-through
// socket fd as the stand-in's stdin+stdout, starts it, and closes the
// broker's own fd copy — exercising the LaunchSidecar/WaitSidecar/
// KillSidecar RPCs + registry + socket passthrough without a real setuid
// exec of the triagefactory binary.
func withStubSidecarProcess(t *testing.T, mkCmd func(ctx context.Context) *exec.Cmd) {
	t.Helper()
	orig := launchSidecarProcess
	launchSidecarProcess = func(ctx context.Context, containerID string, uid, gid int, stdio *os.File, extra []*os.File, stderr io.Writer) (supervisedRuntime, error) {
		cmd := mkCmd(ctx)
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

// withTempSidecarStdioSocketDir redirects the per-sidecar stdio socket path
// off the root-only production /run/tf into a writable temp dir.
func withTempSidecarStdioSocketDir(t *testing.T) {
	t.Helper()
	orig := sidecarStdioSocketPath
	dir := t.TempDir()
	sidecarStdioSocketPath = func(containerID string) string {
		return filepath.Join(dir, containerID+"-sidecar-stdio.sock")
	}
	t.Cleanup(func() { sidecarStdioSocketPath = orig })
}

// validSidecarLaunchParams returns params that pass
// sandbox.ValidateSidecarLaunchParams. idPrefix gets the
// SidecarContainerIDSuffix appended so callers don't have to remember it.
func validSidecarLaunchParams(idPrefix string) sandbox.SidecarLaunchParams {
	return sandbox.SidecarLaunchParams{
		ContainerID: idPrefix + sandbox.SidecarContainerIDSuffix,
		UID:         sandbox.SidecarUIDBase,
		GID:         sandbox.SidecarUIDBase,
	}
}

// TestSidecarLaunch_RoundTripAndClose drives the full orchestrator<->broker
// sidecar path: LaunchSidecar opens the per-sidecar listener, issues the
// launch RPC (the broker dials + hands the fd to the stand-in process +
// registers it), accepts the stdio end, and Close kills + waits it out via
// KillSidecar/WaitSidecar.
func TestSidecarLaunch_RoundTripAndClose(t *testing.T) {
	withStubSidecarProcess(t, func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "cat") })
	withTempSidecarStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	sc, err := client.LaunchSidecar(context.Background(), validSidecarLaunchParams("sc1"))
	if err != nil {
		t.Fatalf("LaunchSidecar: %v", err)
	}
	if err := sc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Idempotent — a second Close must not panic or hang.
	if err := sc.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestSidecarLaunch_RejectsUIDOutsideBand pins the RPC-boundary rejection:
// a launch naming a uid outside the reserved sidecar band must fail before
// the broker ever dials the stdio socket or execs anything.
func TestSidecarLaunch_RejectsUIDOutsideBand(t *testing.T) {
	withStubSidecarProcess(t, func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "cat") })
	withTempSidecarStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	p := validSidecarLaunchParams("scbad")
	p.UID, p.GID = 0, 0 // root — must be refused
	_, err := client.LaunchSidecar(context.Background(), p)
	if err == nil {
		t.Fatal("expected LaunchSidecar to reject a uid outside the reserved sidecar band")
	}
}

// TestSidecarLaunch_RunAndSidecarRegistriesDontCollide pins that a run's
// LaunchRun entry and its sidecar's LaunchSidecar entry — sharing the same
// broker-side registry, keyed by ContainerID — coexist and tear down
// independently, exactly like two concurrent runs do.
func TestSidecarLaunch_RunAndSidecarRegistriesDontCollide(t *testing.T) {
	withStubRuntime(t, func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "cat") })
	withStubSidecarProcess(t, func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "cat") })
	withTempStdioSocketDir(t)
	withTempSidecarStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	run, err := client.LaunchRun(context.Background(), validLaunchParams("c1"))
	if err != nil {
		t.Fatalf("LaunchRun: %v", err)
	}
	defer run.Close()
	if err := run.Start(); err != nil {
		t.Fatalf("Start run: %v", err)
	}

	sc, err := client.LaunchSidecar(context.Background(), validSidecarLaunchParams("c1"))
	if err != nil {
		t.Fatalf("LaunchSidecar: %v", err)
	}

	// Ending the sidecar must not disturb the still-live run.
	if err := sc.Close(); err != nil {
		t.Errorf("sidecar Close: %v", err)
	}
	_ = run.Stdin().Close()
	if err := run.Wait(); err != nil {
		t.Errorf("run Wait after sidecar teardown: %v", err)
	}
}

// TestSidecarLaunch_RejectsDuplicateContainerID pins that registering a
// second sidecar under a container id already live in the broker's shared
// run registry is refused rather than silently overwriting the first
// entry — an overwrite would leak the original process's supervision state
// (no further wait/kill could ever reach it).
func TestSidecarLaunch_RejectsDuplicateContainerID(t *testing.T) {
	withStubSidecarProcess(t, func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "cat") })
	withTempSidecarStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	params := validSidecarLaunchParams("dup")
	first, err := client.LaunchSidecar(context.Background(), params)
	if err != nil {
		t.Fatalf("first LaunchSidecar: %v", err)
	}
	defer first.Close()

	if _, err := client.LaunchSidecar(context.Background(), params); err == nil {
		t.Fatal("expected a second LaunchSidecar with the same container id to be rejected")
	}
}
