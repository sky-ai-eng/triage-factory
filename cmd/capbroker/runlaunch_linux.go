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

// supervisedRuntime is the broker's handle on one started child: block for
// its exit (with OOM attribution) and SIGKILL it. Both occupants of the run
// registry satisfy it — the runsc child and a run's credential sidecar —
// which is why the shared wait/kill handlers need no per-kind logic.
type supervisedRuntime interface {
	Wait() (oomKilled bool, err error)
	Kill() error
}

// jailedRuntime is a supervisedRuntime that also reports what its run cost:
// the runsc child, which the broker starts inside a per-run cgroup. Actuals
// is valid only after Wait, for the same reason the OOM attribution is —
// both are read off the cgroup in the instant before it is removed.
//
// Separate from supervisedRuntime because the registry's other occupant, the
// credential sidecar, is an ordinary host process with no cgroup and nothing
// to measure. Typing the run launcher's seam as this (rather than asserting
// at the read) keeps a stand-in runtime that forgets the actuals a compile
// error instead of a silently unrecorded run.
type jailedRuntime interface {
	supervisedRuntime
	Actuals() sandbox.RunActuals

	// StderrTail is the bounded tail of what the jail wrote to stderr,
	// valid after Wait for the same reason Actuals is. It rides the wait
	// result so the orchestrator can name gVisor's own create/boot failure
	// instead of reporting a dead jail with an empty stderr.
	StderrTail() string
}

// launchRuntime is the seam the broker execs the runtime through. The
// default hands the socket fd to sandbox.LaunchSupervised, which wires it
// as runsc's stdin+stdout and closes the broker's copy — the bytes never
// enter this process. A var so tests can swap in a stand-in runtime. It
// takes the broker-built bundle dir (from prepareBundle), never a
// caller-supplied one.
var launchRuntime = func(ctx context.Context, bundleDir, containerID string, memoryLimitMB int, stdio *os.File, stderr io.Writer) (jailedRuntime, error) {
	return sandbox.LaunchSupervised(ctx, bundleDir, containerID, memoryLimitMB, stdio, stderr)
}

// prepareBundle is the seam the broker builds the OCI spec + bundle
// through. The default is sandbox.PrepareBundle (resolve rootfs by catalog
// name, build the spec from the fixed template, write the bundle). A var so
// tests can stub bundle construction — which needs a real baked rootfs and
// root — while still exercising the RPC, validation, semaphore, and
// socket-passthrough wiring.
var prepareBundle = sandbox.PrepareBundle

// runEntry is one in-flight supervised run. done is closed once the
// supervising goroutine has reaped the child; oom/actuals/waitErr are
// written before that close and read only after it, so no further lock is
// needed to observe them.
type runEntry struct {
	rt         supervisedRuntime
	done       chan struct{}
	oom        bool
	actuals    sandbox.RunActuals
	stderrTail string
	waitErr    error
}

// launchRun is the broker's spec-owning launch. It VALIDATES the params at
// the RPC boundary, acquires an in-flight slot (queueing under load rather
// than exhausting host resources), builds the OCI bundle from its own fixed
// template, dials the orchestrator's per-run stdio socket, hands its fd to
// the runtime as stdin+stdout, execs+supervises runsc, and registers the
// run so a later WaitRun/KillRun can reach it. It returns as soon as runsc
// is started — the RPC is short; the supervision runs in a goroutine and
// the exit status is fetched later via WaitRun.
//
// The socket fd is handed to the runtime and the broker's own copies are
// closed (the dialed conn here, plus the dup inside LaunchSupervised) so
// the run's bytes never enter the broker — there is no read on the socket
// anywhere on this path.
func (s *Server) launchRun(ctx context.Context, a launchRunArgs) (any, error) {
	p := a.Params

	// Boundary gate: this is the broker's real attack surface (the RPC from
	// the hostile-input-exposed orchestrator). Reject anything outside the
	// allowlist — a path/command-shaped param, an unknown rootfs name, a
	// non-allowlisted env key, a denylisted egress CIDR — BEFORE building or
	// execing anything, so a compromised orchestrator gets no path to
	// capabilities.
	if err := sandbox.ValidateLaunchParams(p); err != nil {
		return nil, err
	}

	// Abuse resistance (DoS, NOT a capability boundary — a validated caller
	// still cannot escalate). One orchestrator maps to one broker, so this
	// bounds a runaway caller's in-flight launches to the subnet pool size:
	// it QUEUES here rather than exhausting the /16→/24 allocator or the
	// privileged setup each launch costs. The slot is released when the run
	// is reaped, below. ctx carries the dispatch budget, so a caller that
	// jams the queue eventually times out instead of blocking forever.
	if err := s.acquireLaunchSlot(ctx); err != nil {
		return nil, err
	}
	slotReleased := false
	releaseSlot := func() {
		if !slotReleased {
			slotReleased = true
			s.releaseLaunchSlot()
		}
	}
	defer func() {
		// Release on every early-return path below; the success path clears
		// this by handing ownership to the supervising goroutine.
		if !slotReleased {
			releaseSlot()
		}
	}()

	// Build the bundle the broker owns (rootfs by catalog name → hash,
	// fixed-template spec). The bundle's lifetime is now the broker's: the
	// supervising goroutine removes it when the run is reaped, and every
	// error path below removes it before returning.
	bundleDir, err := prepareBundle(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("capbroker: prepare bundle: %w", err)
	}

	// DialContext (not DialTimeout) so the dial stays responsive to the call
	// budget / broker shutdown carried by ctx, while dialStdioTimeout still
	// bounds a stale/misconfigured socket path.
	dialer := net.Dialer{Timeout: dialStdioTimeout}
	conn, err := dialer.DialContext(ctx, "unix", p.StdioSocketPath)
	if err != nil {
		_ = sandbox.RemoveBundle(bundleDir)
		return nil, fmt.Errorf("capbroker: dial stdio socket %s: %w", p.StdioSocketPath, err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		_ = sandbox.RemoveBundle(bundleDir)
		return nil, fmt.Errorf("capbroker: stdio socket is not a unix conn")
	}
	// File() returns a dup of the connection's fd; the runtime inherits it
	// as stdin+stdout. LaunchSupervised closes this dup after Start; we
	// close the connection itself right after, leaving the runsc child the
	// sole holder of the socket fd.
	f, err := uc.File()
	if err != nil {
		_ = conn.Close()
		_ = sandbox.RemoveBundle(bundleDir)
		return nil, fmt.Errorf("capbroker: take stdio fd: %w", err)
	}

	// baseCtx (not a per-call ctx) ties the runtime's lifetime to the
	// broker: a broker Shutdown cancels it and SIGKILLs the child; explicit
	// cancellation still goes through KillRun.
	rt, err := launchRuntime(s.baseCtx, bundleDir, p.ContainerID, p.MemoryLimitMB, f, os.Stderr)
	// The runtime now holds its own inherited dup (on success); drop the
	// broker's remaining reference to the socket regardless of outcome.
	_ = conn.Close()
	if err != nil {
		_ = sandbox.RemoveBundle(bundleDir)
		return nil, fmt.Errorf("capbroker: launch runtime: %w", err)
	}

	// Key the registry by the unique container id, never the run id (which
	// callers may share via a fixed TraceID — keying off it would let one
	// run's wait/kill hit another's runtime).
	entry := &runEntry{rt: rt, done: make(chan struct{})}
	s.runsMu.Lock()
	s.runs[p.ContainerID] = entry
	s.runsMu.Unlock()

	// Ownership of the in-flight slot + bundle passes to the supervising
	// goroutine; the deferred release above must not also fire.
	slotReleased = true

	// Supervise in a goroutine so the child is reaped (and its cgroup +
	// bundle removed) even if the orchestrator never calls WaitRun. WaitRun
	// just fetches the recorded result. Deliberately NOT tracked by
	// s.inflight (the connection-drain WaitGroup): a run can outlive many
	// RPC connections, and Shutdown must not block on the whole run. On
	// Shutdown baseCtx is canceled, which SIGKILLs runsc (cmd.Cancel), so
	// this goroutine unwinds on its own.
	//
	// This goroutine is also where the run's actuals are captured, because
	// rt.Wait() is the single point every exit funnels through — a natural
	// exit, a KillRun (which only signals; this Wait still reaps and reads),
	// and a broker Shutdown alike. So a killed run's cost is measured before
	// its cgroup is removed just as a completed one's is.
	go func() {
		oom, werr := rt.Wait()
		entry.oom = oom
		entry.actuals = rt.Actuals()
		entry.stderrTail = rt.StderrTail()
		entry.waitErr = werr
		_ = sandbox.RemoveBundle(bundleDir)
		s.releaseLaunchSlot()
		close(entry.done)
	}()

	return emptyResult{}, nil
}

// waitRun blocks until the run's supervising goroutine has reaped the
// child, then returns its exit status + OOM attribution + measured actuals
// and drops the registry entry. In practice the orchestrator only calls
// this after it has seen its stdio socket EOF (the runtime exited), so done
// is already closed or about to be.
func (s *Server) waitRun(ctx context.Context, a waitRunArgs) (any, error) {
	s.runsMu.Lock()
	entry := s.runs[a.ContainerID]
	s.runsMu.Unlock()
	if entry == nil {
		return nil, fmt.Errorf("capbroker: waitrun: unknown run %q", a.ContainerID)
	}

	select {
	case <-entry.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	s.runsMu.Lock()
	delete(s.runs, a.ContainerID)
	s.runsMu.Unlock()

	res := waitRunResult{
		OOMKilled:  entry.oom,
		PeakMemMB:  entry.actuals.PeakMemMB,
		CPUUsec:    entry.actuals.CPUUsec,
		StderrTail: entry.stderrTail,
	}
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
	entry := s.runs[a.ContainerID]
	s.runsMu.Unlock()
	if entry == nil {
		return emptyResult{}, nil
	}
	_ = entry.rt.Kill()
	return emptyResult{}, nil
}
