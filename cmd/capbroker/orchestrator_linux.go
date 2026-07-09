//go:build linux

package capbroker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// readyTimeout bounds how long Start waits for the freshly-spawned broker
// to answer a Ping before giving up and surfacing a clear "broker never
// came up" error — rather than a caller's first real sandbox.Wrap
// mysteriously timing out or dialing a socket that doesn't exist yet. A
// var (not const) so tests can shrink it rather than waiting out the
// production budget.
var readyTimeout = 10 * time.Second

// readyPollInterval is how often Start retries the readiness Ping while
// waiting for the broker subprocess to create its socket and start
// serving.
var readyPollInterval = 50 * time.Millisecond

// brokerSocketPath is the socket path Start spawns the broker against.
// Production always uses DefaultSocketPath (the one fixed, per-executor
// path); a var — rather than reading DefaultSocketPath directly — so
// tests can redirect it to an isolated temp path instead of binding the
// real /run/tf/cap-broker.sock, which a non-root `go test` invocation
// (e.g. CI) can't create.
var brokerSocketPath = DefaultSocketPath

// Process owns the spawned cap-broker child process's lifecycle. The
// orchestrator holds exactly one per boot when TF_PRIVSEP=1 — "one broker
// process per executor (long-lived)... not one per run." cmd is nil when
// Start found (and dialed) a broker some other ancestor already spawned
// — the default container path — in which case Close is correctly a
// no-op: this process was never the broker's parent and, post-drop, no
// longer has the privilege to manage its lifecycle anyway. That process
// gets torn down via the container's own signal propagation instead (see
// docker/entrypoint.sh).
type Process struct {
	cmd        *exec.Cmd
	socketPath string
}

// execSelfCommand builds the child command that runs the broker. A
// package var so tests can point it at a helper process instead of
// re-exec'ing the real binary — mirrors
// internal/delegate/capture_isolated_linux.go's captureCommand. Production
// re-invokes this same binary's `cap-broker` subcommand via
// os.Executable().
var execSelfCommand = func(socketPath string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("capbroker: locate self: %w", err)
	}
	return exec.Command(self, "cap-broker", "--socket", socketPath), nil
}

// Start connects this process to a cap-broker and returns an IPCClient
// satisfying sandbox.PrivilegedOps, ready for sandbox.SetPrivilegedOps.
//
// In the default (TF_PRIVSEP on) container deployment,
// docker/entrypoint.sh already spawned the broker — still fully
// privileged — before it exec'd this (now capability-dropped) process
// into existence, so Start's first move is to check whether a broker is
// already listening at socketPath and, if so, just dial it: this process
// has no capabilities left to spawn a broker that could do anything
// privileged anyway, post-drop.
//
// If nothing answers, Start falls back to spawning the broker itself —
// the original behavior, kept for the dev/bare-metal path that runs the
// binary directly instead of through the provided container image. That
// fallback leaves both processes privileged (the spawning process's own
// capabilities are inherited unchanged by the child); only the container
// entrypoint's exec-time drop actually narrows anything — see
// docs/usage.md's privilege-separation section.
func Start(ctx context.Context) (*Process, sandbox.PrivilegedOps, error) {
	socketPath := brokerSocketPath

	if client := Dial(socketPath); client.Ping(ctx) == nil {
		return &Process{socketPath: socketPath}, client, nil
	}

	cmd, err := execSelfCommand(socketPath)
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("capbroker: start subprocess: %w", err)
	}

	client := Dial(socketPath)
	if err := waitReady(ctx, client); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, nil, err
	}

	return &Process{cmd: cmd, socketPath: socketPath}, client, nil
}

// waitReady polls Ping until it succeeds, readyTimeout elapses, or ctx is
// canceled — checked both before each Ping and during the poll sleep, so
// a caller that cancels ctx (e.g. Start's own caller giving up early)
// unwinds immediately rather than riding out the full readyTimeout.
func waitReady(ctx context.Context, client *IPCClient) error {
	deadline := time.Now().Add(readyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("capbroker: waiting for broker: %w", err)
		}
		pingCtx, cancel := context.WithTimeout(ctx, readyPollInterval)
		lastErr = client.Ping(pingCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("capbroker: waiting for broker: %w", ctx.Err())
		case <-time.After(readyPollInterval):
		}
	}
	return fmt.Errorf("capbroker: broker did not become ready within %s: %w", readyTimeout, lastErr)
}

// closeGrace bounds how long Close waits for a graceful SIGTERM exit
// before escalating to SIGKILL.
const closeGrace = 5 * time.Second

// Close terminates the broker subprocess. Idempotent-safe to call once;
// a nil Process (Start never succeeded) is a no-op. A broker that ignores
// SIGTERM is killed after closeGrace so shutdown never hangs the
// orchestrator.
func (h *Process) Close() error {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return nil
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)

	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(closeGrace):
		_ = h.cmd.Process.Kill()
		<-done
	}
	return nil
}
