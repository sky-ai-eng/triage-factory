package sandbox

import (
	"context"
	"os/exec"
)

// Config is the per-run sandbox spec. Caller owns all field values;
// sandbox.Wrap does NOT synthesize from os.Environ or read host state
// beyond what these fields name. The Property B invariant (no
// credentials enter the sandbox) is preserved by ensuring Env
// contains no credential-shaped values; sandbox itself does no
// filtering or injection.
type Config struct {
	// RunID identifies the agent run. Used as the OCI container id
	// passed to runsc and as input to the netns / veth naming.
	// Any string is accepted — sandbox sha1-hashes it internally to
	// derive an 8-hex-char fragment for tf-<frag>-<subnetIdx>, so the
	// reaper's strict hex regex matches regardless of caller shape
	// (UUID, "live-smoke", anything). Same RunID → same fragment.
	RunID string

	// Worktree is the host path bind-mounted at /work inside the
	// sandbox. Caller MUST have created and chowned it to UID
	// WorktreeUID before invoking Wrap, or the agent's writes will
	// EACCES.
	Worktree string

	// SDKDir is the host path containing wrapper.mjs + node_modules.
	// Returned by agentproc.EnsureSDK. Bind-mounted RO at /sdk
	// inside the sandbox.
	SDKDir string

	// Argv is the command exec'd inside the sandbox. Typically
	// ["/usr/bin/node", "/sdk/wrapper.mjs", ...BuildArgs(opts)] —
	// /usr/bin/node is the apk-installed nodejs in the cached alpine
	// rootfs. First element MUST be an absolute path that exists in
	// the sandbox rootfs or via a bind mount (runsc does not invoke
	// a shell to PATH-resolve).
	Argv []string

	// Env is the COMPLETE env exposed to the sandboxed process.
	// sandbox does NOT merge with os.Environ; whatever the caller
	// puts here is what the agent observes in /proc/self/environ.
	// The Property B invariant is preserved by ensuring this slice
	// contains no credentials in multi mode — see internal/agentproc
	// and the package doc.
	Env []string

	// ExtraMounts is additional bind mounts the caller needs (none
	// today — a future change will add a unix-socket mount here for
	// cmd/exec IPC).
	ExtraMounts []Mount

	// MemoryLimitMB, when > 0, runs the sandbox inside a cgroup v2
	// group with memory.max set to this many MiB (swap pinned to 0),
	// so one pathological run OOM-kills alone instead of degrading
	// the host. Fail-open: a host where the cgroup setup can't
	// complete (read-only cgroupfs that won't remount, no memory
	// controller, non-Linux) runs without a ceiling and logs once.
	// Zero disables. Check Sandbox.OOMKilled() before Close to
	// attribute a killed run to the limit.
	MemoryLimitMB int

	// ConfigureProxies, if non-nil, is invoked after the network is
	// set up (subnet allocated, netns + veth created, MASQUERADE
	// applied) but before the OCI bundle is built. The caller
	// receives a partially-populated *Sandbox with HostIP / Subnet /
	// NetnsPath set, so it can bind per-run LLM + git proxies on the
	// host-side veth IP and return their addresses for injection into
	// the sandbox env. The returned slice is appended to Config.Env
	// when constructing the OCI spec.
	//
	// The consumer side owns the proxies: they hold the org's real
	// credential on the host, expose only a proxy URL to the agent.
	// Property B holds because the returned env entries are URLs +
	// placeholders, never the real credential.
	//
	// Lifecycle: sandbox does NOT shut down anything the callback
	// started — the caller (internal/agentproc) tracks the proxy
	// handles via its own defer chain and shuts them down after the
	// agent process exits. If the callback returns an error, Wrap
	// tears down any state it allocated and returns the error.
	ConfigureProxies func(s *Sandbox) (envAdditions []string, err error)
}

// Mount declares an additional bind mount in the sandbox. Source is
// a host path; Destination is the in-sandbox path. Options are
// passed through to the OCI spec's mount options (e.g. ["ro"] for
// read-only); sandbox prepends "rbind" automatically.
type Mount struct {
	Source      string
	Destination string
	Options     []string
}

// Sandbox is the live state for one sandboxed run. Returned by Wrap
// alongside the *exec.Cmd; caller MUST invoke Close() (typically via
// defer) regardless of how the cmd terminates.
//
// Fields are read-only from outside the package. RunID, Subnet,
// HostIP, NetnsPath are exposed for logs + the proxy wiring point
// (proxies bind on HostIP).
type Sandbox struct {
	RunID     string // sandbox.Config.RunID, preserved for telemetry
	Subnet    string // e.g. "10.42.7.0/24"
	HostIP    string // host-side veth IP, e.g. "10.42.7.1" — proxies bind here
	NetnsPath string // /var/run/netns/tf-<runID>-<idx>

	// teardown holds the platform-specific cleanup state. On Linux
	// it's a *teardownState (defined in sandbox_linux.go); on
	// non-Linux it's always nil because Wrap returns early with
	// ErrUnsupportedPlatform. Typed as any so the cross-platform
	// Sandbox struct doesn't drag the Linux-only types into the
	// non-Linux build. The unused-on-non-Linux lint warning is
	// expected — only the Linux Close() reads this field.
	teardown any //nolint:unused // used by sandbox_linux.go's Close
}

// Wrap prepares the sandbox (netns, veth, iptables MASQUERADE, OCI
// bundle on disk) and returns an *exec.Cmd that, when Start+Wait'd,
// invokes `runsc run` against the bundle. The cmd is configured so:
//
//   - cmd.Cancel SIGKILLs the runsc parent; runsc's supervision
//     propagates termination into the sandboxed init.
//   - cmd.Stdout / cmd.Stderr are unset; caller wires them.
//
// PROPERTY B INVARIANT: Wrap does NOT inject credentials into
// cfg.Env, does NOT read os.Environ for ANTHROPIC_*/AWS_*, and does
// NOT call agentproc.resolveCredentials. Caller is responsible for
// the env it passes; the caller's env is intentionally
// credential-free.
//
// On any error, Wrap has cleaned up anything it created — caller
// does NOT need to call Close() unless the returned error is nil.
//
// Non-Linux: returns ErrUnsupportedPlatform without doing any work.
func Wrap(ctx context.Context, cfg Config) (*exec.Cmd, *Sandbox, error) {
	return wrap(ctx, cfg)
}

// ReapOrphans scans /var/run/netns for tf-<id>-<idx> entries and
// bundle dirs in $TMPDIR matching tf-bundle-* and tears down each.
// Called once from main() at TF startup so a hard-crashed previous
// TF process doesn't leak netns/veth/bundle dirs.
//
// Non-Linux: returns nil without scanning (nothing to reap).
//
// Errors from individual entries are logged via the supplied
// context's logger if present; ReapOrphans returns only catastrophic
// failures (e.g. /var/run/netns not readable). Per-entry failures
// are not fatal — best-effort cleanup is the contract.
func ReapOrphans(ctx context.Context) error {
	return reapOrphans(ctx)
}
