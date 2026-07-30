//go:build linux && integration

// Live proof that a sandboxed run's cost is actually measured. The readers
// themselves are unit-tested against fixture files (cgroup_linux_test.go);
// what only a real jail can show is that the numbers the kernel puts in a
// live run's cgroup are non-zero, bounded by the run's own ceiling, and still
// readable at the moment teardown takes them — before removeRunCgroup
// destroys the group.
//
// The recorded CPU deliberately includes the gVisor sentry's own systrap
// overhead: these are TF's cost view of a run, not a billed quantity, so the
// assertions bound the numbers rather than pinning them.

package sandbox

import (
	"context"
	"testing"
	"time"
)

// actualsLimitMB is the ceiling the measured run gets. Well under
// agentproc's production default so the assertion "peak ≤ the budget" is a
// real bound rather than a number no run could reach, and well over what the
// sentry + a busybox payload need so the run doesn't OOM instead of exiting.
const actualsLimitMB = 512

// TestIntegration_RunActuals_MeasuredFromCgroup runs a payload that spends
// real CPU inside the jail and checks that its cost comes back on the
// LaunchedRun: cpu time above zero, peak memory above zero and inside the
// run's own ceiling.
func TestIntegration_RunActuals_MeasuredFromCgroup(t *testing.T) {
	requireRunsc(t)

	cfg := minimalConfig(t)
	cfg.MemoryLimitMB = actualsLimitMB
	// Busybox work that costs measurable CPU and touches memory, then exits
	// on its own — dd through /dev/null spends its time in the sentry's
	// syscall path, which is exactly the cost this run should be charged for.
	cfg.Argv = []string{"/bin/sh", "-c", "dd if=/dev/zero of=/dev/null bs=1M count=512 2>/dev/null; echo measured"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()
	defer run.Close()

	out, err := runToCompletion(t, run)
	if err != nil {
		t.Fatalf("run: %v (output: %s)", err, out)
	}
	if run.OOMKilled() {
		t.Fatalf("run OOM-killed under a %d MB ceiling; the payload outgrew the test's headroom", actualsLimitMB)
	}

	got := run.Actuals()

	if got.CPUUsec == nil {
		t.Error("no cpu-time actual; cpu.stat should be readable on every cgroup v2 group")
	} else if *got.CPUUsec <= 0 {
		t.Errorf("cpu time = %d µs for a run that did real work, want > 0", *got.CPUUsec)
	}

	// A pre-5.19 kernel has no memory.peak, and reporting NULL there is the
	// designed behavior — not something this test should fail the box over.
	// The run itself is unaffected either way, which the assertions above
	// have already established.
	if got.PeakMemMB == nil {
		t.Log("memory.peak unavailable on this kernel (needs >= 5.19); peak-memory actual is NULL by design")
		return
	}
	if *got.PeakMemMB <= 0 {
		t.Errorf("peak memory = %d MB for a live jail, want > 0", *got.PeakMemMB)
	}
	if *got.PeakMemMB > actualsLimitMB {
		t.Errorf("peak memory = %d MB exceeds the run's own %d MB ceiling — the ceiling is enforced by memory.max, so this cannot happen with a correctly-scoped read",
			*got.PeakMemMB, actualsLimitMB)
	}
}

// TestIntegration_RunActuals_CapturedOnKill proves the kill path measures
// too: a run torn down mid-flight still consumed memory and CPU, and the
// read happens on the same exit path — before the cgroup is removed — so its
// cost is attributable rather than lost.
func TestIntegration_RunActuals_CapturedOnKill(t *testing.T) {
	requireRunsc(t)

	cfg := minimalConfig(t)
	cfg.MemoryLimitMB = actualsLimitMB
	// A payload that never ends on its own: the only way out is the kill.
	cfg.Argv = []string{"/bin/sh", "-c", "while true; do :; done"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()

	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let the payload burn a moment of CPU so the measurement has something
	// to report, then kill it.
	time.Sleep(500 * time.Millisecond)
	if err := run.Close(); err != nil {
		t.Fatalf("Close (kill): %v", err)
	}
	// Wait after the kill: the exit is non-nil (killed), and Wait is where
	// the actuals arrive.
	_ = run.Wait()

	got := run.Actuals()
	if got.CPUUsec == nil || *got.CPUUsec <= 0 {
		t.Errorf("killed run cpu time = %v, want > 0 — a cancelled run still cost real CPU", got.CPUUsec)
	}
	if got.PeakMemMB == nil {
		t.Log("memory.peak unavailable on this kernel (needs >= 5.19); peak-memory actual is NULL by design")
		return
	}
	if *got.PeakMemMB <= 0 || *got.PeakMemMB > actualsLimitMB {
		t.Errorf("killed run peak memory = %d MB, want > 0 and <= %d", *got.PeakMemMB, actualsLimitMB)
	}
}
