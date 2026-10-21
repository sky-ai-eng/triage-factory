//go:build linux

package agentproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// installInProcessLauncher lets sandbox.Wrap launch a jail from this test
// process. Production launches every run through the cap-broker, whose
// argv pin admits only the tool host's serve verb, so a live test that runs
// a script in the jail cannot go through it. This launcher runs the same
// launch primitives the broker runs once it has validated a launch
// (PrepareBundle and LaunchSupervised, with the runtime's stdio across a
// socket), the way internal/sandbox's integration suite does; the only
// difference is that there is no process boundary to cross.
//
// Call it only after the test's skip checks pass: it installs package-level
// state in internal/sandbox for the rest of the test binary.
func installInProcessLauncher(t *testing.T) {
	t.Helper()
	launcherOnce.Do(func() {
		sandbox.SetPrivilegedOps(inProcessOps{PrivilegedOps: sandbox.NewHostOps()})
	})
}

var launcherOnce sync.Once

// inProcessOps is the host's own privileged ops plus the in-process run
// launcher. These tests start no sidecar and capture no run tree, so those
// two seams refuse.
type inProcessOps struct {
	sandbox.PrivilegedOps
}

func (inProcessOps) LaunchSidecar(context.Context, sandbox.SidecarLaunchParams) (sandbox.LaunchedSidecar, error) {
	return nil, errors.New("agentproc test launcher: no sidecar launch")
}

func (inProcessOps) CaptureRunTree(context.Context, sandbox.CaptureParams) ([]byte, error) {
	return nil, errors.New("agentproc test launcher: no run-tree capture")
}

func (inProcessOps) LaunchRun(ctx context.Context, p sandbox.LaunchParams) (sandbox.LaunchedRun, error) {
	bundleDir, err := sandbox.PrepareBundle(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("prepare bundle: %w", err)
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		_ = sandbox.RemoveBundle(bundleDir)
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	runtimeFile := os.NewFile(uintptr(fds[0]), "runtime-stdio")
	testFile := os.NewFile(uintptr(fds[1]), "test-stdio")
	conn, err := net.FileConn(testFile)
	_ = testFile.Close()
	if err != nil {
		_ = runtimeFile.Close()
		_ = sandbox.RemoveBundle(bundleDir)
		return nil, fmt.Errorf("wrap test socket: %w", err)
	}
	return &inProcessLaunch{
		ctx:         ctx,
		params:      p,
		bundleDir:   bundleDir,
		runtimeFile: runtimeFile,
		conn:        conn.(*net.UnixConn),
	}, nil
}

// inProcessLaunch is the LaunchedRun inProcessOps returns: this process's
// end of the stdio socket plus a supervised runtime started at Start.
type inProcessLaunch struct {
	ctx         context.Context
	params      sandbox.LaunchParams
	bundleDir   string
	runtimeFile *os.File
	conn        *net.UnixConn
	stderr      lockedBuffer

	rt        *sandbox.SupervisedRuntime
	waitOnce  sync.Once
	oom       bool
	actuals   sandbox.RunActuals
	waitErr   error
	closeOnce sync.Once
}

func (l *inProcessLaunch) Start() error {
	rt, err := sandbox.LaunchSupervised(l.ctx, l.bundleDir, l.params.ContainerID, l.params.MemoryLimitMB, l.runtimeFile, &l.stderr)
	l.runtimeFile = nil // LaunchSupervised closed it
	if err != nil {
		return err
	}
	l.rt = rt
	return nil
}

func (l *inProcessLaunch) Stdin() io.WriteCloser { return halfCloser{l.conn} }
func (l *inProcessLaunch) Stdout() io.Reader     { return l.conn }
func (l *inProcessLaunch) Stderr() string        { return l.stderr.String() }
func (l *inProcessLaunch) Pid() int              { return 0 }

// Wait reaps the runtime once; every caller gets the first reap's result,
// because only that one read the cgroup before it was removed.
func (l *inProcessLaunch) Wait() error {
	l.waitOnce.Do(func() {
		oom, err := l.rt.Wait()
		l.oom, l.actuals, l.waitErr = oom, l.rt.Actuals(), err
	})
	return l.waitErr
}

func (l *inProcessLaunch) OOMKilled() bool             { return l.oom }
func (l *inProcessLaunch) Actuals() sandbox.RunActuals { return l.actuals }

func (l *inProcessLaunch) Close() error {
	l.closeOnce.Do(func() {
		if l.rt != nil {
			// Only the reap removes the run's cgroup.
			_ = l.rt.Kill()
			_ = l.Wait()
		}
		_ = l.conn.Close()
		if l.runtimeFile != nil { // Start never ran
			_ = l.runtimeFile.Close()
		}
		_ = sandbox.RemoveBundle(l.bundleDir)
	})
	return nil
}

// halfCloser is the write side of the stdio socket; Close half-closes it,
// as the broker client's stdin does.
type halfCloser struct{ c *net.UnixConn }

func (w halfCloser) Write(p []byte) (int, error) { return w.c.Write(p) }
func (w halfCloser) Close() error                { return w.c.CloseWrite() }

// lockedBuffer collects the runtime's stderr, which os/exec writes from its
// own goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *lockedBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *lockedBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
