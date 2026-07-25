package agentproc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
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
	// RunID is the conversation id — the sandbox container id, the run-tree
	// key, and what the socket directory is named after.
	RunID string
	// MemoryNamespace is the blueprint run id, the run tree's second
	// legitimate key (a cold rehydrate rebuilds under it).
	MemoryNamespace string
	// Worktree is the host path bind-mounted at /work and the working
	// directory every tool call resolves against.
	Worktree string
	// SDKDir is bind-mounted at /sdk. Still required by the spec builder and
	// still mounted here; removing it from the native rootfs is a later
	// phase, and mounting an unused directory changes nothing observable.
	SDKDir string
	// ExtraEnv is host-path-bearing env translated for the sandbox, exactly
	// as the SDK path translates it.
	ExtraEnv []string
	// PrebuiltNetwork and PrebuiltProxyEnv come from the per-run credential
	// sidecar the caller brought up. Required: this process holds no
	// credential, so a native jail without a prebuilt network is a wiring
	// bug, not a case to degrade through.
	PrebuiltNetwork  *sandbox.RunNetwork
	PrebuiltProxyEnv []string
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
	// MemoryLimitMB caps the jail's cgroup. Zero uses the process default.
	MemoryLimitMB int
}

// ToolHostJail is a launched tool-host sandbox. The caller connects to
// SocketPath, drives the engagement, then closes: the server observes EOF at
// a frame boundary and exits, and Close reclaims the jail either way.
type ToolHostJail struct {
	// SocketPath is the host-side path of the tool host's unix socket. It
	// does not exist until the in-jail server binds it, so the loop's dial
	// retries.
	SocketPath string

	run     sandbox.LaunchedRun
	sb      *sandbox.Sandbox
	sockDir string
}

// Close kills the jail, reclaims its cgroup, and removes the socket
// directory. The prebuilt network and sidecar are the caller's to close,
// ordered after this.
func (j *ToolHostJail) Close() error {
	if j == nil {
		return nil
	}
	if j.run != nil {
		_ = j.run.Close()
	}
	if j.sb != nil {
		_ = j.sb.Close()
	}
	if j.sockDir != "" {
		_ = os.RemoveAll(j.sockDir)
	}
	return nil
}

// LaunchToolHost starts the resident tool host as the jail's main process
// for a native engagement.
//
// The shape mirrors the SDK path's sandbox branch — same rootfs, same
// worktree mount and chown, same prebuilt network, same TF-binary/git-hooks/
// gh-channel mounts — with two differences: the pinned entrypoint is the
// tool host rather than node + the SDK wrapper, and a per-run directory is
// mounted read-write for the socket. Everything TF-side still arrives via
// bind mounts and env, never baked into or assumed of the rootfs.
func LaunchToolHost(ctx context.Context, opts ToolHostOptions) (*ToolHostJail, error) {
	if opts.RunID == "" {
		return nil, fmt.Errorf("agentproc: tool host launch requires a run id")
	}
	if opts.Worktree == "" {
		return nil, fmt.Errorf("agentproc: tool host launch requires a worktree")
	}
	if opts.PrebuiltNetwork == nil {
		return nil, fmt.Errorf("agentproc: tool host launch requires a prebuilt run network — multi mode is always per-run-isolated")
	}

	sockDir, err := prepareToolHostSocketDir(opts.RunID)
	if err != nil {
		return nil, err
	}
	jail := &ToolHostJail{
		SocketPath: filepath.Join(sockDir, toolHostSocketName),
		sockDir:    sockDir,
	}

	if err := chownWorktreeForSandbox(ctx, opts.Worktree); err != nil {
		_ = jail.Close()
		return nil, fmt.Errorf("agentproc: chown worktree for tool host: %w", err)
	}

	mounts, err := toolHostMounts(sockDir, opts.GHChannel, opts.SkillsSourcePath)
	if err != nil {
		_ = jail.Close()
		return nil, err
	}

	env := buildSandboxEnv(translateEnvForSandbox(opts.ExtraEnv, opts.Worktree))
	env = append(env, githooks.BinEnvVar+"="+sandboxTFBinary)
	env = append(env, opts.PrebuiltProxyEnv...)
	env = append(env, ghChannelEnv(opts.GHChannel)...)

	// The socket path and cwd are passed explicitly rather than defaulted, so
	// the server resolves tool paths against /work regardless of the process
	// cwd the runtime happens to give it.
	argv := []string{
		sandboxToolHostBinary, "serve",
		"--socket", filepath.Join(sandboxToolHostSocketDir, toolHostSocketName),
		"--cwd", "/work",
	}

	run, sb, err := sandbox.Wrap(ctx, sandbox.Config{
		RunID:           opts.RunID,
		MemoryNamespace: opts.MemoryNamespace,
		Worktree:        opts.Worktree,
		SDKDir:          opts.SDKDir,
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
	return jail, nil
}

func memoryLimitOrDefault(mb int) int {
	if mb > 0 {
		return mb
	}
	return runMemoryLimitMB()
}

// prepareToolHostSocketDir creates the per-run directory the in-jail server
// binds its socket inside, and grants it to the sandbox identity.
//
// The grant is the mirror image of the agenthost socket's: there the
// orchestrator owns the socket and grants it to the jail, here the jail owns
// the socket and the orchestrator must be able to reach it. The directory is
// setgid to the sandbox group so the socket the jail creates carries that
// group, and the server chmods it 0660 after binding — which is what lets an
// orchestrator that is a member of that group connect without anyone holding
// CAP_CHOWN.
func prepareToolHostSocketDir(runID string) (string, error) {
	dir := sandbox.TrustedToolHostSocketDir(runID)
	// A stale directory from a crashed predecessor would carry a dead socket
	// that the server's own stale-socket removal handles, but the directory's
	// mode may not survive; recreate from scratch.
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return "", fmt.Errorf("agentproc: create tool host socket dir %s: %w", dir, err)
	}
	if os.Getuid() == 0 {
		if err := os.Chown(dir, sandbox.WorktreeUID, sandbox.WorktreeGID); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("agentproc: chown tool host socket dir %s: %w", dir, err)
		}
	} else if err := os.Chown(dir, -1, sandbox.WorktreeGID); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("agentproc: chgrp tool host socket dir %s to gid=%d: %w "+
			"(an unprivileged orchestrator must be a member of the sandbox group — the provided image's tf-sandbox — for this owner-legal group grant)",
			dir, sandbox.WorktreeGID, err)
	}
	// setgid so the socket the jail binds inherits the sandbox group even if
	// a future launch changes the jail process's supplementary groups.
	if err := os.Chmod(dir, 0o2770); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("agentproc: chmod tool host socket dir %s: %w", dir, err)
	}
	return dir, nil
}

// toolHostMounts assembles the jail's bind mounts: the tool host binary and
// the socket directory it needs, plus the same TF-side mounts an SDK jail
// gets (the TF binary under both names, the git-hooks dir, the gh channel's
// cert + pinned binary, and this step's staged skill).
func toolHostMounts(sockDir string, gh *GHChannelParams, skillsSource string) ([]sandbox.Mount, error) {
	if _, err := os.Stat(hostToolHostBinaryPath); err != nil {
		return nil, fmt.Errorf("agentproc: tool host binary is not present at %s: %w", hostToolHostBinaryPath, err)
	}
	mounts := []sandbox.Mount{
		{Source: hostToolHostBinaryPath, Destination: sandboxToolHostBinary, Options: []string{"ro"}},
		// Read-write by necessity: the server binds its socket in here.
		{Source: sockDir, Destination: sandboxToolHostSocketDir},
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
	return mounts, nil
}
