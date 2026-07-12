//go:build linux

package capbroker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/capinfo"
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

// dialRaceWindow bounds how long Start retries its initial dial against a
// broker socket it expects some other, already-privileged process (the
// container entrypoint) to have created — not to wait out a slow boot
// (readyTimeout is for that), but only to absorb the narrow window
// between runBroker's listen() (the socket file becomes visible) and its
// subsequent --orchestrator-uid chown (cli_linux.go), during which a
// non-root dial can see a transient EACCES on the as-yet root-owned
// socket. Deliberately far shorter than readyTimeout: the common
// dev/bare-metal case — no broker exists at all yet, and Start must spawn
// its own — should fall through to spawning quickly rather than stalling
// for the full spawn-wait budget on a check that was never going to
// succeed. A var (not const) so tests can shrink it further.
var dialRaceWindow = 500 * time.Millisecond

// dialRacePollInterval is how often Start retries the initial dial within
// dialRaceWindow.
var dialRacePollInterval = 10 * time.Millisecond

// effectiveCaps reports this process's own effective Linux capability
// set — a package var wrapping capinfo.Effective (rather than calling it
// directly) so tests can stub a privileged posture. Start's spawn-fallback
// mechanism is meaningful independent of whether the *test* process
// happens to hold real capabilities (a `go test` binary run by CI's
// unprivileged runner user never does), so tests exercising that path
// stub this to simulate the privileged bare-metal deployment they stand
// in for, the same way they already stub execSelfCommand instead of
// re-exec'ing the real binary.
var effectiveCaps = capinfo.Effective

// brokerSocketPath is the socket path Start spawns the broker against.
// Production always uses DefaultSocketPath (the one fixed, per-executor
// path); a var — rather than reading DefaultSocketPath directly — so
// tests can redirect it to an isolated temp path instead of binding the
// real /run/tf/cap-broker.sock, which a non-root `go test` invocation
// (e.g. CI) can't create.
var brokerSocketPath = DefaultSocketPath

// Process owns the spawned cap-broker child process's lifecycle. The
// orchestrator holds exactly one per boot — one broker process per
// executor (long-lived), not one per run. cmd is nil when
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
// satisfying sandbox.SandboxOps, ready for sandbox.SetPrivilegedOps.
//
// In the container deployment, docker/entrypoint.sh already spawned the
// broker — still fully privileged — before it exec'd this (now
// capability-dropped) process into existence, so Start's first move is to
// check whether a broker is already listening at socketPath and, if so,
// just dial it: this process has no capabilities left to spawn a broker
// that could do anything privileged anyway, post-drop.
//
// That first check retries for dialRaceWindow — a short, dedicated
// budget distinct from the spawn-and-wait path's readyTimeout below — not
// a single dial attempt: the entrypoint's readiness poll only observes
// the socket file appearing (net.Listen, inside listen()), which happens
// a moment *before* runBroker's --orchestrator-uid chown — so a single
// unlucky Ping in that narrow window would otherwise see EACCES on an
// as-yet root-owned 0600 socket and misread a starting-up broker as
// absent. dialRaceWindow is sized only to absorb that syscall-scale race,
// not to wait out a slow boot — reusing the full readyTimeout here would
// mean the common dev/bare-metal case (no broker exists at all) stalls
// for that entire budget on a doomed check before ever attempting to
// spawn its own.
//
// If nothing answers within that budget, Start falls back to spawning
// the broker itself — the original behavior, kept for the dev/bare-metal
// path that runs the binary directly instead of through the provided
// container image, where both processes stay privileged (the spawning
// process's own capabilities are inherited unchanged by the child); only
// the container entrypoint's exec-time drop actually narrows anything —
// see docs/security/privilege-separation.md. Before taking that
// fallback, Start refuses to self-spawn if this process itself holds no
// capabilities: a self-spawned child can never hold more capabilities
// than its parent, so a capability-dropped orchestrator attempting this
// fallback could only ever produce an equally capability-less "broker" —
// one that would look started (spawning and serving a plain unix socket
// needs no privilege) but silently fail every real privileged operation
// later, deep inside a sandboxed run, with nothing at boot to explain
// why. Failing loudly here instead turns that into an immediate,
// diagnosable boot error.
func Start(ctx context.Context) (*Process, sandbox.SandboxOps, error) {
	socketPath := brokerSocketPath

	if client := Dial(socketPath); pollReady(ctx, client, dialRaceWindow, dialRacePollInterval) == nil {
		return &Process{socketPath: socketPath}, client, nil
	}

	if names, err := effectiveCaps(); err == nil && len(names) == 0 {
		return nil, nil, fmt.Errorf("capbroker: no broker answered at %s within %s, and this process holds no capabilities to spawn one with — expected the container entrypoint to have already started it", socketPath, dialRaceWindow)
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
// canceled — the budget for waiting on a broker Start itself just spawned
// (or, before that, will fall back to spawning) to finish booting.
func waitReady(ctx context.Context, client *IPCClient) error {
	return pollReady(ctx, client, readyTimeout, readyPollInterval)
}

// pollReady polls Ping until it succeeds, timeout elapses, or ctx is
// canceled — checked both before each Ping and during the poll sleep, so
// a caller that cancels ctx (e.g. Start's own caller giving up early)
// unwinds immediately rather than riding out the full timeout. Shared by
// waitReady (readyTimeout: waiting for a broker Start spawned to boot)
// and Start's initial existing-broker check (dialRaceWindow: a much
// shorter budget for a syscall-scale race, not a boot).
func pollReady(ctx context.Context, client *IPCClient, timeout, pollInterval time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("capbroker: waiting for broker: %w", err)
		}
		pingCtx, cancel := context.WithTimeout(ctx, pollInterval)
		lastErr = client.Ping(pingCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("capbroker: waiting for broker: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
	return fmt.Errorf("capbroker: broker did not become ready within %s: %w", timeout, lastErr)
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
