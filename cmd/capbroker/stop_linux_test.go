//go:build linux

package capbroker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// processGroupHelperEnv gates the TestMain branch that runs
// runProcessGroupHelper in place of the test suite.
const processGroupHelperEnv = "CAPBROKER_TEST_PROCESS_GROUP"

// parentWatchHelperEnv gates the TestMain branch that runs
// runParentWatchHelper, and names the role the re-invoked binary plays.
const parentWatchHelperEnv = "CAPBROKER_TEST_PARENT_WATCH"

const (
	parentWatchIntermediate = "intermediate"
	parentWatchBroker       = "broker"
)

// parentWatchPipeFD is where both parent-watch helpers find the write end of
// the test's pipe: the first descriptor past stdio, as ExtraFiles hands it.
const parentWatchPipeFD = 3

// runProcessGroupHelper moves into a process group as a starting broker does,
// spawns a child, and prints its own pid, its group, and the child's group.
func runProcessGroupHelper() int {
	if err := leaveCallerProcessGroup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	childGroup, err := syscall.Getpgid(child.Process.Pid)
	_ = child.Process.Kill()
	_ = child.Wait()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("%d %d %d\n", os.Getpid(), syscall.Getpgrp(), childGroup)
	return 0
}

// A broker leaves the process group it was started in, and what it spawns
// stays in the broker's: a signal sent to the starter's group reaches neither.
func TestLeaveCallerProcessGroup_BrokerAndItsChildrenLeaveTheCallersGroup(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), processGroupHelperEnv+"=1")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	var pid, group, childGroup int
	if _, err := fmt.Sscan(string(out), &pid, &group, &childGroup); err != nil {
		t.Fatalf("helper printed %q: %v", out, err)
	}
	if pid != cmd.Process.Pid {
		t.Fatalf("helper reported pid %d, started as %d", pid, cmd.Process.Pid)
	}
	if group != pid {
		t.Errorf("helper's process group = %d, want its own pid %d", group, pid)
	}
	if group == syscall.Getpgrp() {
		t.Errorf("helper's process group = %d, the group of the process that started it", group)
	}
	if childGroup != group {
		t.Errorf("the helper's child is in process group %d, want the helper's %d", childGroup, group)
	}
}

// runParentWatchHelper plays one of the two processes under
// TestServe_ParentExitEndsTheRuntimesAndTheBroker.
func runParentWatchHelper(role string) int {
	var err error
	switch role {
	case parentWatchIntermediate:
		err = parentWatchIntermediateMain()
	case parentWatchBroker:
		err = parentWatchBrokerMain()
	default:
		err = fmt.Errorf("unknown parent-watch helper role %q", role)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "parent-watch helper ("+role+"):", err)
		return 1
	}
	return 0
}

// parentWatchIntermediateMain stands in for an orchestrator that is killed: it
// starts the broker helper, waits until that one is serving with its runtime
// up, and exits without stopping it.
func parentWatchIntermediateMain() error {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), parentWatchHelperEnv+"="+parentWatchBroker)
	cmd.ExtraFiles = []*os.File{os.NewFile(parentWatchPipeFD, "pipe")}
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		_ = cmd.Process.Kill()
		return fmt.Errorf("the broker helper did not come up: read %q, %v", line, err)
	}
	return nil
}

// parentWatchBrokerMain is a broker from its group move on, as runBroker runs
// it, over fake ops: one stand-in runtime launched through the real RPC, then
// serve until something stops it. The stand-in holds the test's pipe, and so
// does this process, so the pipe reads end-of-file only when both are gone.
func parentWatchBrokerMain() error {
	if err := leaveCallerProcessGroup(); err != nil {
		return err
	}
	parent := os.Getppid()
	pipe := os.NewFile(parentWatchPipeFD, "pipe")
	// The test kills this process's group when it is done, whatever happened.
	if _, err := fmt.Fprintln(pipe, os.Getpid()); err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "capbroker-parent-watch")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	launchRuntime = func(ctx context.Context, _, _ string, _ int, stdio *os.File, stderr io.Writer) (jailedRuntime, error) {
		cmd := exec.CommandContext(ctx, "sleep", "600")
		cmd.Stdin = stdio
		cmd.Stdout = stdio
		cmd.Stderr = stderr
		cmd.ExtraFiles = []*os.File{pipe}
		err := cmd.Start()
		_ = stdio.Close()
		if err != nil {
			return nil, err
		}
		return &stubRuntime{cmd: cmd}, nil
	}
	prepareBundle = func(context.Context, sandbox.LaunchParams) (string, error) {
		return os.MkdirTemp(dir, "bundle")
	}
	armJailNetwork = func(context.Context, string, string) error { return nil }
	stdioSocketPath = func(containerID string) string {
		return filepath.Join(dir, containerID+"-stdio.sock")
	}

	sockPath := filepath.Join(dir, "broker.sock")
	l, err := listen(sockPath)
	if err != nil {
		return err
	}
	served := make(chan error, 1)
	go func() { served <- serve(NewServer(&fakeOps{}), l, parent) }()

	run, err := Dial(sockPath).LaunchRun(context.Background(), validLaunchParams("c-parent-watch"))
	if err != nil {
		return err
	}
	if err := run.Start(); err != nil {
		return err
	}
	fmt.Println("ready")
	return <-served
}

// A broker whose parent exits without stopping it ends the runtimes it
// supervises and exits. Nothing signals it: the intermediate process only
// exits, as a killed orchestrator does.
func TestServe_ParentExitEndsTheRuntimesAndTheBroker(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), parentWatchHelperEnv+"="+parentWatchIntermediate)
	cmd.ExtraFiles = []*os.File{w}
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	_ = w.Close()

	// The broker helper's first write is its pid, which is also its process
	// group and its stand-in's.
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(r)
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read the broker helper's pid: %v (intermediate: %v)", err, runErr)
	}
	brokerPID, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("the broker helper wrote %q for its pid", line)
	}
	t.Cleanup(func() { _ = syscall.Kill(-brokerPID, syscall.SIGKILL) })
	if runErr != nil {
		t.Fatalf("intermediate: %v", runErr)
	}

	// The intermediate has exited. End-of-file needs the broker helper and
	// its stand-in runtime both gone.
	if _, err := io.Copy(io.Discard, rd); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal("5s after its parent exited, the broker or its runtime still holds the pipe")
		}
		t.Fatalf("read the pipe: %v", err)
	}
}
