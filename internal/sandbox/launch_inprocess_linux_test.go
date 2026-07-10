//go:build linux && integration

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
)

// inProcessLauncher is the integration suite's stand-in for the cap-broker.
// Production always launches through the broker — a separate process this
// package can't import without a cycle — so to drive Wrap() end-to-end
// against a real runsc, the tests install this launcher (via TestMain), which
// runs the same broker launch primitives (PrepareBundle + LaunchSupervised)
// IN THIS process over a socketpair. It exercises the real production launch
// shape (a supervised runtime with its stdio handed across a passed-through
// socket), not a bespoke test path — the only difference from the broker is
// that there is no process boundary, which the integration box doesn't need
// to cross. Test-only: the shipped binary has no in-process launcher.
type inProcessLauncher struct{}

func (inProcessLauncher) LaunchRun(ctx context.Context, p LaunchParams) (LaunchedRun, error) {
	// Single trust domain (this test process IS the capability holder), so
	// build the bundle directly — what the broker does once it has validated
	// the params. No ValidateLaunchParams here: the integration payloads
	// deliberately use non-pinned argv (/usr/bin/env, /bin/cat, ...).
	bundleDir, err := PrepareBundle(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("sandbox: prepare bundle: %w", err)
	}

	// A connected AF_UNIX pair standing in for the broker's socket-by-path
	// handoff: one end becomes the runtime's stdin+stdout, the other is the
	// orchestrator's drivable end.
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		_ = RemoveBundle(bundleDir)
		return nil, fmt.Errorf("sandbox: socketpair: %w", err)
	}
	runtimeFile := os.NewFile(uintptr(fds[0]), "runtime-stdio")
	orchFile := os.NewFile(uintptr(fds[1]), "orch-stdio")
	conn, err := net.FileConn(orchFile)
	_ = orchFile.Close() // FileConn dups; drop our copy
	if err != nil {
		_ = runtimeFile.Close()
		_ = RemoveBundle(bundleDir)
		return nil, fmt.Errorf("sandbox: wrap orchestrator socket: %w", err)
	}
	return &inProcessLaunch{
		ctx:         ctx,
		params:      p,
		bundleDir:   bundleDir,
		runtimeFile: runtimeFile,
		conn:        conn.(*net.UnixConn),
		stderr:      &testSyncBuf{},
	}, nil
}

// inProcessLaunch is the LaunchedRun the test launcher returns: the
// orchestrator's end of the passed-through socket plus a SupervisedRuntime
// launched in this process at Start.
type inProcessLaunch struct {
	ctx         context.Context
	params      LaunchParams
	bundleDir   string
	runtimeFile *os.File
	conn        *net.UnixConn
	stderr      *testSyncBuf

	rt        *SupervisedRuntime
	oom       bool
	oomOnce   sync.Once
	started   bool
	closeOnce sync.Once
}

func (l *inProcessLaunch) Start() error {
	rt, err := LaunchSupervised(l.ctx, l.bundleDir, l.params.ContainerID, l.params.MemoryLimitMB, l.runtimeFile, l.stderr)
	l.runtimeFile = nil // LaunchSupervised closed it
	if err != nil {
		return err
	}
	l.rt = rt
	l.started = true
	return nil
}

func (l *inProcessLaunch) Stdin() io.WriteCloser { return testStdinWriter{l.conn} }
func (l *inProcessLaunch) Stdout() io.Reader     { return l.conn }
func (l *inProcessLaunch) Stderr() string        { return l.stderr.String() }
func (l *inProcessLaunch) Pid() int              { return 0 }

func (l *inProcessLaunch) Wait() error {
	oom, err := l.rt.Wait()
	l.oomOnce.Do(func() { l.oom = oom })
	return err
}

func (l *inProcessLaunch) OOMKilled() bool { return l.oom }

func (l *inProcessLaunch) Close() error {
	l.closeOnce.Do(func() {
		if l.rt != nil {
			_ = l.rt.Kill()
		}
		if l.conn != nil {
			_ = l.conn.Close()
		}
		if l.runtimeFile != nil { // Start never ran
			_ = l.runtimeFile.Close()
		}
		_ = RemoveBundle(l.bundleDir)
	})
	return nil
}

// testStdinWriter exposes only the write side of the shared stdio socket as
// Stdin, half-closing (CloseWrite) on Close rather than tearing down the
// whole connection — mirroring the broker client's unixWriteCloser.
type testStdinWriter struct{ c *net.UnixConn }

func (w testStdinWriter) Write(p []byte) (int, error) { return w.c.Write(p) }
func (w testStdinWriter) Close() error                { return w.c.CloseWrite() }

// testSyncBuf is a concurrency-safe buffer for capturing the supervised
// runtime's stderr, which os/exec writes from its own goroutine while the
// test reads it via Stderr() after Wait.
type testSyncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *testSyncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *testSyncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
