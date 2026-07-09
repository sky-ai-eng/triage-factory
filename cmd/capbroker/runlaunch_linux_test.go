//go:build linux

package capbroker

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// stubRuntime replicates sandbox.LaunchSupervised's fd handoff with a
// stand-in command instead of runsc, so the broker's LaunchRun/WaitRun/
// KillRun RPCs + run registry + socket passthrough are exercised without a
// real gVisor host. It wires the passed-through socket as the stand-in's
// stdin+stdout, starts it, and closes its own fd copy — exactly the
// no-read/close-after-Start property the real launcher has.
type stubRuntime struct{ cmd *exec.Cmd }

func (s *stubRuntime) Wait() (bool, error) { return false, s.cmd.Wait() }
func (s *stubRuntime) Kill() error {
	if s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Signal(syscall.SIGKILL)
}

// withStubRuntime points launchRuntime at a stand-in built by mkCmd
// (e.g. cat for a bidirectional echo, or sleep for a run that ignores its
// stdio and only ends on Kill).
func withStubRuntime(t *testing.T, mkCmd func(ctx context.Context) *exec.Cmd) {
	t.Helper()
	orig := launchRuntime
	launchRuntime = func(ctx context.Context, _ sandbox.LaunchParams, stdio *os.File, stderr io.Writer) (supervisedRuntime, error) {
		cmd := mkCmd(ctx)
		cmd.Stdin = stdio
		cmd.Stdout = stdio
		cmd.Stderr = stderr
		err := cmd.Start()
		_ = stdio.Close()
		if err != nil {
			return nil, err
		}
		return &stubRuntime{cmd: cmd}, nil
	}
	t.Cleanup(func() { launchRuntime = orig })
}

// withTempStdioSocketDir redirects the per-run stdio socket path off the
// root-only production /run/tf into a writable temp dir.
func withTempStdioSocketDir(t *testing.T) {
	t.Helper()
	orig := stdioSocketPath
	dir := t.TempDir()
	stdioSocketPath = func(containerID string) string {
		return filepath.Join(dir, containerID+"-stdio.sock")
	}
	t.Cleanup(func() { stdioSocketPath = orig })
}

// TestBrokerRun_RoundTripAndWait drives the full orchestrator↔broker launch
// path: LaunchRun opens the per-run listener, Start issues the launch RPC
// (the broker dials + hands the fd to the stand-in runtime + registers the
// run) and accepts the stdio end, an NDJSON line round-trips through the
// runtime, and Wait fetches the clean exit via the WaitRun RPC.
func TestBrokerRun_RoundTripAndWait(t *testing.T) {
	withStubRuntime(t, func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "cat") })
	withTempStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	run, err := client.LaunchRun(context.Background(), sandbox.LaunchParams{RunID: "r1", ContainerID: "c1"})
	if err != nil {
		t.Fatalf("LaunchRun: %v", err)
	}
	defer run.Close()
	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	line := `{"kind":"user_message","text":"hi"}` + "\n"
	if _, err := io.WriteString(run.Stdin(), line); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	got, err := bufio.NewReader(run.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if got != line {
		t.Errorf("NDJSON round-trip = %q, want %q", got, line)
	}

	// Half-close stdin → the runtime's stdin EOFs → it exits → WaitRun
	// resolves.
	if err := run.Stdin().Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	if err := run.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}
	if run.OOMKilled() {
		t.Error("OOMKilled true with no memory limit configured")
	}
}

// TestBrokerRun_KillViaContext pins that canceling the run context makes the
// orchestrator-side watcher issue a KillRun RPC, terminating a runtime that
// ignores its stdio — the cancellation path, since the orchestrator can't
// SIGKILL the broker's child directly.
func TestBrokerRun_KillViaContext(t *testing.T) {
	withStubRuntime(t, func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "sleep", "60") })
	withTempStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, err := client.LaunchRun(ctx, sandbox.LaunchParams{RunID: "r2", ContainerID: "c2"})
	if err != nil {
		t.Fatalf("LaunchRun: %v", err)
	}
	defer run.Close()
	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Cancel the run ctx → the watcher issues KillRun → the child dies.
	cancel()
	done := make(chan error, 1)
	go func() { done <- run.Wait() }()
	select {
	case werr := <-done:
		if werr == nil {
			t.Error("Wait returned nil after ctx-cancel kill; want a non-nil exit")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after ctx-cancel kill")
	}
}

// TestBrokerServer_KillUnknownRunIsNoop pins that a KillRun for a run the
// broker has already reaped (or never knew) is a harmless success, so a
// cancel racing a natural exit doesn't error.
func TestBrokerServer_KillUnknownRunIsNoop(t *testing.T) {
	client := serveTestBroker(t, &fakeOps{})
	if err := client.call(context.Background(), methodKillRun, killRunArgs{RunID: "nope"}, nil); err != nil {
		t.Errorf("KillRun for unknown run = %v, want nil", err)
	}
}
