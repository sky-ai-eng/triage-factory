//go:build linux

package capbroker

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// dialStdioTimeout bounds the broker's dial of the orchestrator's per-run
// stdio socket. The orchestrator listens before it issues LaunchRun, so the
// dial connects at once; the timeout only guards a stale/misconfigured path
// so a broker goroutine fails fast instead of blocking forever.
const dialStdioTimeout = 5 * time.Second

// supervisedRuntime is the broker's handle on one started runsc child:
// block for its exit (with OOM attribution) and SIGKILL it. sandbox's
// *SupervisedRuntime satisfies this; tests substitute a stand-in via
// launchRuntime so the RPC + registry + socket-passthrough wiring can be
// exercised without a real gVisor host.
type supervisedRuntime interface {
	Wait() (oomKilled bool, err error)
	Kill() error
}

// launchRuntime is the seam the broker execs the runtime through. The
// default hands the socket fd to sandbox.LaunchSupervised, which wires it
// as runsc's stdin+stdout and closes the broker's copy — the bytes never
// enter this process. A var so tests can swap in a stand-in runtime.
var launchRuntime = func(ctx context.Context, p sandbox.LaunchParams, stdio *os.File, stderr io.Writer) (supervisedRuntime, error) {
	return sandbox.LaunchSupervised(ctx, p, stdio, stderr)
}

// runEntry is one in-flight supervised run. done is closed once the
// supervising goroutine has reaped the child; oom/waitErr are written
// before that close and read only after it, so no further lock is needed
// to observe them.
type runEntry struct {
	rt      supervisedRuntime
	done    chan struct{}
	oom     bool
	waitErr error
}

// launchRun dials the orchestrator's per-run stdio socket, hands its fd to
// the runtime as stdin+stdout, execs+supervises runsc, and registers the
// run so a later WaitRun/KillRun can reach it. It returns as soon as runsc
// is started — the RPC is short; the supervision runs in a goroutine and
// the exit status is fetched later via WaitRun.
//
// The socket fd is handed to the runtime and the broker's own copies are
// closed (the dialed conn here, plus the dup inside LaunchSupervised) so
// the run's bytes never enter the broker — there is no read on the socket
// anywhere on this path.
func (s *Server) launchRun(a launchRunArgs) (any, error) {
	conn, err := net.DialTimeout("unix", a.StdioSocketPath, dialStdioTimeout)
	if err != nil {
		return nil, fmt.Errorf("capbroker: dial stdio socket %s: %w", a.StdioSocketPath, err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("capbroker: stdio socket is not a unix conn")
	}
	// File() returns a dup of the connection's fd; the runtime inherits it
	// as stdin+stdout. LaunchSupervised closes this dup after Start; we
	// close the connection itself right after, leaving the runsc child the
	// sole holder of the socket fd.
	f, err := uc.File()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("capbroker: take stdio fd: %w", err)
	}

	// baseCtx (not a per-call ctx) ties the runtime's lifetime to the
	// broker: a broker Shutdown cancels it and SIGKILLs the child; explicit
	// cancellation still goes through KillRun.
	rt, err := launchRuntime(s.baseCtx, sandbox.LaunchParams{
		RunID:         a.RunID,
		BundleDir:     a.BundleDir,
		ContainerID:   a.ContainerID,
		MemoryLimitMB: a.MemoryLimitMB,
	}, f, os.Stderr)
	// The runtime now holds its own inherited dup (on success); drop the
	// broker's remaining reference to the socket regardless of outcome.
	_ = conn.Close()
	if err != nil {
		return nil, fmt.Errorf("capbroker: launch runtime: %w", err)
	}

	entry := &runEntry{rt: rt, done: make(chan struct{})}
	s.runsMu.Lock()
	s.runs[a.RunID] = entry
	s.runsMu.Unlock()

	// Supervise in a goroutine so the child is reaped (and its cgroup
	// removed) even if the orchestrator never calls WaitRun. WaitRun just
	// fetches the recorded result. Deliberately NOT tracked by s.inflight
	// (the connection-drain WaitGroup): a run can outlive many RPC
	// connections, and Shutdown must not block on the whole run. On
	// Shutdown baseCtx is canceled, which SIGKILLs runsc (cmd.Cancel), so
	// this goroutine unwinds on its own.
	go func() {
		oom, werr := rt.Wait()
		entry.oom = oom
		entry.waitErr = werr
		close(entry.done)
	}()

	return emptyResult{}, nil
}

// waitRun blocks until the run's supervising goroutine has reaped the
// child, then returns its exit status + OOM attribution and drops the
// registry entry. In practice the orchestrator only calls this after it
// has seen its stdio socket EOF (the runtime exited), so done is already
// closed or about to be.
func (s *Server) waitRun(ctx context.Context, a waitRunArgs) (any, error) {
	s.runsMu.Lock()
	entry := s.runs[a.RunID]
	s.runsMu.Unlock()
	if entry == nil {
		return nil, fmt.Errorf("capbroker: waitrun: unknown run %q", a.RunID)
	}

	select {
	case <-entry.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	s.runsMu.Lock()
	delete(s.runs, a.RunID)
	s.runsMu.Unlock()

	res := waitRunResult{OOMKilled: entry.oom}
	if entry.waitErr != nil {
		res.ExitError = entry.waitErr.Error()
	}
	return res, nil
}

// killRun SIGKILLs the run's runtime. Idempotent: an unknown run (already
// exited and reaped) is a no-op success, so a cancel that races the
// natural exit is harmless.
func (s *Server) killRun(a killRunArgs) (any, error) {
	s.runsMu.Lock()
	entry := s.runs[a.RunID]
	s.runsMu.Unlock()
	if entry == nil {
		return emptyResult{}, nil
	}
	_ = entry.rt.Kill()
	return emptyResult{}, nil
}
