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

// reExecAsSidecarHelperIfRequested intercepts the re-exec branch this suite
// uses to mint a named, blocking child it can find and reap by comm name.
// Both the default and the integration test binary drive it from their
// respective TestMain — one TestMain compiles per build tag, and the
// integration binary additionally pre-warms the rootfs and installs the
// in-process launcher, so the shared re-exec discipline lives here rather
// than in a single TestMain that only one build could hold. When the gate
// env is unset it returns and the normal suite runs.
func reExecAsSidecarHelperIfRequested() {
	if os.Getenv(sidecarReapHelperEnv) != "" {
		procname.SetTitle(SidecarCommName)
		select {} // blocks until SIGKILLed (reapOrphanSidecars, or test cleanup)
	}
}

// procAlive reports whether pid still exists in the process table — true
// for a SIGKILLed-but-not-yet-reaped zombie too, since kill(pid, 0) only
// answers "does this pid still have an entry," not "is it running." Only
// safe to use for a process nothing in the test ever waits on; a process
// this test itself reaps is instead confirmed dead via Wait (see
// TestReapOrphanSidecars_KillsCommAndUIDMatch), because a zombie would
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

// startSidecarCommHelper re-execs this test binary with its comm set to
// SidecarCommName, optionally dropped to uid/gid via a syscall.Credential
// (nil runs at the caller's own uid). Blocks (via TestMain's re-exec
// branch) until killed.
//
// When cred is non-nil, this execs a COPY of os.Args[0] under a directory
// this test controls the mode of, rather than the original path directly:
// `go test`'s own build cache places the compiled test binary under an
// intermediate directory (e.g. /tmp/go-build<random>/) it creates at mode
// 0700, and the kernel resolves that path AFTER the credential switch — so
// exec-ing the original path as a different, unprivileged uid would EACCES
// on that ancestor regardless of the binary's own permissions (the same
// class of issue withStubSidecarBinary works around for the fake sidecar
// script in launchsidecar_linux_test.go).
func startSidecarCommHelper(t *testing.T, cred *syscall.Credential) *exec.Cmd {
	t.Helper()
	binPath := os.Args[0]
	if cred != nil {
		binPath = copyToAccessibleBinary(t, os.Args[0])
	}
	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(), sidecarReapHelperEnv+"=1")
	if cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sidecar-comm helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	waitForComm(t, cmd.Process.Pid, SidecarCommName, 2*time.Second)
	return cmd
}

// copyToAccessibleBinary copies src into a fresh, world-traversable
// (0755) directory this test owns the permissions of, and returns the
// copy's path.
func copyToAccessibleBinary(t *testing.T, src string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tf-sidecar-reap-helper-")
	if err != nil {
		t.Fatalf("mkdir helper binary dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod helper binary dir: %v", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read test binary %s: %v", src, err)
	}
	dst := dir + "/helper"
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatalf("write helper binary copy: %v", err)
	}
	return dst
}

// TestReapOrphanSidecars_KillsCommAndUIDMatch pins reapOrphanSidecars' whole
// contract: a process wearing the sidecar's fixed comm AND running at a uid
// inside the reserved sidecar band is killed; an unrelated process (neither
// comm nor uid match) survives untouched. This is the mechanism behind the
// acceptance criterion "a killed-mid-run executor leaves no orphaned
// sidecar after the next boot's ReapOrphans" — the re-exec'd helper here
// stands in for a sidecar left behind by a crashed broker. Root-gated: only
// root can actually drop the helper to the synthetic sidecar-band uid.
func TestReapOrphanSidecars_KillsCommAndUIDMatch(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to drop the helper to a sidecar-band uid")
	}

	sidecar := startSidecarCommHelper(t, &syscall.Credential{Uid: SidecarUIDBase, Gid: SidecarUIDBase})

	unrelated := exec.Command("sleep", "30")
	if err := unrelated.Start(); err != nil {
		t.Fatalf("start unrelated helper: %v", err)
	}
	t.Cleanup(func() {
		_ = unrelated.Process.Kill()
		_, _ = unrelated.Process.Wait()
	})

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

// TestReapOrphanSidecars_LeavesWrongUIDCommMatchAlone pins the security
// property the uid-band check exists for: a comm-name match ALONE is not
// enough to be reaped. Without this, any unrelated process that happened
// to (accidentally or deliberately) set its own comm to "tf-sidecar" would
// be SIGKILLed by the next boot's sweep. Needs no root — the helper runs at
// this test process's own uid, which the assertion below confirms is
// outside the reserved band (true for every real dev/CI uid: 0, or a
// low-numbered service account, never 20000-20255).
func TestReapOrphanSidecars_LeavesWrongUIDCommMatchAlone(t *testing.T) {
	if IsSidecarUID(os.Getuid()) {
		t.Skip("test process's own uid falls inside the reserved sidecar band; precondition for this test doesn't hold")
	}

	helper := startSidecarCommHelper(t, nil)

	reapOrphanSidecars()

	if !procAlive(helper.Process.Pid) {
		t.Error("comm-matching process at a non-sidecar uid was killed by reapOrphanSidecars — the uid-band check isn't being enforced")
	}
}
