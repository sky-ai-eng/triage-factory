//go:build windows

package agentproc

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	procTreeHelperEnv  = "TF_PROCTREE_HELPER"
	procTreeHelperTest = "^TestProcTreeHelper$"

	// stillActive is GetExitCodeProcess's answer for a running process.
	stillActive = 259
)

// TestProcTreeHelper is not a test. The tests below re-run the test binary
// with procTreeHelperEnv set to get a process tree they control: the "child"
// role spawns a "grandchild" and publishes its pid, and both then sleep.
func TestProcTreeHelper(t *testing.T) {
	switch os.Getenv(procTreeHelperEnv) {
	case "":
		t.Skip("helper process only")
	case "grandchild":
		time.Sleep(5 * time.Minute)
	case "briefchild":
		time.Sleep(20 * time.Second)
	case "leaver":
		// Wait for the parent to put this process in its job, then leave a
		// descendant holding the inherited stderr pipe and exit cleanly.
		time.Sleep(500 * time.Millisecond)
		gc := exec.Command(os.Args[0], "-test.run="+procTreeHelperTest)
		gc.Env = append(os.Environ(), procTreeHelperEnv+"=briefchild")
		gc.Stderr = os.Stderr
		if err := gc.Start(); err != nil {
			t.Fatalf("start descendant: %v", err)
		}
	case "child":
		gc := exec.Command(os.Args[0], "-test.run="+procTreeHelperTest)
		gc.Env = append(os.Environ(), procTreeHelperEnv+"=grandchild")
		if err := gc.Start(); err != nil {
			t.Fatalf("start grandchild: %v", err)
		}
		pidFile := os.Getenv(procTreeHelperEnv + "_PIDFILE")
		tmp := pidFile + ".tmp"
		if err := os.WriteFile(tmp, []byte(strconv.Itoa(gc.Process.Pid)), 0o600); err != nil {
			t.Fatalf("write pid file: %v", err)
		}
		if err := os.Rename(tmp, pidFile); err != nil {
			t.Fatalf("publish pid file: %v", err)
		}
		time.Sleep(5 * time.Minute)
	default:
		t.Fatalf("unknown helper role %q", os.Getenv(procTreeHelperEnv))
	}
}

func processRunning(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestProcessTree_CancelKillsGrandchildAndFreesCwd runs a child that spawns a
// grandchild through the same configure/attach path a delegated run uses, then
// cancels. Both processes must be gone, and the directory the child ran in
// must be removable, since teardown deletes the run root right after cancel.
func TestProcessTree_CancelKillsGrandchildAndFreesCwd(t *testing.T) {
	workDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run="+procTreeHelperTest)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		procTreeHelperEnv+"=child",
		procTreeHelperEnv+"_PIDFILE="+pidFile,
	)
	proc, err := newExecProc(cmd, configureProcessTree(cmd))
	if err != nil {
		t.Fatalf("newExecProc: %v", err)
	}
	if err := proc.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !proc.tree.attached {
		t.Fatal("process was not attached to the job object")
	}
	childPid := cmd.Process.Pid

	var grandchildPid int
	waitFor(t, "grandchild pid file", 20*time.Second, func() bool {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			return false
		}
		grandchildPid = pid
		return true
	})
	if !processRunning(childPid) || !processRunning(grandchildPid) {
		t.Fatalf("expected child %d and grandchild %d running before cancel", childPid, grandchildPid)
	}

	cancel()
	_ = proc.Wait()

	if processRunning(childPid) {
		t.Errorf("child %d still running after cancel", childPid)
	}
	if processRunning(grandchildPid) {
		t.Errorf("grandchild %d still running after cancel", grandchildPid)
	}

	waitFor(t, "removal of the child's working directory", 5*time.Second, func() bool {
		return os.RemoveAll(workDir) == nil
	})
}

// TestProcessTree_StartFailureReleasesJob pins that a command that never
// starts does not leak its job handle, and that releasing twice is harmless.
func TestProcessTree_StartFailureReleasesJob(t *testing.T) {
	cmd := exec.Command(filepath.Join(t.TempDir(), "no-such-binary.exe"))
	tree := configureProcessTree(cmd)
	if tree.job == 0 {
		t.Fatal("configureProcessTree created no job object")
	}
	proc, err := newExecProc(cmd, tree)
	if err != nil {
		t.Fatalf("newExecProc: %v", err)
	}
	if err := proc.Start(); err == nil {
		t.Fatal("Start of a missing binary succeeded")
	}
	if tree.job != 0 {
		t.Error("job handle still held after a failed Start")
	}
	tree.close()
}

// TestProcessTree_CancelBeforeAttachKillsDirectChild covers the fallback: a
// cancel that finds no attached job kills the direct child instead of
// failing.
func TestProcessTree_CancelBeforeAttachKillsDirectChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run="+procTreeHelperTest)
	cmd.Env = append(os.Environ(), procTreeHelperEnv+"=grandchild")
	tree := configureProcessTree(cmd)
	defer tree.close()
	proc, err := newExecProc(cmd, tree)
	if err != nil {
		t.Fatalf("newExecProc: %v", err)
	}
	// Start the command directly so attach never runs.
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid

	cancel()
	_ = proc.cmd.Wait()
	if processRunning(pid) {
		t.Errorf("direct child %d still running after unattached cancel", pid)
	}
}

// An agent that exits 0 while a descendant still holds its stderr pipe makes
// exec report ErrWaitDelay once the wait delay passes; the run succeeded, so
// Wait reports no error. A non-zero exit still fails.
func TestExecProc_WaitIgnoresWaitDelayAfterCleanExit(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run="+procTreeHelperTest)
	cmd.Env = append(os.Environ(), procTreeHelperEnv+"=leaver")
	proc, err := newExecProc(cmd, configureProcessTree(cmd))
	if err != nil {
		t.Fatalf("newExecProc: %v", err)
	}
	if err := proc.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, proc.Stdout()) }()
	start := time.Now()
	if err := proc.Wait(); err != nil {
		t.Errorf("Wait after a clean exit with a pipe holder = %v, want nil", err)
	}
	if time.Since(start) < windowsWaitDelay-time.Second {
		t.Errorf("Wait returned in %s, before the wait delay: the pipe holder did not exercise ErrWaitDelay", time.Since(start))
	}
}

func TestWaitOutcome_PassesOtherErrorsThrough(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit 3")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected a non-zero exit")
	}
	if got := waitOutcome(cmd, err); got != err {
		t.Errorf("waitOutcome changed a non-zero exit error: %v", got)
	}
	plain := exec.Command("cmd.exe", "/c", "exit 0")
	if err := plain.Run(); err != nil {
		t.Fatal(err)
	}
	if got := waitOutcome(plain, exec.ErrWaitDelay); got != nil {
		t.Errorf("waitOutcome(ErrWaitDelay after exit 0) = %v, want nil", got)
	}
	if got := waitOutcome(cmd, exec.ErrWaitDelay); got == nil {
		t.Error("waitOutcome swallowed ErrWaitDelay after a non-zero exit")
	}
}

// A cancel that lands between Start and attach kills the direct child at once
// and records itself; attach then finds the process already gone, cannot join
// it to the job, and still returns no error, since there is nothing to warn
// about in a run that was cancelled.
func TestProcessTree_AttachAfterCancelEndsTheTree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run="+procTreeHelperTest)
	cmd.Env = append(os.Environ(), procTreeHelperEnv+"=grandchild")
	tree := configureProcessTree(cmd)
	defer tree.close()
	proc, err := newExecProc(cmd, tree)
	if err != nil {
		t.Fatalf("newExecProc: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid

	if err := tree.cancel(); err != nil {
		t.Fatalf("cancel before attach: %v", err)
	}
	tree.mu.Lock()
	cancelled := tree.cancelled
	tree.mu.Unlock()
	if !cancelled {
		t.Fatal("cancel before attach was not recorded")
	}
	if err := tree.attach(cmd.Process); err != nil {
		t.Fatalf("attach after cancel returned %v, want nil", err)
	}
	_ = proc.cmd.Wait()
	if processRunning(pid) {
		t.Errorf("child %d still running after cancel and attach", pid)
	}
}

// When the cancelled process cannot join a job at all, attach kills it itself.
func TestProcessTree_AttachAfterCancelWithoutJobKillsTheProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run="+procTreeHelperTest)
	cmd.Env = append(os.Environ(), procTreeHelperEnv+"=grandchild")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	tree := &procTree{cmd: cmd, cancelled: true}

	if err := tree.attach(cmd.Process); err != nil {
		t.Fatalf("attach of a cancelled process with no job returned %v, want nil", err)
	}
	_ = cmd.Wait()
	if processRunning(pid) {
		t.Errorf("process %d still running after attach of a cancelled tree", pid)
	}
}
