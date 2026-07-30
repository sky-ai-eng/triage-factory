//go:build linux

package capbroker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// acceptTimeout bounds how long brokerRun.Start waits for the broker to
// dial its stdio socket. The broker dials during the LaunchRun RPC, which
// has already returned by the time we Accept, so the connection is queued
// and Accept returns at once; the deadline only guards a broker that
// reported success without dialing, so it fails fast instead of hanging.
var acceptTimeout = 10 * time.Second

// stdioSocketPath is the per-run unix socket the orchestrator listens on
// and the broker dials to hand its fd to the runtime. Keyed by the runsc
// container id (unique per Wrap) under the same owner-only /run/tf dir the
// broker + agenthost sockets live in. A var so tests can redirect it to a
// writable temp dir (production /run/tf needs root).
var stdioSocketPath = func(containerID string) string {
	return filepath.Join(socketDir, containerID+"-stdio.sock")
}

// LaunchRun (broker client) begins a socket-by-path launch: it opens the
// per-run stdio listener the broker will dial, and returns a brokerRun
// whose Start issues the launch RPC and accepts the runtime's stdio end.
// The runsc child is exec'd and supervised inside the cap-broker; this
// process only ever reads/writes its own end of the socket.
func (c *IPCClient) LaunchRun(ctx context.Context, p sandbox.LaunchParams) (sandbox.LaunchedRun, error) {
	socketPath := stdioSocketPath(p.ContainerID)
	l, err := listen(socketPath)
	if err != nil {
		return nil, fmt.Errorf("capbroker: listen stdio socket: %w", err)
	}
	return &brokerRun{
		client:     c,
		ctx:        ctx,
		params:     p,
		socketPath: socketPath,
		listener:   l,
	}, nil
}

// brokerRun is the orchestrator-side LaunchedRun for a broker-supervised
// runsc child. Its Stdin/Stdout are the orchestrator's end of the
// passed-through stdio socket; Wait and cancellation are RPCs to the
// broker, which owns the child.
type brokerRun struct {
	client     *IPCClient
	ctx        context.Context
	params     sandbox.LaunchParams
	socketPath string
	listener   net.Listener

	conn        *net.UnixConn
	watchCancel context.CancelFunc
	started     bool

	oom       bool               // set by Wait, read by OOMKilled (same goroutine)
	actuals   sandbox.RunActuals // set by Wait, read by Actuals (same goroutine)
	closeOnce sync.Once
}

func (r *brokerRun) Start() error {
	// Ask the broker to launch runsc wired to our socket. The RPC carries
	// only the validated launch DATA (the broker builds the spec from it);
	// stamp in the per-run stdio socket path we're listening on so the
	// broker knows where to dial. The RPC returns once runsc has been exec'd
	// and the broker has dropped its stdio fd.
	params := r.params
	params.StdioSocketPath = r.socketPath
	if err := r.client.call(r.ctx, methodLaunchRun, launchRunArgs{Params: params}, nil); err != nil {
		r.cleanupListener()
		return err
	}

	// Accept our end of the connection the broker dialed. One connection
	// per run, so the listener is done after this.
	if ul, ok := r.listener.(*net.UnixListener); ok {
		_ = ul.SetDeadline(time.Now().Add(acceptTimeout))
	}
	conn, err := r.listener.Accept()
	r.cleanupListener()
	if err != nil {
		// The broker started a child but we can't wire its stdio — tell it
		// to kill the run so it doesn't leak.
		_ = r.client.call(context.Background(), methodKillRun, killRunArgs{ContainerID: r.params.ContainerID}, nil)
		return fmt.Errorf("capbroker: accept runtime stdio: %w", err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		_ = r.client.call(context.Background(), methodKillRun, killRunArgs{ContainerID: r.params.ContainerID}, nil)
		return fmt.Errorf("capbroker: accepted stdio is not a unix conn")
	}
	r.conn = uc
	r.started = true

	// Translate run-context cancellation into a kill RPC — the broker owns
	// the child, so the orchestrator can't SIGKILL it directly. The watcher
	// stops when Wait or Close runs.
	watchCtx, cancel := context.WithCancel(context.Background())
	r.watchCancel = cancel
	go func() {
		select {
		case <-r.ctx.Done():
			_ = r.client.call(context.Background(), methodKillRun, killRunArgs{ContainerID: r.params.ContainerID}, nil)
		case <-watchCtx.Done():
		}
	}()
	return nil
}

func (r *brokerRun) Stdin() io.WriteCloser { return unixWriteCloser{r.conn} }
func (r *brokerRun) Stdout() io.Reader     { return r.conn }

// Stderr is empty: the broker keeps runsc's stderr on its own side (gVisor
// boot logs, surfaced to the broker's logs) — it is not the agent's
// payload channel.
func (r *brokerRun) Stderr() string { return "" }

// Pid is 0: the runtime is the broker's child, not reachable from this
// (orchestrator) process.
func (r *brokerRun) Pid() int { return 0 }

// Wait fetches the run's exit status via the WaitRun RPC. Called exactly
// once, after the caller has drained Stdout to EOF (the run has already
// exited), so it returns promptly. Uses a fresh context, not the run
// context: cancellation kills the child (via the watcher), and we still
// need the exit status afterward.
func (r *brokerRun) Wait() error {
	var res waitRunResult
	// Uncapped (budget 0): WaitRun blocks server-side until the run exits;
	// a fixed client deadline would spuriously time out a one-shot Run whose
	// runsc child hasn't finished exiting after emitting its terminal result.
	err := r.client.callWithCap(context.Background(), methodWaitRun, waitRunArgs{ContainerID: r.params.ContainerID}, &res, 0)
	r.oom = res.OOMKilled
	r.actuals = sandbox.RunActuals{PeakMemMB: res.PeakMemMB, CPUUsec: res.CPUUsec}
	r.stopWatcher()
	if r.conn != nil {
		_ = r.conn.Close()
	}
	if err != nil {
		return err
	}
	if res.ExitError != "" {
		return errors.New(res.ExitError)
	}
	return nil
}

// OOMKilled reports the broker's OOM attribution captured by Wait. Valid
// after Wait.
func (r *brokerRun) OOMKilled() bool { return r.oom }

// Actuals reports the run's measured cost as the broker read it off the
// cgroup at exit. Valid after Wait; both fields absent when the run never
// reached the broker's wait (an abandoned bring-up torn down via Close) or
// when the host's kernel offered nothing to read.
func (r *brokerRun) Actuals() sandbox.RunActuals { return r.actuals }

// Close tears the run down. If Start ran, it kills the broker's child (so
// the broker reaps it and removes its cgroup); otherwise it just releases
// the listener. Idempotent.
func (r *brokerRun) Close() error {
	r.closeOnce.Do(func() {
		r.stopWatcher()
		if r.started {
			_ = r.client.call(context.Background(), methodKillRun, killRunArgs{ContainerID: r.params.ContainerID}, nil)
		}
		if r.conn != nil {
			_ = r.conn.Close()
		}
		r.cleanupListener()
	})
	return nil
}

func (r *brokerRun) stopWatcher() {
	if r.watchCancel != nil {
		r.watchCancel()
		r.watchCancel = nil
	}
}

func (r *brokerRun) cleanupListener() {
	if r.listener != nil {
		_ = r.listener.Close()
		r.listener = nil
	}
	_ = os.Remove(r.socketPath)
}

// unixWriteCloser exposes only the write side of the shared stdio
// connection as the LaunchedRun's Stdin. Its Close half-closes the write
// direction (signaling EOF to the runtime's stdin) rather than tearing
// down the whole connection, so a caller that closes stdin doesn't also
// kill the stdout it's still reading. brokerRun.Wait owns the full close.
type unixWriteCloser struct{ c *net.UnixConn }

func (w unixWriteCloser) Write(p []byte) (int, error) { return w.c.Write(p) }
func (w unixWriteCloser) Close() error                { return w.c.CloseWrite() }
