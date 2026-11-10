//go:build windows

package agentproc

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// terminateExitCode is the exit status every process in a terminated
	// job reports.
	terminateExitCode = 1

	// jobDrainTimeout bounds how long cancel waits for the job to report no
	// live processes after TerminateJobObject, which returns before the
	// kernel has finished tearing the processes down. Teardown callers delete
	// the run's directory right after, and a process still holding a handle
	// inside it fails that delete.
	jobDrainTimeout = 2 * time.Second

	jobDrainPoll = 20 * time.Millisecond

	// windowsWaitDelay bounds Wait after the job is terminated, so a pipe
	// holder that somehow survived the termination cannot hang it.
	windowsWaitDelay = 3 * time.Second
)

// jobBasicAccounting mirrors JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, which
// x/sys/windows does not define.
type jobBasicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// procTree is a Job Object holding the agent process and everything it
// spawns. Windows has no process group that can be signalled as a unit, so
// the job is what makes a cancel reach grandchildren.
//
// mu guards job and attached, which cmd.Cancel reads from the exec watcher
// goroutine while Start and Wait write them.
type procTree struct {
	cmd *exec.Cmd

	mu       sync.Mutex
	job      windows.Handle
	attached bool
}

// configureProcessTree creates the job and routes cmd's cancel through it.
// The job carries JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so the kernel ends the
// whole tree when the last handle to the job closes, including when TF itself
// dies without running a cancel.
//
// A job that cannot be created is logged and leaves the tree unattached;
// cancel then kills only the direct child.
func configureProcessTree(cmd *exec.Cmd) *procTree {
	t := &procTree{cmd: cmd}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
	cmd.WaitDelay = windowsWaitDelay
	cmd.Cancel = t.cancel

	job, err := newKillOnCloseJob()
	if err != nil {
		agentprocLog.Warn("create job object failed; cancel will kill only the direct child", "error", err)
		return t
	}
	t.job = job
	return t
}

func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return job, nil
}

// attach assigns the started process to the job. It must run right after
// cmd.Start succeeds.
//
// There is a window between CreateProcess returning and the assignment in
// which the child can already have spawned its own children, and those escape
// the job. That window is reachable: a launcher shim such as Volta or fnm
// spawns the real node immediately. It is accepted because the only
// alternative is starting the child suspended and resuming it after the
// assignment, which os/exec cannot express without reimplementing process
// creation. A child that escapes this way is not killed by cancel, but the
// agent's own descendants, spawned after the runtime has loaded, are all
// inside the job.
func (t *procTree) attach(p *os.Process) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return fmt.Errorf("no job object to attach pid %d to", p.Pid)
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", p.Pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.AssignProcessToJobObject(t.job, h); err != nil {
		return fmt.Errorf("AssignProcessToJobObject(%d): %w", p.Pid, err)
	}
	t.attached = true
	return nil
}

// cancel ends the tree. Without a job that holds the process it falls back to
// killing the direct child, which leaves grandchildren running, so it says so.
func (t *procTree) cancel() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.attached || t.job == 0 {
		agentprocLog.Warn("process tree not attached to job object; killing direct child only")
		return t.cmd.Process.Kill()
	}
	if err := windows.TerminateJobObject(t.job, terminateExitCode); err != nil {
		return fmt.Errorf("TerminateJobObject: %w", err)
	}
	t.waitDrained()
	return nil
}

// waitDrained polls until the job reports no active processes or the drain
// timeout passes. Caller holds t.mu.
func (t *procTree) waitDrained() {
	deadline := time.Now().Add(jobDrainTimeout)
	for {
		var info jobBasicAccounting
		err := windows.QueryInformationJobObject(t.job, windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
		if err != nil {
			agentprocLog.Warn("query job object after terminate failed", "error", err)
			return
		}
		if info.ActiveProcesses == 0 {
			return
		}
		if time.Now().After(deadline) {
			agentprocLog.Warn("job object still has active processes after terminate",
				"active", info.ActiveProcesses, "timeout", jobDrainTimeout)
			return
		}
		time.Sleep(jobDrainPoll)
	}
}

// close releases the job handle. Anything still in the job when the handle
// closes, such as a background process the agent left running, is killed by
// KILL_ON_JOB_CLOSE. Safe to call more than once and on a nil tree.
func (t *procTree) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return
	}
	if err := windows.CloseHandle(t.job); err != nil {
		agentprocLog.Warn("close job object failed", "error", err)
	}
	t.job = 0
	t.attached = false
}
