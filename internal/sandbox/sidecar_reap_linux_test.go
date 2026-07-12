//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/procname"
)

// sidecarReapHelperEnv gates this test binary's re-exec branch — the same
// idiom internal/procname's own test uses (procname_linux_test.go) and for
// the same reason: a re-exec'd child sets its comm via SetTitle and then
// blocks, so this suite can find and reap it by name without a built
// triagefactory binary.
const sidecarReapHelperEnv = "TF_TEST_SIDECAR_REAP_HELPER"

// TestMain intercepts the re-exec branch before the normal test suite runs.
// This is the only TestMain in the package's default (non-"integration"
// build-tagged) test binary — internal/sandbox/integration_linux_test.go
// defines its own, but under a separate "integration" build tag that a
// plain `go test ./internal/sandbox/...` never compiles in.
func TestMain(m *testing.M) {
	if os.Getenv(sidecarReapHelperEnv) != "" {
		procname.SetTitle(sidecarCommName)
		select {} // blocks until SIGKILLed (reapOrphanSidecars, or test cleanup)
	}
	os.Exit(m.Run())
}

// procAlive reports whether pid still exists in the process table — true
// for a SIGKILLed-but-not-yet-reaped zombie too, since kill(pid, 0) only
// answers "does this pid still have an entry," not "is it running." Only
// safe to use here for the UNRELATED process, which nothing in this test
// ever waits on; the sidecar-comm process is instead confirmed dead by
// actually reaping it (see waitKilled below), because a zombie would
// otherwise make this always report "still alive."
func procAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func waitForComm(t *testing.T, pid int, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err == nil && strings.TrimRight(string(b), "\n") == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d never reported comm %q", pid, want)
}

// TestReapOrphanSidecars_KillsMatchingProcessesOnly pins reapOrphanSidecars'
// whole contract: a process wearing the sidecar's fixed comm is killed, an
// unrelated process survives untouched. This is the mechanism behind the
// acceptance criterion "a killed-mid-run executor leaves no orphaned
// sidecar after the next boot's ReapOrphans" — the re-exec'd helper here
// stands in for a sidecar left behind by a crashed broker.
func TestReapOrphanSidecars_KillsMatchingProcessesOnly(t *testing.T) {
	sidecar := exec.Command(os.Args[0])
	sidecar.Env = append(os.Environ(), sidecarReapHelperEnv+"=1")
	if err := sidecar.Start(); err != nil {
		t.Fatalf("start sidecar-comm helper: %v", err)
	}
	t.Cleanup(func() {
		_ = sidecar.Process.Kill()
		_, _ = sidecar.Process.Wait()
	})

	unrelated := exec.Command("sleep", "30")
	if err := unrelated.Start(); err != nil {
		t.Fatalf("start unrelated helper: %v", err)
	}
	t.Cleanup(func() {
		_ = unrelated.Process.Kill()
		_, _ = unrelated.Process.Wait()
	})

	waitForComm(t, sidecar.Process.Pid, sidecarCommName, 2*time.Second)

	reapOrphanSidecars()

	// A SIGKILLed-but-unreaped child is a zombie, which still holds a pid
	// table entry — so confirm the kill by actually reaping it (Wait
	// returning a SIGKILL exit status), not by a kill(pid, 0) liveness
	// probe, which would report "alive" for the zombie either way.
	waited := make(chan error, 1)
	go func() { waited <- sidecar.Wait() }()
	select {
	case err := <-waited:
		if err == nil {
			t.Error("sidecar-comm process exited cleanly; want a SIGKILL-caused error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sidecar-comm process was not reaped after reapOrphanSidecars — it was not killed")
	}

	if !procAlive(unrelated.Process.Pid) {
		t.Error("unrelated process was killed by reapOrphanSidecars")
	}
}
