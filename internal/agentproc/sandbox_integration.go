package agentproc

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/egressrelay"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// shouldSandbox decides whether agent execution on this host runs inside
// the gVisor sandbox. Both conditions must hold:
//
//   - runmode.ModeMulti: local-mode users are trusted with their own
//     creds; sandboxing them is friction without isolation benefit
//     (single tenant). The local→multi-style sandbox toggle could
//     land later as defense-in-depth but isn't a v1 concern.
//   - runtime.GOOS == "linux": gVisor only works on Linux. Multi mode
//     on macOS isn't a supported config (the production runner image
//     is alpine Linux).
//
// The one consumer of this gate is the native runtime's resident tool-host
// jail (agentproc.LaunchToolHost) — a multi+Linux deployment always runs its
// delegations that way. Run/RunInteractive (the SDK loop) never sandbox: they
// always take the direct spawn, wrapped on Linux in the bubblewrap courtesy
// isolation instead (opts.LocalSandbox), which is not what this predicate
// answers.
func shouldSandbox() bool {
	return runmode.Current() == runmode.ModeMulti && runtime.GOOS == "linux"
}

// SandboxWorkRoot is where the run's Cwd (the run-root) is bind-mounted
// inside the gVisor sandbox; mirrors sandbox/spec.go's Cwd="/work" mount and
// the translateEnvForSandbox / translateAddDirsForSandbox rewrites. It is
// also the sandboxed agent's HOME (buildSandboxEnv), which is why
// worktree.ClaudeProjectDir keys the sandboxed session-transcript location
// off it: the agent's ~/.claude lands inside the /work bind-mount, i.e.
// inside the run's own host-side directory.
const SandboxWorkRoot = "/work"

// WillSandbox reports whether agent execution on this host runs inside the
// gVisor sandbox (multi mode + Linux — the native runtime's resident
// tool-host jail; see shouldSandbox). A caller that must pre-stage
// sandbox-only inputs branches on this. Exported form of the internal
// shouldSandbox gate so the predicate stays single-sourced.
func WillSandbox() bool {
	return shouldSandbox()
}

// errSDKLoopInMultiMode is what Run/RunInteractive return when called in
// multi mode. Multi-mode delegations are always runtime='native' — minted
// that way at the dialect-keyed enqueue, never this process's to choose —
// so a call here is a wiring bug (a regression that stamps 'sdk' again, or a
// new call site that forgot the ratchet), not a config this process can
// route around. Refusing loudly is the fail-closed choice: the SDK loop's
// only isolation on Linux is the bubblewrap courtesy sandbox (not a tenant
// boundary — see internal/agentproc/localsandbox's package doc), so silently
// spawning it in multi mode would run a multi-tenant agent on the bare host
// with none of the gVisor/broker/sidecar isolation multi mode requires.
var errSDKLoopInMultiMode = errors.New("agentproc: the SDK loop (Run/RunInteractive) refuses to spawn in multi mode — multi-mode delegations run through LaunchToolHost's jail, never node directly on the host")

// refuseMultiModeSDKLoop is the one guard Run and RunInteractive both call
// before touching anything else, so the refusal is single-sourced and can't
// drift between the two entry points.
func refuseMultiModeSDKLoop() error {
	if runmode.Current() == runmode.ModeMulti {
		return errSDKLoopInMultiMode
	}
	return nil
}

// AgentVisibleRoot returns the absolute path the agent observes for hostRoot
// when hostRoot is the run's Cwd (the run-root). In sandbox mode the run-root
// is always bind-mounted at /work, so the agent sees "/work" regardless of
// the host path; un-sandboxed the agent runs directly against hostRoot.
//
// Callers interpolate this into the prompts and tool-result messages they
// hand the agent so a concrete absolute memory/scratch path lands where the
// agent's file tools can actually write it. Those tools do no shell env
// expansion, so a bare "$TRIAGE_FACTORY_CONVERSATION_ROOT/..." reference would be
// written verbatim; pre-expanding to this value is what makes the path
// resolve identically whether the run is sandboxed or not.
func AgentVisibleRoot(hostRoot string) string {
	if shouldSandbox() {
		return SandboxWorkRoot
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

// SandboxMarkerEnvVar / SandboxMarkerEnvValue are the marker every
// sandboxed agent process carries, set by buildSandboxEnv — the one env
// assembly point every jail shape goes through. It answers exactly one
// question for code running inside the jail: am I in a jail?
//
// It has to be an explicit marker rather than a heuristic over the rest of
// the env. Every other variable a jailed process can observe — the proxy
// pairs, the git-hooks bin, an unset TF_MODE — has a legitimate non-sandbox
// configuration, so only a variable whose sole writer is this assembler can
// be trusted as the answer. Read it by exact match against
// SandboxMarkerEnvValue; anything else means "not in a jail".
//
// main.go's boot-identity resolution is the reader: with the marker set, a CLI
// invocation is the jailed CLI — a pure RPC client that opens no database and
// fails closed when the exec-verb socket is absent. Non-credential by
// construction, so it belongs in the Property B-safe base set. The direct (unsandboxed) path strips any
// inherited copy — see newDirectCommand — so the marker can only ever mean
// what the assembler meant by it.
const (
	SandboxMarkerEnvVar   = "TRIAGE_FACTORY_SANDBOXED"
	SandboxMarkerEnvValue = "1"
)

// goToolchainEnvKey / goToolchainEnvValue are the Go toolchain policy
// buildSandboxEnv pins onto every sandboxed process. Pulled out as consts
// (rather than inlined) so the assembler can filter a caller-supplied copy
// by the same name before appending its own — the same shape jscJITEnvKey
// uses on the direct path.
const (
	goToolchainEnvKey   = "GOTOOLCHAIN"
	goToolchainEnvValue = "auto"
)

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
		// /opt/tf/bin leads so the TF-pinned gh (bind-mounted there when the
		// real-gh channel is on) wins the PATH race against any gh a custom
		// profile image ships at /usr/local/bin or /usr/bin (TFAC-408). Harmless
		// when nothing is mounted there — an empty dir on PATH is a no-op.
		"PATH=/opt/tf/bin:/usr/local/bin:/usr/bin:/bin",
		"HOME=/work",
		"TERM=xterm",
		// The rootfs installs Go from alpine's apk, and a distro Go ships
		// GOTOOLCHAIN=local baked into its go.env so a distro build never
		// silently swaps toolchains. That default is wrong for us: a repo
		// whose go.mod floor is newer than the packaged Go then fails to
		// build at all, with no path forward the agent can take — it is not
		// root, so it cannot install a package, and the egress allowlist
		// carries no vendor download host. Restoring the upstream default
		// makes the version floor self-healing: cmd/go fetches the toolchain
		// as an ordinary module (golang.org/toolchain) from the module proxy,
		// which is already an allowlisted registry and is checksum-verified
		// like any other module. No new host, no new capability — the same
		// fetch-and-run reach `go build` has for every dependency.
		goToolchainEnvKey + "=" + goToolchainEnvValue,
		// The sandbox's egress is a fail-closed allowlist of package registries
		// (egressproxy.DefaultRegistryHosts); the SDK's non-essential hosts —
		// telemetry, error reporting, the auto-updater, Statsig feature gates —
		// are deliberately absent, since inference rides the per-run LLM proxy
		// rather than direct egress. Left on, that traffic can only produce
		// denied connections: wasted SDK retry loops plus a flood of INFO-level
		// egress denials in the executor log, with nothing gained. Disable it at
		// the source so the jail never opens a connection it can't complete. A
		// pure behavior toggle, no credential — Property B-safe. Sandbox-only:
		// the local direct-spawn path runs on the operator's host with real
		// egress where this traffic legitimately completes, so it stays unset
		// there (buildSandboxEnv feeds only the sandboxed subprocess).
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		// "I am inside the jail" — the marker in-jail code reads to tell an
		// outage apart from a mode signal. Unconditional: both jail shapes
		// assemble their env here, and a marker that is only sometimes set
		// is worse than none.
		SandboxMarkerEnvVar + "=" + SandboxMarkerEnvValue,
	}
	// Engine runtime tuning (JSC JIT off by default). Non-credential by
	// construction, so it belongs in the Property B-safe base set.
	base = append(base, agentRuntimeEnv()...)
	out := make([]string, 0, len(base)+len(extraEnv))
	out = append(out, base...)
	// ExtraEnv carries non-credential run-scoped metadata
	// (TRIAGE_FACTORY_CONVERSATION_ID etc). Callers that pass credential
	// env vars in ExtraEnv would violate Property B — that's a
	// caller bug, but the package's existing contract for ExtraEnv
	// is "run-scoped non-credential variables" so we trust it.
	// The sandbox marker is the one key ExtraEnv may not contribute. It is a
	// fact this function asserts about the process, not a run-scoped setting,
	// and its whole value to the reader is that the assembler is its sole
	// writer: a caller-supplied copy would either duplicate the key (leaving
	// which one the reader sees to platform-dependent duplicate resolution) or
	// override the value outright, so the exact-match read stops meaning "the
	// assembler put me in a jail". Dropped rather than rejected — a caller
	// passing it is confused, not dangerous, and the base entry above is
	// already the right answer.
	//
	// GOTOOLCHAIN is filtered for the same reason: the host's own env very
	// plausibly carries GOTOOLCHAIN=local (alpine's go.env sets it, so
	// anything shelling out from the rootfs picks it up), and the base
	// policy must be authoritative rather than merely positional.
	//
	// The relay catalog's env keys (GOPROXY today) are filtered on the
	// OPPOSITE positional reasoning, and here the filter is load-bearing,
	// not defensive. Duplicate env keys resolve FIRST-wins on Linux —
	// getenv walks environ and returns the first match, and Go's runtime
	// does the same (matching the GIT_CONFIG_COUNT note above) — and the
	// relay's own copy is appended LAST, after this function's output
	// (run.go appends opts.PrebuiltProxyEnv to the assembled env). So an
	// inherited GOPROXY threaded through ExtraEnv would shadow the relay's
	// and point the jail's cmd/go at a host the allowlist doesn't carry,
	// presenting as a broken network. The keys come from the catalog
	// (egressrelay.CatalogEnvKeys) so a future entry's key is protected the
	// moment it exists, with no second list to update.
	drop := append([]string{SandboxMarkerEnvVar, goToolchainEnvKey}, egressrelay.CatalogEnvKeys()...)
	out = append(out, filterEnv(extraEnv, drop)...)
	return out
}

// ghChannelEnv builds the sandbox env entries that point the real gh binary at
// the per-run injector: GH_HOST (the injector's host:port — gh forces https and
// verifies against the mounted cert), GH_ENTERPRISE_TOKEN (the per-run
// placeholder gh sends; the injector strips it and injects the real token),
// SSL_CERT_FILE (the mounted trust file — the system roots plus this run's
// injector leaf, so no rootfs edit and no narrowing of other in-jail TLS
// clients), and the prompt/update-notifier suppressors so gh runs
// non-interactively and never phones home for release checks. Property B-safe:
// the only credential-shaped value is the placeholder, never a real token.
// Empty when the channel is off.
func ghChannelEnv(gc *GHChannelParams) []string {
	if gc == nil || gc.Host == "" {
		return nil
	}
	return []string{
		"GH_HOST=" + gc.Host,
		"GH_ENTERPRISE_TOKEN=" + gc.Token,
		"SSL_CERT_FILE=" + sandboxGHInjectorCert,
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
	}
}

// chownWorktreeForSandbox hands the worktree's ownership to the uid/gid
// the sandboxed agent runs as. Without this, the agent can't write to
// its own worktree (EACCES). Idempotent — chowning already-correctly-
// owned files is a no-op at the kernel level.
//
// Routed through sandbox.ChownRunTree rather than chowning in-process:
// changing a file's owner needs CAP_CHOWN, which the orchestrator no
// longer holds after its exec-time capability drop — the op executes in
// the cap-broker (byte-identical to the recursive Lchown that used to live
// here, including the symlink-safety and ownership-precondition properties
// — see that implementation's doc). No-op off Linux; the sandbox path
// isn't reachable there per shouldSandbox.
func chownWorktreeForSandbox(ctx context.Context, worktree string) error {
	if worktree == "" {
		return nil
	}
	return sandbox.ChownRunTree(ctx, worktree, "")
}

// ChownWorkspaceCheckoutForSandbox chowns a checkout materialized into the run
// root MID-RUN (the agenthost daemon's host-side `workspace add` create,
// TFAC-546) to the sandbox UID. The run-start chown above already covered the
// run root, but a later create runs as the host process and would otherwise
// leave the new subtree unwritable to the jailed agent (EACCES on its own
// checkout). Covers the checkout recursively plus the owner/repo intermediate
// directories the create minted between runRoot and it — those must at least
// be traversable-and-ours so the agent can later add sibling checkouts' mount
// paths, remove its tree, etc.
//
// Same privileged routing as chownWorktreeForSandbox (the subpath form of
// sandbox.ChownRunTree — shallow intermediates, recursive final tree).
// The escape check here is a fast caller-bug guard; the privileged side
// re-validates independently, since this boundary is exactly what a
// compromised orchestrator would try to steer.
func ChownWorkspaceCheckoutForSandbox(ctx context.Context, runRoot, wtDir string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	rel, err := filepath.Rel(runRoot, wtDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("checkout %q is not inside run root %q", wtDir, runRoot)
	}
	return sandbox.ChownRunTree(ctx, runRoot, rel)
}

// translateEnvForSandbox rewrites absolute host paths embedded in env
// var values to their /work-relative sandbox equivalents. The shape
// matches translateAddDirsForSandbox: same workCwd, same drop-on-
// outside-cwd policy, same pass-through for empty/non-path values.
//
// Why: delegate/resume callers set TRIAGE_FACTORY_CONVERSATION_ROOT to a host
// path (e.g. /data/worktrees/<run>) so the agent's memory-gate retry
// message can reference an absolute "$TRIAGE_FACTORY_CONVERSATION_ROOT/_tfac/
// memory.md" path. Inside the sandbox that host path
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
