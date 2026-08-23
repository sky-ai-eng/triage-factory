package agentproc

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

// Fixed in-sandbox paths for the native runtime's resident tool host. Both
// are duplicated as literals in internal/sandbox (the broker validates
// against its own copy and cannot import this package without a cycle);
// toolhost_drift_test.go cross-checks the pairs.
const (
	sandboxToolHostBinary    = "/opt/tf/bin/tf-harness-tools"
	sandboxToolHostSocketDir = "/run/tf-tools"
	// toolHostSocketName is the socket file the in-jail server binds inside
	// the mounted directory. One agent per jail today; a subagent fan-out
	// gives each agent its own name in this same directory.
	toolHostSocketName = "tools.sock"
)

// hostToolHostBinaryPath is the executor-host path the compiled tool host is
// baked at (docker/Dockerfile), the source of the read-only bind mount. A
// var so tests can point it at a stub; production never reassigns it.
var hostToolHostBinaryPath = sandboxToolHostBinary

// ToolHostOptions is one native engagement's jail launch. It is deliberately
// much narrower than RunOptions: the jail runs a tool server, not an agent,
// so there is no model, no prompt, no allowlist, and no stream to parse.
type ToolHostOptions struct {
	// ConversationID is the conversation id — the sandbox container id, the run-tree
	// key, and what the socket directory is named after.
	ConversationID string
	// MemoryNamespace is the blueprint run id, the run tree's second
	// legitimate key (a cold rehydrate rebuilds under it).
	MemoryNamespace string
	// Worktree is the host path bind-mounted at /work and the working
	// directory every tool call resolves against.
	Worktree string
	// ExtraEnv is host-path-bearing env translated for the sandbox, exactly
	// as the SDK path translates it.
	ExtraEnv []string
	// PrebuiltNetwork and PrebuiltProxyEnv come from the per-run credential
	// sidecar the caller brought up. Required: this process holds no
	// credential, so a native jail without a prebuilt network is a wiring
	// bug, not a case to degrade through.
	PrebuiltNetwork  *sandbox.RunNetwork
	PrebuiltProxyEnv []string
	// AgentHostSocket bind-mounts the per-run exec-verb socket (the sidecar
	// hosts the server) at its fixed in-jail path, exactly as the SDK path's
	// startAgentHost supplies it. Required, same reasoning as PrebuiltNetwork:
	// without it every `tfac` verb inside the jail falls back to a local
	// client reading a fresh in-jail database and reports the run as
	// nonexistent — a wiring bug wearing a data-corruption costume.
	AgentHostSocket sandbox.Mount
	// GHChannel wires the real-gh credential injector, when the sidecar
	// bound one.
	GHChannel *GHChannelParams
	// SkillsSourcePath is the orchestrator-owned staging dir holding this
	// step's SKILL.md, bind-mounted read-only. Same contract as
	// RunOptions.SkillsSourcePath and set from the same runConfig field:
	// only a blueprint step carries one, and it must reach a native jail
	// exactly as it reaches an SDK one or a sandboxed step silently loses
	// the skill it staged.
	SkillsSourcePath string
	// MemorySourcePath is the orchestrator-owned staging dir holding the
	// entity-memory tree materialized for THIS launch, bind-mounted read-only.
	// Same contract as RunOptions.MemorySourcePath and set from the same
	// runConfig field: a jailed agent reads its blueprint handoff through this
	// mount, so a native step that does not get it starts from nothing where
	// an SDK step of the same blueprint would have had its predecessor's notes.
	MemorySourcePath string
	// MemoryLimitMB caps the jail's cgroup. Zero uses the process default.
	MemoryLimitMB int
	// OrgID and ClaimID name the engagement this jail belongs to — the same
	// pair RunOptions carries. ClaimID keys the live-jail registry entry the
	// resource sampler iterates and the claim row the teardown actuals stamp
	// lands on; empty leaves both unattributed (registration no-ops), which
	// is the test-harness shape, never a production launch.
	OrgID   string
	ClaimID string
	// RecordSandboxActuals mirrors RunOptions.RecordSandboxActuals: persists
	// what the jail actually cost, measured at its cgroup, once the runtime
	// has exited. Called from Close, best-effort.
	RecordSandboxActuals func(ctx context.Context, orgID, claimID string, actuals sandbox.RunActuals) error
}

// ToolHostJail is a launched tool-host sandbox. The caller Accepts the
// in-jail host's connection, drives the engagement, then closes: the server
// observes EOF at a frame boundary and exits, and Close reclaims the jail
// either way.
type ToolHostJail struct {
	// SocketPath is the host-side path of the tool host's unix socket. It
	// exists before the jail starts — this side binds it — and is what the
	// jail's argv points at through the bind mount.
	SocketPath string

	listener net.Listener
	run      sandbox.LaunchedRun
	sb       *sandbox.Sandbox
	sockDir  string

	// One watcher owns run.Wait for the jail's whole life: Accept selects on
	// waitDone to fail fast when the jail dies before connecting, and Close
	// waits on it because Actuals is only valid after Wait returns — two
	// concurrent Wait calls on one LaunchedRun is not a contract anyone
	// offers. waitErr is written before waitDone closes.
	waitDone chan struct{}
	waitErr  error

	orgID           string
	claimID         string
	recordActuals   func(ctx context.Context, orgID, claimID string, actuals sandbox.RunActuals) error
	releaseLiveJail func()
}

// Accept waits for the in-jail tool host to dial in and returns the resulting
// connection.
//
// It fails early rather than waiting out the deadline when the jail exits
// first. That case is otherwise indistinguishable from a slow start and was
// the difference between a one-line diagnosis and a silent timeout: a tool
// host that cannot exec, or refuses its arguments, dies immediately and would
// leave the caller blocked on a connection nobody was ever going to make.
func (j *ToolHostJail) Accept(ctx context.Context, timeout time.Duration) (net.Conn, error) {
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
			return nil, fmt.Errorf("agentproc: accept tool host connection: %w", a.err)
		}
		return a.conn, nil
	case <-j.waitDone:
		return nil, fmt.Errorf("agentproc: tool host exited before connecting (%v); stderr: %s", j.waitErr, j.run.Stderr())
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, fmt.Errorf("agentproc: tool host did not connect within %s", timeout)
	}
}

// toolHostWaitGrace bounds how long Close waits for the watcher's Wait to
// return after the kill, which is also the window for reading actuals. A
// killed runsc exits promptly; a jail that outlives this forfeits its
// actuals rather than holding teardown hostage.
const toolHostWaitGrace = 10 * time.Second

// Close kills the jail, stamps the engagement's measured actuals, reclaims
// the registry entry and cgroup, closes the listener and removes the socket
// directory. The prebuilt network and sidecar are the caller's to close,
// ordered after this.
func (j *ToolHostJail) Close() error {
	if j == nil {
		return nil
	}
	if j.listener != nil {
		_ = j.listener.Close()
	}
	if j.run != nil {
		_ = j.run.Close()
		j.recordActualsOnce()
	}
	if j.releaseLiveJail != nil {
		j.releaseLiveJail()
	}
	if j.sb != nil {
		_ = j.sb.Close()
	}
	if j.sockDir != "" {
		_ = os.RemoveAll(j.sockDir)
	}
	return nil
}

// OOMKilled reports whether the jail's memory-ceiling cgroup recorded an OOM
// kill. Meaningful only once the tool host has exited (the same contract as
// LaunchedRun.OOMKilled); false while it is still running or was never
// launched.
func (j *ToolHostJail) OOMKilled() bool {
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

// recordActualsOnce waits (briefly) for the watcher's Wait to return — the
// point after which Actuals is valid — then stamps the claim. Best-effort at
// every step, mirroring recordSandboxActuals on the SDK path: a jail that
// measured nothing, a launch with no claim, or a recorder that fails costs a
// warn line, never the teardown.
func (j *ToolHostJail) recordActualsOnce() {
	if j.recordActuals == nil || j.claimID == "" || j.waitDone == nil {
		return
	}
	select {
	case <-j.waitDone:
	case <-time.After(toolHostWaitGrace):
		agentprocLog.Warn("tool host did not exit within the teardown grace; skipping actuals stamp", "claim", j.claimID)
		return
	}
	actuals := j.run.Actuals()
	if actuals.PeakMemMB == nil && actuals.CPUUsec == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordActualsTimeout)
	defer cancel()
	if err := j.recordActuals(ctx, j.orgID, j.claimID, actuals); err != nil {
		agentprocLog.Warn("record sandbox actuals for claim failed", "claim", j.claimID, "error", err)
	}
}

// LaunchToolHost starts the resident tool host as the jail's main process
// for a native engagement.
//
// The rootfs, worktree mount and chown, prebuilt network, and TF-binary/
// git-hooks/gh-channel mounts are the same shape every sandboxed launch
// uses; what's specific to this one is the pinned entrypoint (the tool
// host's binary + `serve` verb, not an agent process) and a per-run
// directory mounted read-write for its socket. Everything TF-side still
// arrives via bind mounts and env, never baked into or assumed of the
// rootfs.
func LaunchToolHost(ctx context.Context, opts ToolHostOptions) (_ *ToolHostJail, err error) {
	// The native runtime's jail launch. The loop this hands off to is out of
	// the tracing epic's scope, but everything up to and including the jail
	// coming up is shared setup and belongs in the engagement's trace either
	// way.
	var launchSpan trace.Span
	ctx, launchSpan = tracer.Start(ctx, "agent.jail.launch", trace.WithAttributes(telemetry.Runtime("native")))
	defer func() {
		recordSpanError(launchSpan, err)
		launchSpan.End()
	}()

	if opts.ConversationID == "" {
		return nil, fmt.Errorf("agentproc: tool host launch requires a conversation id")
	}
	if opts.Worktree == "" {
		return nil, fmt.Errorf("agentproc: tool host launch requires a worktree")
	}
	if opts.PrebuiltNetwork == nil {
		return nil, fmt.Errorf("agentproc: tool host launch requires a prebuilt run network — multi mode is always per-run-isolated")
	}
	if opts.AgentHostSocket.Source == "" || opts.AgentHostSocket.Destination == "" {
		return nil, fmt.Errorf("agentproc: tool host launch requires the agenthost socket mount — without it every exec verb inside the jail is stranded")
	}

	sockDir, listener, err := prepareToolHostSocket(opts.ConversationID)
	if err != nil {
		return nil, err
	}
	jail := &ToolHostJail{
		SocketPath:    filepath.Join(sockDir, toolHostSocketName),
		listener:      listener,
		sockDir:       sockDir,
		orgID:         opts.OrgID,
		claimID:       opts.ClaimID,
		recordActuals: opts.RecordSandboxActuals,
	}

	if err := chownWorktreeForSandbox(ctx, opts.Worktree); err != nil {
		_ = jail.Close()
		return nil, fmt.Errorf("agentproc: chown worktree for tool host: %w", err)
	}

	mounts, err := toolHostMounts(sockDir, opts.AgentHostSocket, opts.GHChannel, opts.SkillsSourcePath, opts.MemorySourcePath)
	if err != nil {
		_ = jail.Close()
		return nil, err
	}

	env := buildSandboxEnv(translateEnvForSandbox(opts.ExtraEnv, opts.Worktree))
	env = append(env, githooks.BinEnvVar+"="+sandboxTFBinary)
	env = append(env, opts.PrebuiltProxyEnv...)
	env = append(env, ghChannelEnv(opts.GHChannel)...)

	// --connect, not --bind: the jail runs with gVisor's --host-uds=open,
	// which permits connecting to a host socket but not creating one, so the
	// listener has to be on this side. The socket path and cwd are passed
	// explicitly rather than defaulted, so the server resolves tool paths
	// against /work regardless of the process cwd the runtime gives it.
	argv := []string{
		sandboxToolHostBinary, "serve",
		"--connect", filepath.Join(sandboxToolHostSocketDir, toolHostSocketName),
		"--cwd", "/work",
	}

	run, sb, err := sandbox.Wrap(ctx, sandbox.Config{
		ConversationID:  opts.ConversationID,
		MemoryNamespace: opts.MemoryNamespace,
		Worktree:        opts.Worktree,
		Argv:            argv,
		Env:             env,
		ExtraMounts:     mounts,
		Network:         opts.PrebuiltNetwork,
		MemoryLimitMB:   memoryLimitOrDefault(opts.MemoryLimitMB),
	})
	if err != nil {
		_ = jail.Close()
		return nil, fmt.Errorf("agentproc: launch tool host sandbox: %w", err)
	}
	jail.run, jail.sb = run, sb

	if err := run.Start(); err != nil {
		_ = jail.Close()
		return nil, fmt.Errorf("agentproc: start tool host: %w", err)
	}

	// The single watcher that owns run.Wait (see the ToolHostJail fields),
	// started only once the runtime is live, and the resource sampler's
	// registry entry — both scoped to the running jail exactly.
	jail.waitDone = make(chan struct{})
	go func() {
		jail.waitErr = run.Wait()
		close(jail.waitDone)
	}()
	jail.releaseLiveJail = registerLiveJail(opts.ClaimID, sb.ContainerID)
	return jail, nil
}

func memoryLimitOrDefault(mb int) int {
	if mb > 0 {
		return mb
	}
	return ClaimMemoryLimitMB()
}

// prepareToolHostSocket creates the per-run directory, binds the tool host's
// socket inside it, and grants both to the sandbox identity.
//
// This side binds because the jail cannot. gVisor gates host unix sockets with
// a sandbox-wide --host-uds flag: "open" permits connect, and creating one
// needs "create". The jail gets "open" deliberately — "create" would let the
// process holding hostile input bind host-visible sockets anywhere writable in
// its mount tree — so a listener inside it is not available, and the in-jail
// host dials out to this socket instead. The direction is exactly the
// agenthost socket's, for exactly the same reason.
//
// The grant mirrors agenthost's too: the jail runs as another uid, and
// connect(2) needs write permission on the socket, so it is chowned to the
// sandbox identity (or group-granted when this process is not root) and
// chmodded 0660. The directory is setgid so anything created inside it later
// carries the same group.
func prepareToolHostSocket(conversationID string) (string, net.Listener, error) {
	dir := sandbox.TrustedToolHostSocketDir(conversationID)
	// A stale directory from a crashed predecessor would carry a dead socket
	// that would make bind fail with EADDRINUSE, and its mode may not have
	// survived either; recreate from scratch.
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return "", nil, fmt.Errorf("agentproc: create tool host socket dir %s: %w", dir, err)
	}
	fail := func(err error) (string, net.Listener, error) {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	if os.Getuid() == 0 {
		if err := os.Chown(dir, sandbox.WorktreeUID, sandbox.WorktreeGID); err != nil {
			return fail(fmt.Errorf("agentproc: chown tool host socket dir %s: %w", dir, err))
		}
	} else if err := os.Chown(dir, -1, sandbox.WorktreeGID); err != nil {
		return fail(fmt.Errorf("agentproc: chgrp tool host socket dir %s to gid=%d: %w "+
			"(an unprivileged orchestrator must be a member of the sandbox group — the provided image's tf-sandbox — for this owner-legal group grant)",
			dir, sandbox.WorktreeGID, err))
	}
	if err := os.Chmod(dir, 0o2770); err != nil {
		return fail(fmt.Errorf("agentproc: chmod tool host socket dir %s: %w", dir, err))
	}

	sockPath := filepath.Join(dir, toolHostSocketName)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fail(fmt.Errorf("agentproc: bind tool host socket %s: %w", sockPath, err))
	}
	closeAndFail := func(err error) (string, net.Listener, error) {
		_ = listener.Close()
		return fail(err)
	}
	if os.Getuid() == 0 {
		if err := os.Chown(sockPath, sandbox.WorktreeUID, sandbox.WorktreeGID); err != nil {
			return closeAndFail(fmt.Errorf("agentproc: chown tool host socket %s: %w", sockPath, err))
		}
	} else if err := os.Chown(sockPath, -1, sandbox.WorktreeGID); err != nil {
		return closeAndFail(fmt.Errorf("agentproc: chgrp tool host socket %s to gid=%d: %w", sockPath, sandbox.WorktreeGID, err))
	}
	// net.Listen creates the socket 0777&~umask (0755 under the usual 022) and
	// connect(2) requires write permission, so without this the in-jail peer
	// the socket exists for gets EACCES.
	if err := os.Chmod(sockPath, 0o660); err != nil {
		return closeAndFail(fmt.Errorf("agentproc: chmod tool host socket %s: %w", sockPath, err))
	}
	return dir, listener, nil
}

// toolHostMounts assembles the jail's bind mounts: the tool host binary and
// the socket directory it needs, plus the same TF-side mounts an SDK jail
// gets (the TF binary under both names, the agenthost exec-verb socket, the
// git-hooks dir, the gh channel's cert + pinned binary, and this step's
// staged skill).
func toolHostMounts(sockDir string, agentHost sandbox.Mount, gh *GHChannelParams, skillsSource, memorySource string) ([]sandbox.Mount, error) {
	if _, err := os.Stat(hostToolHostBinaryPath); err != nil {
		return nil, fmt.Errorf("agentproc: tool host binary is not present at %s: %w", hostToolHostBinaryPath, err)
	}
	mounts := []sandbox.Mount{
		{Source: hostToolHostBinaryPath, Destination: sandboxToolHostBinary, Options: []string{"ro"}},
		// Read-write by necessity: the server binds its socket in here.
		{Source: sockDir, Destination: sandboxToolHostSocketDir},
		// Mounted unconditionally: the sidecar created the socket during
		// bring-up, before any dispatch reached this launch, and the broker
		// validates the source against its own per-run derivation. A missing
		// file host-side fails the launch loudly there rather than launching
		// a jail whose exec verbs would consult a fresh local database.
		agentHost,
	}
	if selfBin, err := os.Executable(); err == nil && selfBin != "" {
		mounts = append(mounts,
			sandbox.Mount{Source: selfBin, Destination: sandboxTFBinary, Options: []string{"ro"}},
			sandbox.Mount{Source: selfBin, Destination: sandboxTFACBinary, Options: []string{"ro"}},
		)
	}
	hooksDir := githooks.HostDir()
	if _, err := os.Stat(hooksDir); err == nil {
		mounts = append(mounts, sandbox.Mount{Source: hooksDir, Destination: githooks.SandboxDir, Options: []string{"ro"}})
	}
	if gh != nil {
		if gh.CertSourcePath != "" {
			mounts = append(mounts, sandbox.Mount{Source: gh.CertSourcePath, Destination: sandboxGHInjectorCert, Options: []string{"ro"}})
		}
		if _, err := os.Stat(hostGHBinaryPath); err == nil {
			mounts = append(mounts, sandbox.Mount{Source: hostGHBinaryPath, Destination: sandboxGHBinary, Options: []string{"ro"}})
		}
	}
	// The stat guard matches the SDK path's: a staging dir that vanished (a
	// swept orphan on a cold cross-executor resume) must not fail the launch
	// on the broker's source-resolution check. The agent then continues from
	// its transcript without the skill file, which is a discovery
	// degradation, not a broken run.
	if skillsSource != "" {
		if _, err := os.Stat(skillsSource); err == nil {
			mounts = append(mounts, sandbox.Mount{Source: skillsSource, Destination: sandbox.TrustedSkillsDestination, Options: []string{"ro"}})
		} else {
			agentprocLog.Warn("step-skill staging dir missing; launching the tool host without the skills mount", "path", skillsSource, "error", err)
		}
	}
	// Same stat guard, same reasoning: a swept staging dir costs this step its
	// predecessor's notes, which is a degraded handoff rather than a broken run.
	if memorySource != "" {
		if _, err := os.Stat(memorySource); err == nil {
			mounts = append(mounts, sandbox.Mount{Source: memorySource, Destination: sandbox.TrustedMemoryDestination, Options: []string{"ro"}})
		} else {
			agentprocLog.Warn("entity-memory staging dir missing; launching the tool host without the memory mount", "path", memorySource, "error", err)
		}
	}
	return mounts, nil
}
