package sandbox

import (
	"context"
	"io"
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
	// Zero disables. Check LaunchedRun.OOMKilled() (after Wait) to
	// attribute a killed run to the limit.
	MemoryLimitMB int

	// Rlimits, when non-empty, sets the sandboxed process's POSIX
	// rlimits (RLIMIT_NOFILE / RLIMIT_NPROC). Numeric-only and folded
	// into the broker-owned spec template; an empty slice uses the
	// package's fixed defaults (defaultRlimits). A profile-specific
	// resource shape (a browser sandbox needs more fds than a
	// CI-triage one — sandbox-fleet §5) rides here.
	Rlimits []Rlimit

	// ExtraEgressCIDR, when non-empty, names an additional destination
	// network the sandbox may reach directly at L3 — the self-host-only
	// "raw-L3-to-private" egress variant (sandbox-fleet §3.5). It is a
	// data parameter, not a path or command: validated against the
	// immutable internal denylist (cloud metadata, control-plane subnet,
	// private/link-local ranges, the sandbox subnet pool) before it could
	// ever widen egress, so "validated" means *safe*, not merely
	// well-formed. Empty in every shared-SaaS deployment and in every
	// caller today; the validation gate lives here ahead of the feature
	// that populates it so a compromised orchestrator can never smuggle a
	// denylisted CIDR past the broker.
	ExtraEgressCIDR string

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

// Rlimit is one POSIX rlimit applied to the sandboxed process. Purely
// numeric (a type name plus soft/hard values) so it crosses the broker
// RPC as data — never a path or command — and folds into the
// broker-owned spec template. Type is an RLIMIT_* name validated against
// allowedRlimitTypes before it reaches the spec.
type Rlimit struct {
	Type string `json:"type"`
	Soft uint64 `json:"soft"`
	Hard uint64 `json:"hard"`
}

// Sandbox is the live state for one sandboxed run. Returned by Wrap
// alongside the LaunchedRun; caller MUST invoke Close() (typically via
// defer) regardless of how the run terminates. Sandbox owns the
// network/bundle/subnet teardown; the run process and its memory cgroup
// are owned by the LaunchedRun.
//
// Fields are read-only from outside the package. RunID, Subnet,
// HostIP, NetnsPath are exposed for logs + the proxy wiring point
// (proxies bind on HostIP).
type Sandbox struct {
	RunID     string // sandbox.Config.RunID, preserved for telemetry
	Subnet    string // e.g. "10.42.7.0/24"
	HostIP    string // host-side veth IP, e.g. "10.42.7.1" — proxies bind here
	NetnsPath string // /var/run/netns/tf-<runID>-<idx>

	// SubnetIdx is the allocator index Wrap claimed for this run (the same
	// index Subnet/HostIP/NetnsPath are all derived from). Exposed so a
	// caller can derive the run's credential-sidecar uid from the exact
	// same per-run index (see SidecarUID / LaunchSidecar) rather than
	// re-parsing it out of Subnet or HostIP.
	SubnetIdx uint8

	// teardown holds the platform-specific cleanup state. On Linux
	// it's a *teardownState (defined in sandbox_linux.go); on
	// non-Linux it's always nil because Wrap returns early with
	// ErrUnsupportedPlatform. Typed as any so the cross-platform
	// Sandbox struct doesn't drag the Linux-only types into the
	// non-Linux build. The unused-on-non-Linux lint warning is
	// expected — only the Linux Close() reads this field.
	teardown any //nolint:unused // used by sandbox_linux.go's Close
}

// LaunchedRun is a started (or startable) agent-runtime process — the
// gVisor runsc child. Wrap returns one instead of a bare *exec.Cmd because
// the launch crosses a process boundary: it is a proxy for the
// cap-broker-supervised runsc child, whose Stdin/Stdout are the
// orchestrator's end of a passed-through socket while the broker
// execs+supervises runsc.
//
// Lifecycle: Start the run, then Stdin/Stdout are valid; drive the NDJSON
// stream; Wait blocks until the runtime exits; OOMKilled is valid after
// Wait; Close tears the run down and is idempotent — safe to call before
// Start (abandoned bring-up) and after Wait (normal teardown).
type LaunchedRun interface {
	// Start begins execution. In-process this is cmd.Start(); brokered it
	// issues the launch RPC (the broker dials the stdio socket, wires it to
	// runsc, execs and supervises) and accepts the orchestrator's stdio end.
	Start() error

	// Stdin is the writer for NDJSON steering to the runtime's stdin. Valid
	// only after Start returns nil.
	Stdin() io.WriteCloser

	// Stdout is the reader for the runtime's NDJSON stdout; EOF when the
	// runtime exits. Valid only after Start returns nil.
	Stdout() io.Reader

	// Stderr returns the runtime's captured stderr. In-process this is the
	// runsc child's stderr; brokered it is empty (the broker keeps runsc's
	// stderr on its own side and surfaces it to logs — it is not the
	// agent's payload channel). Meaningful after Wait.
	Stderr() string

	// Wait blocks until the runtime exits and returns its exit error (nil
	// on clean exit).
	Wait() error

	// OOMKilled reports whether the run's memory-ceiling cgroup recorded an
	// OOM kill. Valid only after Wait (in-process it is read before the
	// cgroup is torn down; brokered the broker reports it with the exit).
	OOMKilled() bool

	// Pid is the runtime (runsc parent) process id, for observability such
	// as walking the sandbox process tree. In-process it is the local
	// child's pid after Start; brokered it is 0 — the runtime is the
	// broker's child, not reachable from this process. Valid after Start.
	Pid() int

	// Close tears down the run: kills the runtime if still live and removes
	// its per-run cgroup. Idempotent.
	Close() error
}

// Wrap prepares the sandbox (netns, veth, iptables MASQUERADE, OCI bundle
// on disk) and launches `runsc run` against the bundle, returning a
// LaunchedRun the caller drives (Start → stream → Wait → Close) plus the
// *Sandbox that owns the network/bundle/subnet teardown.
//
// The launch itself is routed across the socket to the cap-broker (the
// installed RunLauncher), which execs+supervises runsc with the run's
// stdio wired to a passed-through socket.
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
func Wrap(ctx context.Context, cfg Config) (LaunchedRun, *Sandbox, error) {
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
