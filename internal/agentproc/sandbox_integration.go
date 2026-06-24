package agentproc

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// shouldSandbox decides whether the current Run invocation routes
// through the gVisor sandbox. Both conditions must hold:
//
//   - runmode.ModeMulti: local-mode users are trusted with their own
//     creds; sandboxing them is friction without isolation benefit
//     (single tenant). The local→multi-style sandbox toggle could
//     land later as defense-in-depth but isn't a v1 concern.
//   - runtime.GOOS == "linux": gVisor only works on Linux. Multi mode
//     on macOS isn't a supported config (the production runner image
//     is alpine Linux per SKY-256).
func shouldSandbox() bool {
	return runmode.Current() == runmode.ModeMulti && runtime.GOOS == "linux"
}

// sandboxWorkRoot is where the run's Cwd (the run-root) is bind-mounted
// inside the gVisor sandbox; mirrors sandbox/spec.go's Cwd="/work" mount and
// the translateEnvForSandbox / translateAddDirsForSandbox rewrites.
const sandboxWorkRoot = "/work"

// WillSandbox reports whether a Run on this host will route through the gVisor
// sandbox (multi mode + Linux). Callers that must pre-stage sandbox-only
// inputs branch on this: the Curator materializes its pinned repos as shared
// read-only mounts only when a jail is active, and keeps the per-project
// on-disk worktree on the local/non-Linux path. Exported form of the internal
// shouldSandbox gate so the predicate stays single-sourced.
func WillSandbox() bool {
	return shouldSandbox()
}

// cleanRepoMountRelPath validates and normalizes a ReadOnlyRepoMount.RelPath
// as a safe subpath under the agent's Cwd. RelPath is caller-built
// ("repos/<owner>/<repo>"), but this is a sandbox-setup security boundary, so
// reject anything that could place the mount point / bind destination OUTSIDE
// /work: an empty value, an absolute path (filepath.Join would discard the
// /work or Cwd prefix — e.g. RelPath="/etc" → destination "/etc"), the cwd
// itself ("."), or a path that climbs out via "..". Returns the cleaned
// relative path and ok; callers drop the entry on !ok.
func cleanRepoMountRelPath(rel string) (string, bool) {
	if rel == "" {
		return "", false
	}
	cleaned := filepath.Clean(rel)
	if filepath.IsAbs(cleaned) || cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", false
	}
	return cleaned, true
}

// ensureRepoMountPoints creates the empty mount-point directories the
// read-only repo bind-mounts (opts.ReadOnlyRepoMounts) land on, under workCwd.
// They must exist before the per-run chown (so they're owned by the sandbox
// UID like the rest of /work) and before sandbox.Wrap (so each nested bind
// mount has a target inside the /work mount). The shared worktree each mount
// exposes lives OUTSIDE workCwd and is never created or chowned here.
//
// Entries are filtered identically to readOnlyRepoMounts (empty Source or an
// unsafe/empty RelPath dropped), so the mount points created here line up
// exactly with the bind mounts added there — no orphan dir, no missing target.
func ensureRepoMountPoints(workCwd string, mounts []ReadOnlyRepoMount) error {
	for _, m := range mounts {
		rel, ok := cleanRepoMountRelPath(m.RelPath)
		if m.Source == "" || !ok {
			continue
		}
		mp := filepath.Join(workCwd, rel)
		if err := os.MkdirAll(mp, 0o755); err != nil {
			return fmt.Errorf("create repo mount point %s: %w", mp, err)
		}
	}
	return nil
}

// readOnlyRepoMounts translates opts.ReadOnlyRepoMounts into sandbox bind
// mounts: each Source (a shared worktree, a host path outside Cwd) exposed
// read-only at /work/<RelPath>. The "ro" option is the enforcement boundary —
// it makes an in-jail write to the shared tree fail, so one session can't
// mutate what another reads. Entries with no Source or an unsafe/empty RelPath
// (see cleanRepoMountRelPath) are dropped so a bad descriptor can never mount
// Source outside /work.
func readOnlyRepoMounts(mounts []ReadOnlyRepoMount) []sandbox.Mount {
	out := make([]sandbox.Mount, 0, len(mounts))
	for _, m := range mounts {
		rel, ok := cleanRepoMountRelPath(m.RelPath)
		if m.Source == "" || !ok {
			continue
		}
		out = append(out, sandbox.Mount{
			Source:      m.Source,
			Destination: filepath.Join(sandboxWorkRoot, rel),
			// Only "ro" here — sandbox.mountsFromExtra auto-prepends "rbind" for
			// every extra mount (asserted by TestBuildSpec_ReadOnlyRepoMountIsRO),
			// so the final mount is a recursive read-only bind. Don't duplicate
			// "rbind" or it'd appear twice in the spec.
			Options: []string{"ro"},
		})
	}
	return out
}

// AgentVisibleRoot returns the absolute path the agent observes for hostRoot
// when hostRoot is the run's Cwd (the run-root). In sandbox mode the run-root
// is always bind-mounted at /work, so the agent sees "/work" regardless of
// the host path; un-sandboxed the agent runs directly against hostRoot.
//
// Callers interpolate this into the prompts and tool-result messages they
// hand the agent so a concrete absolute memory/scratch path lands where the
// agent's file tools can actually write it. Those tools do no shell env
// expansion, so a bare "$TRIAGE_FACTORY_RUN_ROOT/..." reference would be
// written verbatim; pre-expanding to this value is what makes the path
// resolve identically whether the run is sandboxed or not.
func AgentVisibleRoot(hostRoot string) string {
	if shouldSandbox() {
		return sandboxWorkRoot
	}
	return hostRoot
}

// AgentVisibleBinary returns the path the agent must use to invoke the TF
// CLI. Under the sandbox the host binary is bind-mounted at sandboxTFBinary
// (/usr/local/bin/triagefactory) and the host path doesn't exist inside the
// rootfs, so the agent sees the canonical mount path; un-sandboxed it runs
// the host binary directly.
//
// This is the {{BINARY_PATH}} value prompts interpolate, and it mirrors
// rewriteAllowedToolsForSandbox, which re-points the `Bash(<selfBin> exec *)`
// allowlist pattern to the same mount — without this the prompt would tell a
// sandboxed agent to run a host path that ENOENTs and that the per-tool path
// check would reject anyway.
func AgentVisibleBinary(hostBin string) string {
	if shouldSandbox() {
		return sandboxTFBinary
	}
	return hostBin
}

// buildSandboxEnv constructs the *base* env exposed to the
// sandboxed agent — the slice the sandbox's ConfigureProxies hook
// then appends ANTHROPIC_BASE_URL / placeholder credentials onto
// (see proxies.go). PROPERTY B INVARIANT: this slice contains
// NO credential-shaped entries. The agent's process.env / FDs /
// memory contain only the keys below + the proxy URL/placeholder
// pair that ConfigureProxies adds; a jailbroken agent dumping its
// own state into a tool result / commit message / model response
// leaks nothing usable.
func buildSandboxEnv(extraEnv []string) []string {
	// Floor: just enough for Node to find its binaries + cache dirs.
	// Deliberately minimal; the sandbox's filesystem layout fills in
	// most of what HOME would normally point at.
	//
	// INVARIANT: this base must never set GIT_CONFIG_COUNT (or any
	// GIT_CONFIG_* entry). The sandboxed git's config is delivered as a
	// single consolidated block by startProxiesForSandbox (core.hooksPath +
	// the per-run proxy pairs), which owns GIT_CONFIG_COUNT outright and
	// starts numbering at 0. A second GIT_CONFIG_COUNT here would collide
	// with that block once sandbox.Wrap concatenates the two env slices, and
	// which count git reads becomes platform-dependent (first-wins on glibc,
	// last-wins elsewhere) — silently dropping either the hooks entry or the
	// proxy routing. If a future feature needs sandbox git config, fold its
	// pairs into that block, don't add them here. TestBuildSandboxEnv_NoGitConfig
	// enforces this.
	base := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=/work",
		"TERM=xterm",
	}
	out := make([]string, 0, len(base)+len(extraEnv))
	out = append(out, base...)
	// ExtraEnv carries non-credential run-scoped metadata
	// (TRIAGE_FACTORY_RUN_ID etc). Callers that pass credential
	// env vars in ExtraEnv would violate Property B — that's a
	// caller bug, but the package's existing contract for ExtraEnv
	// is "run-scoped non-credential variables" so we trust it.
	out = append(out, extraEnv...)
	return out
}

// chownWorktreeForSandbox recursively chowns the worktree to the
// uid/gid the sandboxed agent runs as. Without this, the agent
// can't write to its own worktree (EACCES). Idempotent — chowning
// already-correctly-owned files is a no-op at the kernel level.
//
// On non-Linux this is a no-op; the sandbox path isn't reachable
// off Linux per shouldSandbox.
//
// SECURITY: uses os.Lchown (not os.Chown) so a symlink inside the
// repo can't redirect the chown to a host file outside the worktree.
// filepath.Walk does not follow symlinks during the walk itself, so
// the recursion stays inside the worktree; the per-entry Lchown
// ensures we change the link's own owner rather than the target's.
// Without this, a repo containing `link -> /etc/passwd` would chown
// the host's passwd file when this runs as root in multi mode.
func chownWorktreeForSandbox(worktree string) error {
	if worktree == "" {
		return nil
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	return filepath.Walk(worktree, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if cerr := os.Lchown(path, sandbox.WorktreeUID, sandbox.WorktreeGID); cerr != nil {
			return fmt.Errorf("lchown %s: %w", path, cerr)
		}
		return nil
	})
}

// translateEnvForSandbox rewrites absolute host paths embedded in env
// var values to their /work-relative sandbox equivalents. The shape
// matches translateAddDirsForSandbox: same workCwd, same drop-on-
// outside-cwd policy, same pass-through for empty/non-path values.
//
// Why: delegate/resume callers set TRIAGE_FACTORY_RUN_ROOT to a host
// path (e.g. /data/worktrees/<run>) so the agent's memory-gate retry
// message can reference an absolute "$TRIAGE_FACTORY_RUN_ROOT/_scratch/
// entity-memory/<id>.md" path. Inside the sandbox that host path
// doesn't exist — the run root is bind-mounted at /work — so the agent
// would write to a path that resolves to nothing. Translate before
// passing to the sandbox so the env var points where the agent can
// actually reach.
//
// Heuristic: only values matching filepath.IsAbs are candidates for
// translation. Non-absolute values (IDs, flags, "owner/repo" strings)
// pass through unchanged. Absolute paths outside workCwd are dropped
// rather than left dangling — the sandbox can't reach them, and a
// dangling host path in the env is more confusing than a missing var.
func translateEnvForSandbox(env []string, workCwd string) []string {
	if len(env) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			// Malformed entry (no '='); pass through verbatim and
			// let the caller's downstream env handling deal with it.
			out = append(out, kv)
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		if val == "" || !filepath.IsAbs(val) {
			out = append(out, kv)
			continue
		}
		if workCwd == "" {
			// No cwd to relativize against — host path can't be
			// translated; drop to avoid leaking it into the sandbox.
			continue
		}
		rel, err := filepath.Rel(workCwd, val)
		if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
			// Absolute host path outside workCwd. Drop.
			continue
		}
		out = append(out, key+"="+filepath.Join("/work", rel))
	}
	return out
}

// translateAddDirsForSandbox rewrites the host paths in opts.AddDirs
// into their sandbox-side equivalents under /work. The agent's tool
// permission checks consume these via `--add-dir` flags; if we
// leave them as host paths (e.g. /data/worktrees/abc/knowledge-base)
// the agent's path checks reject every write attempt because no such
// path exists inside the sandbox rootfs.
//
// Paths that aren't under cwd are dropped — they're not reachable
// from inside the sandbox, so there's nothing useful to do with
// them. Empty entries are dropped too (matches BuildArgs's own
// behavior).
//
// Returns nil for nil input, an empty slice for an empty/all-dropped
// input, so the caller can distinguish "not set" from "set to nothing
// after filtering."
func translateAddDirsForSandbox(addDirs []string, cwd string) []string {
	if len(addDirs) == 0 {
		return nil
	}
	if cwd == "" {
		// Without cwd we can't compute relative paths; safest to
		// drop everything rather than pass through host paths that
		// don't exist in the sandbox.
		return []string{}
	}
	out := make([]string, 0, len(addDirs))
	for _, dir := range addDirs {
		if dir == "" {
			continue
		}
		// filepath.Rel handles both absolute paths under cwd and
		// already-relative paths. Anything that comes back with
		// ".." prefix is outside cwd; drop it.
		rel, err := filepath.Rel(cwd, dir)
		if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
			continue
		}
		// "/work" + relative path. Use filepath.Join to handle the
		// rel == "." case (cwd itself), which becomes "/work".
		out = append(out, filepath.Join("/work", rel))
	}
	return out
}
