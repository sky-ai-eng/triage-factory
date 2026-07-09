package agentproc

import (
	"context"
	"io"
	"os/exec"
	"testing"
)

// TestExecProc_DrivesSubprocess pins the direct-path runProc adapter: Start,
// then Stdout streams the child's output and Stderr captures its stderr,
// Wait reports the exit, and OOMKilled is false (no cgroup on the direct
// path).
func TestExecProc_DrivesSubprocess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "echo out; echo err 1>&2")
	proc, err := newExecProc(cmd)
	if err != nil {
		t.Fatalf("newExecProc: %v", err)
	}
	if err := proc.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	out, err := io.ReadAll(proc.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if string(out) != "out\n" {
		t.Errorf("stdout = %q, want %q", out, "out\n")
	}
	if proc.Stderr() != "err\n" {
		t.Errorf("stderr = %q, want %q", proc.Stderr(), "err\n")
	}
	if proc.OOMKilled() {
		t.Error("OOMKilled true on the direct (non-cgroup) path")
	}
}

// TestExecProc_WaitReportsExitError pins that a non-zero exit surfaces
// through Wait, matching *exec.Cmd.Wait's contract the callers rely on.
func TestExecProc_WaitReportsExitError(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 3")
	proc, err := newExecProc(cmd)
	if err != nil {
		t.Fatalf("newExecProc: %v", err)
	}
	if err := proc.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, _ = io.ReadAll(proc.Stdout())
	if err := proc.Wait(); err == nil {
		t.Error("Wait returned nil for a nonzero exit")
	}
}
