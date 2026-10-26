//go:build linux

package gvisor

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var gvisorLog = logging.Component("cell/gvisor")

// toolHostSocketName is the socket file the in-jail tool host dials inside
// the tool-host socket directory. One agent per jail today; a subagent
// fan-out gives each agent its own name in the same directory.
const toolHostSocketName = "tools.sock"

// The executor-host paths the tool host binary and the pinned gh binary are
// baked at (docker/Dockerfile): the sources of their read-only mounts, and
// the sources the broker validates them against. Vars so tests can point
// them at stubs; production never reassigns them.
var (
	hostToolHostBinaryPath = sandbox.TrustedToolHostBinaryPath()
	hostGHBinaryPath       = sandbox.TrustedGHBinaryPath()
)

// toolHostWaitGrace bounds how long Stop and Close wait for the runtime to be
// reaped after the kill, which is also the window for reading actuals. A
// killed runsc exits promptly; a jail that outlives this forfeits its actuals
// rather than holding teardown hostage.
const toolHostWaitGrace = 10 * time.Second

// recordActualsTimeout bounds the teardown stamp, one small write.
const recordActualsTimeout = 5 * time.Second

// toolHostJail is a launched runsc jail with the tool host as its main
// process: this backend's cell.Jail.
type toolHostJail struct {
	// socketPath is the host-side path of the tool host's unix socket. It
	// exists before the jail starts — this side binds it — and is what the
	// jail's argv points at through the bind mount.
	socketPath string

	listener net.Listener
	run      sandbox.LaunchedRun
	sb       *sandbox.Sandbox

	// One watcher owns run.Wait for the jail's whole life: Connect selects on
	// waitDone to fail fast when the jail dies before connecting, and Close
	// waits on it because Actuals is only valid after Wait returns — two
	// concurrent Wait calls on one LaunchedRun is not a contract anyone
	// offers. waitErr is written before waitDone closes.
	waitDone chan struct{}
	waitErr  error

	orgID         string
	claimID       string
	recordActuals func(ctx context.Context, orgID, claimID string, actuals cell.Actuals) error

	// closeOnce makes Close idempotent, as cell.Jail requires: a second
	// actuals stamp is a write nobody asked for.
	closeOnce sync.Once
}

var _ cell.Jail = (*toolHostJail)(nil)

// Connect implements cell.Jail. The in-jail tool host dials this side rather
// than listening, because the jail runs with --host-uds=open (see
// prepareToolHostSocket), so connecting to it means accepting its call.
//
// It fails early when the jail exits first. That case is otherwise
// indistinguishable from a slow start: a tool host that cannot exec, or
// refuses its arguments, dies immediately and would leave the caller blocked
// on a connection nobody was ever going to make.
func (j *toolHostJail) Connect(ctx context.Context, timeout time.Duration) (net.Conn, error) {
	type accepted struct {
		conn net.Conn
		err  error
	}
	conns := make(chan accepted, 1)
	go func() {
		c, err := j.listener.Accept()
		conns <- accepted{c, err}
	}()

	select {
	case a := <-conns:
		if a.err != nil {
			return nil, fmt.Errorf("gvisor: accept tool host connection: %w", a.err)
		}
		return a.conn, nil
	case <-j.waitDone:
		return nil, fmt.Errorf("gvisor: tool host exited before connecting (%v); stderr: %s", j.waitErr, j.run.Stderr())
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, fmt.Errorf("gvisor: tool host did not connect within %s", timeout)
	}
}

// Stop implements cell.Jail: it kills the jail and returns once its runtime
// has been reaped.
func (j *toolHostJail) Stop() error {
	if j == nil || j.run == nil || j.waitDone == nil {
		return nil
	}
	_ = j.run.Close()
	select {
	case <-j.waitDone:
		return nil
	case <-time.After(toolHostWaitGrace):
		return fmt.Errorf("gvisor: tool host jail was still running %s after it was killed", toolHostWaitGrace)
	}
}

// Exited implements cell.Jail: whether the jail's runtime has been reaped.
func (j *toolHostJail) Exited() bool {
	if j == nil || j.waitDone == nil {
		return true
	}
	select {
	case <-j.waitDone:
		return true
	default:
		return false
	}
}

// Freeze implements cell.Jail through runsc pause.
func (j *toolHostJail) Freeze(ctx context.Context) error {
	if j == nil || j.Exited() {
		return nil
	}
	f, ok := j.run.(sandbox.Freezer)
	if !ok {
		return fmt.Errorf("gvisor: this tool host's launcher cannot freeze it")
	}
	return f.Pause(ctx)
}

// Thaw implements cell.Jail through runsc resume.
func (j *toolHostJail) Thaw(ctx context.Context) error {
	if j == nil || j.Exited() {
		return nil
	}
	f, ok := j.run.(sandbox.Freezer)
	if !ok {
		return fmt.Errorf("gvisor: this tool host's launcher cannot freeze it")
	}
	return f.Resume(ctx)
}

// OOMKilled implements cell.Jail: whether the jail's memory-ceiling cgroup
// recorded an OOM kill. False while the tool host is still running or was
// never launched.
func (j *toolHostJail) OOMKilled() bool {
	if j == nil || j.run == nil || j.waitDone == nil {
		return false
	}
	select {
	case <-j.waitDone:
		return j.run.OOMKilled()
	default:
		return false
	}
}

// Sample implements cell.Jail by reading the jail's cgroup, which the
// unprivileged executor may do directly: cgroup v2 stat files are
// world-readable, and the broker's monopoly is on cgroup lifecycle, never
// observation.
func (j *toolHostJail) Sample() cell.Sample {
	if j == nil || j.sb == nil || j.Exited() {
		return cell.Sample{}
	}
	return sandbox.SampleRunCgroup(j.sb.ContainerID)
}

// OOMKills implements cell.Jail from the jail cgroup's memory.events, readable
// by the executor for the same reason Sample's files are. The kernel's victim
// is a host process, usually the systrap stub running one guest process, so
// each kill the count records is one guest process ended.
func (j *toolHostJail) OOMKills() (int, bool) {
	if j == nil || j.sb == nil || j.Exited() {
		return 0, false
	}
	return sandbox.RunCgroupOOMKills(j.sb.ContainerID)
}

// Close implements cell.Jail: it kills the jail, stamps the engagement's
// measured actuals, reclaims the cgroup, closes the listener and removes the
// socket. The socket's directory and the network belong to the cell and go
// with it, after this.
func (j *toolHostJail) Close() error {
	if j == nil {
		return nil
	}
	j.closeOnce.Do(func() {
		if j.listener != nil {
			_ = j.listener.Close()
		}
		if j.run != nil {
			_ = j.run.Close()
			j.recordActualsOnce()
		}
		if j.sb != nil {
			_ = j.sb.Close()
		}
		if j.socketPath != "" {
			_ = os.Remove(j.socketPath)
		}
	})
	return nil
}

// recordActualsOnce waits briefly for the watcher's Wait to return — the
// point after which Actuals is valid — then stamps the claim. Best-effort at
// every step: a jail that measured nothing, a launch with no claim, or a
// recorder that fails costs a warn line, never the teardown.
func (j *toolHostJail) recordActualsOnce() {
	if j.recordActuals == nil || j.claimID == "" || j.waitDone == nil {
		return
	}
	select {
	case <-j.waitDone:
	case <-time.After(toolHostWaitGrace):
		gvisorLog.Warn("tool host did not exit within the teardown grace; skipping actuals stamp", "claim", j.claimID)
		return
	}
	actuals := j.run.Actuals()
	if actuals.PeakMemMB == nil && actuals.CPUUsec == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordActualsTimeout)
	defer cancel()
	if err := j.recordActuals(ctx, j.orgID, j.claimID, actuals); err != nil {
		gvisorLog.Warn("record sandbox actuals for claim failed", "claim", j.claimID, "error", err)
	}
}

// launchToolHost starts the tool host as the main process of a runsc jail on
// rn, for conversationID. The rootfs, the run-tree hand-off and the TF-side
// mounts are the same shape every gVisor launch uses; what is specific to
// this one is the pinned entrypoint (the tool host's `serve` verb) and the
// per-run directory mounted read-write for its socket.
func launchToolHost(ctx context.Context, rn *sandbox.RunNetwork, conversationID string, spec cell.LaunchSpec) (_ *toolHostJail, err error) {
	ctx, span := tracer.Start(ctx, "agent.jail.launch", trace.WithAttributes(telemetry.Runtime("native")))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	if conversationID == "" {
		return nil, fmt.Errorf("gvisor: tool host launch requires a conversation id")
	}
	if spec.RunTree == "" {
		return nil, fmt.Errorf("gvisor: tool host launch requires a run tree")
	}
	if rn == nil {
		return nil, fmt.Errorf("gvisor: tool host launch requires a run network")
	}
	if spec.AgentHostSocket == "" {
		return nil, fmt.Errorf("gvisor: tool host launch requires the agenthost socket — without it every exec verb inside the jail is stranded")
	}
	if spec.CABundle == "" {
		return nil, fmt.Errorf("gvisor: tool host launch requires the CA bundle — without it no registry the egress proxy admits verifies in the jail")
	}

	sockDir, listener, err := prepareToolHostSocket(conversationID)
	if err != nil {
		return nil, err
	}
	jail := &toolHostJail{
		socketPath:    filepath.Join(sockDir, toolHostSocketName),
		listener:      listener,
		orgID:         spec.OrgID,
		claimID:       spec.ClaimID,
		recordActuals: spec.RecordActuals,
	}

	// The run tree goes to this engagement's jail identity at every launch,
	// not only the first: a task's tree outlives an engagement, and the
	// engagement before this one may have held another index. Made through
	// the network, which takes it back before the index can go to anyone else.
	if err := rn.HandOffRunTree(ctx, spec.RunTree); err != nil {
		_ = jail.Close()
		return nil, fmt.Errorf("gvisor: hand the run tree to the tool host's jail: %w", err)
	}

	mounts, err := toolHostMounts(sockDir, spec)
	if err != nil {
		_ = jail.Close()
		return nil, err
	}
	// The staging directories this launch mounts are the orchestrator's and
	// closed to everyone else; the jail reads them through its gid, granted
	// for this engagement and taken back with the network.
	for _, m := range mounts {
		if m.Destination != sandbox.TrustedSkillsDestination && m.Destination != sandbox.TrustedMemoryDestination {
			continue
		}
		if err := rn.GrantStaging(ctx, m.Source); err != nil {
			_ = jail.Close()
			return nil, fmt.Errorf("gvisor: grant %s to the tool host's jail: %w", m.Source, err)
		}
	}

	// --connect, not --bind: the jail runs with gVisor's --host-uds=open,
	// which permits connecting to a host socket but not creating one, so the
	// listener has to be on this side. The socket path and cwd are passed
	// explicitly rather than defaulted, so the server resolves tool paths
	// against the run tree regardless of the process cwd the runtime gives it.
	argv := []string{
		sandbox.TrustedToolHostBinaryDestination, "serve",
		"--connect", filepath.Join(sandbox.TrustedToolHostSocketDirDestination, toolHostSocketName),
		"--cwd", cell.WorkDir,
	}

	run, sb, err := sandbox.Wrap(ctx, sandbox.Config{
		ConversationID: conversationID,
		WorkspaceKey:   spec.WorkspaceKey,
		Worktree:       spec.RunTree,
		Argv:           argv,
		Env:            cell.JailEnv(spec),
		ExtraMounts:    mounts,
		Network:        rn,
		MemoryLimitMB:  spec.MemoryLimitMB,
	})
	if err != nil {
		_ = jail.Close()
		return nil, fmt.Errorf("gvisor: launch tool host sandbox: %w", err)
	}
	jail.run, jail.sb = run, sb

	if err := run.Start(); err != nil {
		_ = jail.Close()
		return nil, fmt.Errorf("gvisor: start tool host: %w", err)
	}

	// The single watcher that owns run.Wait (see the toolHostJail fields),
	// started only once the runtime is live.
	jail.waitDone = make(chan struct{})
	go func() {
		jail.waitErr = run.Wait()
		close(jail.waitDone)
	}()
	return jail, nil
}

// prepareToolHostSocket binds the tool host's socket inside the run's
// tool-host socket directory and opens it to the run's jail.
//
// This side binds because the jail cannot. gVisor gates host unix sockets with
// a sandbox-wide --host-uds flag: "open" permits connect, and creating one
// needs "create". The jail gets "open" deliberately — "create" would let the
// process holding hostile input bind host-visible sockets anywhere writable in
// its mount tree — so a listener inside it is not available, and the in-jail
// host dials out to this socket instead. The direction is exactly the
// agenthost socket's, for exactly the same reason.
//
// The directory is not this process's to make: the broker created it for the
// engagement (sandbox.RunNetwork.PrepareSockets), owned by this process with
// the run's jail gid as its setgid group. The socket takes that group at birth
// — this process carries no jail gid and could not chgrp to one — and connect(2)
// needs write permission on the socket, so it is chmodded 0660. A directory
// whose group is not a jail gid was not prepared for an engagement, and the
// launch fails here rather than at the jail's first connect.
func prepareToolHostSocket(conversationID string) (string, net.Listener, error) {
	dir := sandbox.TrustedToolHostSocketDir(conversationID)
	info, err := os.Stat(dir)
	if err != nil {
		return "", nil, fmt.Errorf("gvisor: the run's tool host socket directory %s: %w", dir, err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || !info.IsDir() || !sandbox.IsJailUID(int(st.Gid)) {
		return "", nil, fmt.Errorf("gvisor: tool host socket directory %s was not prepared for a jail", dir)
	}

	sockPath := filepath.Join(dir, toolHostSocketName)
	// A socket left by an earlier launch in this directory would make bind
	// fail with EADDRINUSE.
	_ = os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return "", nil, fmt.Errorf("gvisor: bind tool host socket %s: %w", sockPath, err)
	}
	// net.Listen creates the socket 0777&~umask (0755 under the usual 022) and
	// connect(2) requires write permission, so without this the in-jail peer
	// the socket exists for gets EACCES.
	if err := os.Chmod(sockPath, 0o660); err != nil {
		_ = listener.Close()
		_ = os.Remove(sockPath)
		return "", nil, fmt.Errorf("gvisor: chmod tool host socket %s: %w", sockPath, err)
	}
	return dir, listener, nil
}

// toolHostMounts assembles the jail's bind mounts: the tool host binary and
// the socket directory it needs, the TF binary under both names, the
// exec-verb socket, the run's CA bundle and allowed-signers file, the
// git-hooks directory, the gh
// channel's pinned binary, and this launch's staged skill and memory.
func toolHostMounts(sockDir string, spec cell.LaunchSpec) ([]sandbox.Mount, error) {
	if _, err := os.Stat(hostToolHostBinaryPath); err != nil {
		return nil, fmt.Errorf("gvisor: tool host binary is not present at %s: %w", hostToolHostBinaryPath, err)
	}
	mounts := []sandbox.Mount{
		{Source: hostToolHostBinaryPath, Destination: sandbox.TrustedToolHostBinaryDestination, Options: []string{"ro"}},
		// Read-write by necessity: the server binds its socket in here.
		{Source: sockDir, Destination: sandbox.TrustedToolHostSocketDirDestination},
		// Mounted unconditionally: the sidecar created the socket during
		// bring-up, before any dispatch reached this launch, and the broker
		// validates the source against its own per-run derivation. A missing
		// file host-side fails the launch loudly there rather than launching
		// a jail whose exec verbs would consult a fresh local database.
		{Source: spec.AgentHostSocket, Destination: sandbox.TrustedAgentHostSocketDestination},
		// Mounted unconditionally for the same reason: the sidecar wrote it
		// during bring-up, and the broker validates the source.
		{Source: spec.CABundle, Destination: sandbox.TrustedCABundleDestination, Options: []string{"ro"}},
	}
	// Read-only, like the CA bundle and for the same reason: it is a file the
	// sidecar wrote for the run to read, and the broker validates its source.
	if spec.AllowedSigners != "" {
		mounts = append(mounts, sandbox.Mount{Source: spec.AllowedSigners, Destination: sandbox.TrustedAllowedSignersDestination, Options: []string{"ro"}})
	}
	if selfBin, err := os.Executable(); err == nil && selfBin != "" {
		mounts = append(mounts,
			sandbox.Mount{Source: selfBin, Destination: sandbox.TrustedTFBinaryDestination, Options: []string{"ro"}},
			sandbox.Mount{Source: selfBin, Destination: sandbox.TrustedTFACBinaryDestination, Options: []string{"ro"}},
		)
	}
	hooksDir := githooks.HostDir()
	if _, err := os.Stat(hooksDir); err == nil {
		mounts = append(mounts, sandbox.Mount{Source: hooksDir, Destination: githooks.SandboxDir, Options: []string{"ro"}})
	}
	if spec.GHChannel != nil {
		if _, err := os.Stat(hostGHBinaryPath); err == nil {
			mounts = append(mounts, sandbox.Mount{Source: hostGHBinaryPath, Destination: sandbox.TrustedGHBinaryDestination, Options: []string{"ro"}})
		}
	}
	// A staging directory that vanished (a swept orphan on a cold
	// cross-executor resume) must not fail the launch on the broker's
	// source-resolution check. The agent then continues from its transcript
	// without the skill, or without its predecessor's notes: a degraded start,
	// not a broken run.
	if spec.SkillsSource != "" {
		if _, err := os.Stat(spec.SkillsSource); err == nil {
			mounts = append(mounts, sandbox.Mount{Source: spec.SkillsSource, Destination: sandbox.TrustedSkillsDestination, Options: []string{"ro"}})
		} else {
			gvisorLog.Warn("step-skill staging dir missing; launching the tool host without the skills mount", "path", spec.SkillsSource, "error", err)
		}
	}
	if spec.MemorySource != "" {
		if _, err := os.Stat(spec.MemorySource); err == nil {
			mounts = append(mounts, sandbox.Mount{Source: spec.MemorySource, Destination: sandbox.TrustedMemoryDestination, Options: []string{"ro"}})
		} else {
			gvisorLog.Warn("entity-memory staging dir missing; launching the tool host without the memory mount", "path", spec.MemorySource, "error", err)
		}
	}
	return mounts, nil
}
