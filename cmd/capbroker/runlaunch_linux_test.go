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
// It also stands in for the cgroup read the real launcher does at exit:
// actuals is whatever the test wants Wait to report back through the
// WaitRun result, so the wire path for the measured actuals is exercised
// without a real cgroup (which needs CAP_SYS_ADMIN and a live jail).
// stderrTail plays the same role for the jail's stderr tail: whatever the
// test wants Wait to have captured, so the wire path back to the
// orchestrator's LaunchedRun.Stderr is exercised without a real gVisor
// failure to produce one.
type stubRuntime struct {
	cmd        *exec.Cmd
	actuals    sandbox.RunActuals
	stderrTail string
}

func (s *stubRuntime) Wait() (bool, error)         { return false, s.cmd.Wait() }
func (s *stubRuntime) Actuals() sandbox.RunActuals { return s.actuals }
func (s *stubRuntime) StderrTail() string          { return s.stderrTail }
func (s *stubRuntime) Kill() error {
	if s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Signal(syscall.SIGKILL)
}

// withStubRuntime points launchRuntime at a stand-in built by mkCmd
// (e.g. cat for a bidirectional echo, or sleep for a run that ignores its
// stdio and only ends on Kill). It also stubs prepareBundle so the broker's
// spec/bundle construction (which needs a real baked rootfs + root) is
// skipped while the RPC, validation, semaphore, registry, and
// socket-passthrough wiring are exercised.
func withStubRuntime(t *testing.T, mkCmd func(ctx context.Context) *exec.Cmd) {
	t.Helper()
	withStubRuntimeActuals(t, mkCmd, sandbox.RunActuals{})
}

// withStubRuntimeActuals is withStubRuntime with the exit-time cgroup read
// scripted, for the tests that follow the measured actuals across the RPC.
func withStubRuntimeActuals(t *testing.T, mkCmd func(ctx context.Context) *exec.Cmd, actuals sandbox.RunActuals) {
	t.Helper()
	orig := launchRuntime
	launchRuntime = func(ctx context.Context, _, _ string, _ int, stdio *os.File, stderr io.Writer) (jailedRuntime, error) {
		cmd := mkCmd(ctx)
		cmd.Stdin = stdio
		cmd.Stdout = stdio
		cmd.Stderr = stderr
		err := cmd.Start()
		_ = stdio.Close()
		if err != nil {
			return nil, err
		}
		return &stubRuntime{cmd: cmd, actuals: actuals}, nil
	}
	t.Cleanup(func() { launchRuntime = orig })
	withStubPrepareBundle(t)
}

// withStubPrepareBundle replaces the broker's real spec/bundle construction
// with a throwaway temp dir, so tests exercise the launch RPC path without a
// baked rootfs or root.
func withStubPrepareBundle(t *testing.T) {
	t.Helper()
	orig := prepareBundle
	prepareBundle = func(ctx context.Context, p sandbox.LaunchParams) (string, error) {
		return t.TempDir(), nil
	}
	t.Cleanup(func() { prepareBundle = orig })
}

// validLaunchParams returns params that pass sandbox.ValidateLaunchParams,
// so the broker's boundary gate admits the launch and the test exercises the
// RPC/registry/socket wiring rather than a validation rejection. The netns
// name matches the broker's tf-<hex>-<idx> shape the validator requires.
// The worktree must also actually exist on disk (the validator resolves
// symlinks, which requires the path to be there) — idempotently created
// here rather than threading *testing.T through this function's several
// call sites, since nothing writes into it.
func validLaunchParams(containerID string) sandbox.LaunchParams {
	_ = os.MkdirAll(sandbox.RunTreeRoot("run"), 0o755)
	return sandbox.LaunchParams{
		ConversationID: "run",
		ContainerID:    containerID,
		Worktree:       sandbox.RunTreeRoot("run"),
		Args:           []string{sandbox.TrustedToolHostBinaryDestination, "serve", "--connect", "/run/tf-tools/tools.sock"},
		// The netns name must be the one ConversationID derives — the ownership check
		// binds it to the run, not just the tf-<hex>-<idx> shape.
		NetnsPath: "/var/run/netns/" + sandbox.NetnsNameForRun("run", 1),
	}
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

	run, err := client.LaunchRun(context.Background(), validLaunchParams("c1"))
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
	// No cgroup behind this stand-in → nothing measured. Absent, not zeroes:
	// the claim must record NULL rather than a fabricated cost.
	if a := run.Actuals(); a.PeakMemMB != nil || a.CPUUsec != nil {
		t.Errorf("Actuals = %+v with no cgroup, want both absent", a)
	}
}

// TestBrokerRun_ActualsCrossTheWire pins that what the broker read off the
// run's cgroup at exit reaches the orchestrator through the WaitRun result —
// the whole point of extending that result rather than having the
// orchestrator sample /sys/fs/cgroup itself.
func TestBrokerRun_ActualsCrossTheWire(t *testing.T) {
	peak, cpu := 812, int64(4_250_000)
	withStubRuntimeActuals(t,
		func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "cat") },
		sandbox.RunActuals{PeakMemMB: &peak, CPUUsec: &cpu})
	withTempStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	run, err := client.LaunchRun(context.Background(), validLaunchParams("c-actuals"))
	if err != nil {
		t.Fatalf("LaunchRun: %v", err)
	}
	defer run.Close()
	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := run.Stdin().Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	if err := run.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}

	got := run.Actuals()
	if got.PeakMemMB == nil || *got.PeakMemMB != peak {
		t.Errorf("PeakMemMB = %v, want %d", got.PeakMemMB, peak)
	}
	if got.CPUUsec == nil || *got.CPUUsec != cpu {
		t.Errorf("CPUUsec = %v, want %d", got.CPUUsec, cpu)
	}
}

// TestBrokerRun_ActualsOmitAbsentPeak pins the pre-5.19-kernel shape end to
// end: cpu time crosses, the peak stays absent rather than arriving as a
// zero that would record as a measured 0 MB peak.
func TestBrokerRun_ActualsOmitAbsentPeak(t *testing.T) {
	cpu := int64(9_001)
	withStubRuntimeActuals(t,
		func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "cat") },
		sandbox.RunActuals{CPUUsec: &cpu})
	withTempStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	run, err := client.LaunchRun(context.Background(), validLaunchParams("c-nopeak"))
	if err != nil {
		t.Fatalf("LaunchRun: %v", err)
	}
	defer run.Close()
	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := run.Stdin().Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	if err := run.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}

	got := run.Actuals()
	if got.PeakMemMB != nil {
		t.Errorf("PeakMemMB = %d, want absent (no memory.peak on the host)", *got.PeakMemMB)
	}
	if got.CPUUsec == nil || *got.CPUUsec != cpu {
		t.Errorf("CPUUsec = %v, want %d", got.CPUUsec, cpu)
	}
}

// TestBrokerRun_ActualsCapturedOnKill pins that a killed run still reports
// its cost: KillRun only signals, so the supervising goroutine's Wait is
// still the reader, and it runs before the cgroup is removed. A cancelled
// run consumed real memory and CPU, so its claim must carry them.
func TestBrokerRun_ActualsCapturedOnKill(t *testing.T) {
	peak, cpu := 64, int64(1_500)
	withStubRuntimeActuals(t,
		func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "sleep", "60") },
		sandbox.RunActuals{PeakMemMB: &peak, CPUUsec: &cpu})
	withTempStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, err := client.LaunchRun(ctx, validLaunchParams("c-killactuals"))
	if err != nil {
		t.Fatalf("LaunchRun: %v", err)
	}
	defer run.Close()
	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Cancel → the watcher issues KillRun → the child dies → Wait reports the
	// kill exit AND the actuals captured on the way out.
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

	got := run.Actuals()
	if got.PeakMemMB == nil || *got.PeakMemMB != peak {
		t.Errorf("killed run PeakMemMB = %v, want %d", got.PeakMemMB, peak)
	}
	if got.CPUUsec == nil || *got.CPUUsec != cpu {
		t.Errorf("killed run CPUUsec = %v, want %d", got.CPUUsec, cpu)
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
	run, err := client.LaunchRun(ctx, validLaunchParams("c2"))
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

// TestBrokerRun_ConcurrentRunsAreIndependent pins that two concurrently
// registered runs are keyed independently by their unique container ids:
// ending one does not disturb the other, and neither is left
// unwaitable. The broker keys its registry by the container id (unique per
// live Wrap) precisely so runs that share a fixed, non-unique TraceID can't
// collide and steal each other's wait/kill.
func TestBrokerRun_ConcurrentRunsAreIndependent(t *testing.T) {
	withStubRuntime(t, func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "cat") })
	withTempStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	runA, err := client.LaunchRun(context.Background(), validLaunchParams("cA"))
	if err != nil {
		t.Fatalf("LaunchRun A: %v", err)
	}
	defer runA.Close()
	if err := runA.Start(); err != nil {
		t.Fatalf("Start A: %v", err)
	}

	runB, err := client.LaunchRun(context.Background(), validLaunchParams("cB"))
	if err != nil {
		t.Fatalf("LaunchRun B: %v", err)
	}
	defer runB.Close()
	if err := runB.Start(); err != nil {
		t.Fatalf("Start B: %v", err)
	}

	// B round-trips independently — proving B's registration didn't clobber
	// A's entry (and vice versa).
	line := "ping\n"
	if _, err := io.WriteString(runB.Stdin(), line); err != nil {
		t.Fatalf("write B: %v", err)
	}
	got, err := bufio.NewReader(runB.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if got != line {
		t.Errorf("B round-trip = %q, want %q", got, line)
	}

	// End A, then B — both remain independently waitable.
	_ = runA.Stdin().Close()
	if err := runA.Wait(); err != nil {
		t.Errorf("Wait A: %v", err)
	}
	_ = runB.Stdin().Close()
	if err := runB.Wait(); err != nil {
		t.Errorf("Wait B: %v", err)
	}
}

// TestBrokerRun_WaitOutlastsCallTimeout pins that WaitRun is not subject to
// the client's bounded call budget: a run that stays alive past callTimeout
// (then exits) must be waited out cleanly, not spuriously timed out. This is
// the one-shot Run path, which calls Wait while the runsc child is still
// exiting after emitting its terminal result. With a per-call cap on WaitRun
// this would return an i/o timeout and drop the exit's OOM attribution.
func TestBrokerRun_WaitOutlastsCallTimeout(t *testing.T) {
	// A run that ignores its stdio and stays alive well past the shrunk call
	// budget before exiting on its own.
	withStubRuntime(t, func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "sleep", "0.5") })
	withTempStdioSocketDir(t)

	origTimeout := callTimeout
	callTimeout = 150 * time.Millisecond
	t.Cleanup(func() { callTimeout = origTimeout })

	client := serveTestBroker(t, &fakeOps{})
	run, err := client.LaunchRun(context.Background(), validLaunchParams("cSlow"))
	if err != nil {
		t.Fatalf("LaunchRun: %v", err)
	}
	defer run.Close()
	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := time.Now()
	if err := run.Wait(); err != nil {
		t.Fatalf("Wait errored (spurious timeout past the %s call budget?): %v", callTimeout, err)
	}
	if elapsed := time.Since(start); elapsed < callTimeout {
		t.Errorf("Wait returned in %s, under the call budget — it did not actually wait for the run", elapsed)
	}
}

// TestBrokerRun_InFlightCapQueues pins the abuse-resistance cap: with the
// broker's in-flight LaunchRun limit shrunk to one, a second launch does not
// pile on privileged setup — it QUEUES on the semaphore until the first
// run's slot is freed, then proceeds. This is DoS resistance (a runaway
// caller degrades to queueing instead of exhausting the subnet pool), not a
// capability boundary. It also proves release: ending the first run frees
// the slot the second was waiting on.
func TestBrokerRun_InFlightCapQueues(t *testing.T) {
	prev := maxInflightLaunches
	maxInflightLaunches = 1
	t.Cleanup(func() { maxInflightLaunches = prev })

	withStubRuntime(t, func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "cat") })
	withTempStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	// Run A takes the only slot and holds it: its cat blocks reading stdin,
	// so the supervising goroutine (which releases the slot on exit) doesn't
	// run until A ends.
	runA, err := client.LaunchRun(context.Background(), validLaunchParams("capA"))
	if err != nil {
		t.Fatalf("LaunchRun A: %v", err)
	}
	defer runA.Close()
	if err := runA.Start(); err != nil {
		t.Fatalf("Start A: %v", err)
	}

	// Run B must queue: its Start issues the launch RPC, which blocks in the
	// broker's acquireLaunchSlot until A frees the one slot.
	runB, err := client.LaunchRun(context.Background(), validLaunchParams("capB"))
	if err != nil {
		t.Fatalf("LaunchRun B: %v", err)
	}
	defer runB.Close()

	startedB := make(chan error, 1)
	go func() { startedB <- runB.Start() }()

	select {
	case err := <-startedB:
		t.Fatalf("Start B returned (%v) while the in-flight cap was held; want it queued", err)
	case <-time.After(300 * time.Millisecond):
		// Still queued behind A — the cap is doing its job.
	}

	// Free the slot by ending A (its cat EOFs on stdin close and exits, so
	// the supervising goroutine releases the slot). B must then proceed.
	_ = runA.Stdin().Close()
	select {
	case err := <-startedB:
		if err != nil {
			t.Fatalf("Start B after the slot was freed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start B never proceeded after the in-flight slot was freed — release is broken")
	}
}

// TestBrokerRun_RejectsNonPinnedArgv pins that the broker only ever launches
// the pinned tool-host entrypoint: a launch whose Args names anything else —
// including the retired SDK node+wrapper shape — is rejected by
// ValidateLaunchParams before the broker builds or execs anything. This is
// what makes "the broker owns the command" true: the orchestrator can vary
// the tool host's arguments, never the executed program.
func TestBrokerRun_RejectsNonPinnedArgv(t *testing.T) {
	withTempStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	p := validLaunchParams("nonpinned")
	p.Args = []string{"/usr/bin/node", "/sdk/wrapper.mjs", "-p", "hi"}
	run, err := client.LaunchRun(context.Background(), p)
	if err != nil {
		t.Fatalf("LaunchRun: %v", err)
	}
	defer run.Close()
	if err := run.Start(); err == nil {
		t.Fatal("Start accepted the retired SDK node+wrapper argv; want rejection")
	}
}

// TestBrokerServer_KillUnknownRunIsNoop pins that a KillRun for a run the
// broker has already reaped (or never knew) is a harmless success, so a
// cancel racing a natural exit doesn't error.
func TestBrokerServer_KillUnknownRunIsNoop(t *testing.T) {
	client := serveTestBroker(t, &fakeOps{})
	if err := client.call(context.Background(), methodKillRun, killRunArgs{ContainerID: "nope"}, nil); err != nil {
		t.Errorf("KillRun for unknown run = %v, want nil", err)
	}
}

// withStubRuntimeStderr is withStubRuntime with the exit-time stderr tail
// scripted, for the test that follows it across the RPC.
func withStubRuntimeStderr(t *testing.T, mkCmd func(ctx context.Context) *exec.Cmd, tail string) {
	t.Helper()
	orig := launchRuntime
	launchRuntime = func(ctx context.Context, _, _ string, _ int, stdio *os.File, stderr io.Writer) (jailedRuntime, error) {
		cmd := mkCmd(ctx)
		cmd.Stdin = stdio
		cmd.Stdout = stdio
		cmd.Stderr = stderr
		err := cmd.Start()
		_ = stdio.Close()
		if err != nil {
			return nil, err
		}
		return &stubRuntime{cmd: cmd, stderrTail: tail}, nil
	}
	t.Cleanup(func() { launchRuntime = orig })
	withStubPrepareBundle(t)
}

// TestBrokerRun_StderrTailCrossesTheWire pins that what the broker kept of
// the jail's stderr reaches the orchestrator with the exit status. runsc
// writes its own create failures there and nowhere else, so without this the
// orchestrator's "exited before connecting" error can only say that a jail
// died, never why — which is what made a container-id collision a multi-step
// diagnosis instead of a one-line one.
func TestBrokerRun_StderrTailCrossesTheWire(t *testing.T) {
	const tail = `creating container: container with id "c-stderr" already exists`
	withStubRuntimeStderr(t, func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "cat") }, tail)
	withTempStdioSocketDir(t)
	client := serveTestBroker(t, &fakeOps{})

	run, err := client.LaunchRun(context.Background(), validLaunchParams("c-stderr"))
	if err != nil {
		t.Fatalf("LaunchRun: %v", err)
	}
	defer run.Close()
	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Before Wait there is no exit status to have carried a tail with.
	if got := run.Stderr(); got != "" {
		t.Errorf("Stderr before Wait = %q, want empty", got)
	}
	if err := run.Stdin().Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	if err := run.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}

	if got := run.Stderr(); got != tail {
		t.Errorf("Stderr after Wait = %q, want %q", got, tail)
	}
}
