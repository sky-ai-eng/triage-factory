//go:build linux

package sandbox

import (
	"context"
	"os"
)

// PrivilegedOps is the seam between sandbox orchestration and every
// operation in this package that needs root-equivalent Linux
// capabilities (CAP_NET_ADMIN, CAP_SYS_ADMIN, CAP_SYS_CHROOT): network
// setup/teardown, cgroup create/remove, rootfs bake, and boot-time
// orphan reap.
//
// Every method mirrors the RPC vocabulary in
// docs/specs/privsep/README.md §4 so a future out-of-process cap-broker
// (PS-P1) can implement this same interface over a socket client at
// zero call-site churn. Parameters and returns are therefore restricted
// to serializable values (strings, ints, structs) that can cross an
// eventual JSON RPC unchanged — no live *exec.Cmd, no *os.File — with
// one documented exception: SetupRunCgroup's returned fd, which is only
// meaningful to a caller in the same process (see its doc).
//
// wrap(), Close(), and reapOrphansImpl() — the three entry points the
// PS-P0 audit named — call only through this interface for every
// privileged operation. hostOps (hostops_linux.go) is the sole
// in-process implementation; the cap-broker's socket client is the
// out-of-process one. The runsc launch is LaunchRun: the in-process
// implementation execs runsc with ordinary pipes; the broker execs and
// supervises it with its stdio wired to a passed-through socket.
type PrivilegedOps interface {
	// SetupNetwork creates the per-run netns + veth pair, applies the
	// MASQUERADE + egress-allowlist iptables rules, and ensures
	// ip_forward is enabled — the full "Network" bucket from the
	// privileged-op audit (spec §5). Returns the state TeardownNetwork
	// needs to reverse it; on error the returned state carries
	// whatever prefix of setup succeeded, so a caller that stores it
	// unconditionally (even on error) can still clean up partial state.
	SetupNetwork(ctx context.Context, runID string, subnetIdx uint8) (NetworkState, error)

	// TeardownNetwork reverses SetupNetwork: removes the iptables
	// rules, deletes the veth pair + netns. Idempotent and best-effort
	// — safe to call against partial or zero-value state.
	TeardownNetwork(ctx context.Context, state NetworkState) error

	// EnsureRootfs idempotently bakes (or returns the cached path to)
	// the curated rootfs matching selector. v1 has exactly one curated
	// variant; selector exists now so a broker-owned catalog (spec §8)
	// can grow without changing this signature later.
	EnsureRootfs(ctx context.Context, selector RootfsSelector) (rootfsPath string, err error)

	// LaunchRun execs+supervises the gVisor runtime for one prepared
	// bundle and returns a LaunchedRun the caller drives. The in-process
	// implementation (hostOps) builds the runsc command with ordinary
	// pipes for stdio and creates the memory cgroup locally; the broker
	// implementation execs+supervises runsc in the broker process with its
	// stdio wired to a passed-through socket, so the run's bytes never
	// enter the (unprivileged) orchestrator's peer holder. The per-run
	// memory cgroup is created and torn down by whichever side execs the
	// runtime — the cgroup fd is needed only at that side's clone3 and
	// never crosses the interface.
	LaunchRun(ctx context.Context, p LaunchParams) (LaunchedRun, error)

	// SetupRunCgroup creates the per-run memory-ceiling cgroup and
	// returns its directory plus an open dir-fd for exec.Cmd's
	// CgroupFD. Retained from the P1 seam; the runsc launch path now
	// creates its cgroup inside LaunchRun (the fd stays on whichever side
	// execs the runtime), so this is exercised by the P1 conformance tests
	// rather than the live launch path.
	SetupRunCgroup(name string, limitMB int) (dir string, cgroupFD *os.File, err error)

	// RemoveRunCgroup tears down the group SetupRunCgroup created.
	RemoveRunCgroup(dir string) error

	// ReapOrphans sweeps leftover netns/veth/iptables/cgroup state left
	// by a previous, hard-crashed process. Called once at TF startup.
	ReapOrphans(ctx context.Context) error

	// ChownRunTree hands a run tree's ownership to the sandbox identity
	// (WorktreeUID/GID) so the jailed agent can write its own worktree —
	// the op that used to be agentproc's in-process recursive Lchown,
	// which requires CAP_CHOWN and so must live on the privileged side
	// once the orchestrator's capabilities are dropped at exec. With
	// subpath == "" the whole root is chowned recursively (run start);
	// a non-empty subpath is the mid-run `workspace add` case — the
	// intermediate directories between root and subpath are chowned
	// shallowly and the subpath tree recursively, exactly
	// ChownWorkspaceCheckoutForSandbox's contract. Validated at this
	// boundary (see validateRunTreeRoot): a compromised orchestrator
	// must not be able to point a CAP_CHOWN-holding broker at /etc.
	ChownRunTree(ctx context.Context, root, subpath string) error

	// RemoveRunTree removes a run tree (run root, scratch cwd, parked
	// worktree) and everything under it. After a sandboxed run the tree
	// is owned by WorktreeUID — files the agent created arrive via the
	// (privileged) gofer with modes the unprivileged orchestrator cannot
	// unlink through — so removal, like the chown above, is a privileged
	// op. Missing path is a no-op success (idempotent, matching the
	// os.RemoveAll callers it replaces). Same boundary validation as
	// ChownRunTree, on the tree's top level only: the contents must be
	// removed regardless of what uids the run left inside.
	RemoveRunTree(ctx context.Context, path string) error

	// CaptureRunDelta runs the parked-run git-delta capture
	// (`snapshot-capture <worktree>`) in a child dropped to the sandbox
	// uid/gid inside an empty network namespace, and returns the child's
	// raw JSON stdout (a worktree.GitDelta; decoded by the caller so
	// this package doesn't import internal/worktree). Both halves of
	// that child's confinement — the setuid away from the calling
	// identity and the CLONE_NEWNET — need capabilities the orchestrator
	// no longer holds, so the exec moves to the privileged side whole
	// (spec §5's PS-P5). Empty output means "not a git worktree, no
	// delta".
	CaptureRunDelta(ctx context.Context, worktree string) ([]byte, error)
}

// NetworkState is the serializable per-run network state SetupNetwork
// returns and TeardownNetwork consumes. Every field is a plain string
// or a small value struct so the whole thing round-trips as JSON over
// a future broker RPC.
type NetworkState struct {
	// Subnet, HostIP, NetnsPath are SetupNetwork's documented return
	// per spec §4.
	Subnet    string
	HostIP    string
	NetnsPath string

	// The rest is teardown-only bookkeeping. A future broker keeps this
	// broker-internal once Teardown becomes a bare run-id RPC (PS-P3);
	// P0 has no broker process to hold it, so it travels alongside the
	// documented fields so this same process can reverse its own setup
	// without re-deriving names.
	NetnsName      string
	VethHost       string
	VethSandbox    string
	MasqueradeRule iptablesRule
	EgressRules    []iptablesRule
}

// RootfsSelector names which curated rootfs variant the broker should
// resolve and mount — a NAME, never a path. The broker maps it against a
// catalog it owns (rootfsCatalog: name → recipe → content hash) and mounts
// the result read-only, so a compromised orchestrator can select only a
// vetted variant, never point the root at arbitrary host content. An empty
// Name resolves to the "base" variant; v1's catalog is curated (base), and
// additional named variants (browser, org-authored recipes) are later
// catalog rows built by an isolated builder — never inside the broker.
type RootfsSelector struct {
	Name string
}

// EnvVar is one entry of the sandbox environment allowlist. Structured
// (a key and a value, never a raw "K=V" blob) precisely so the broker can
// validate the Key against allowedSandboxEnvKeys before folding it into
// the spec template. A compromised orchestrator can inject any *value* the
// unprivileged agent will see (harmless — that is the agent's own reach),
// but cannot introduce an env key outside the allowlist.
type EnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// LaunchParams is the serializable, validated input to LaunchRun — the
// data the broker folds into the OCI spec it owns. It deliberately
// carries NO config.json, NO rootfs path, and NO free command: the
// privileged frame (capabilities, uid/gid, seccomp, namespaces, the
// content-addressed rootfs) is the broker's fixed template, and these
// fields are only the narrow data that rides inside it. Every field is a
// plain string/int/struct so it round-trips as JSON over the broker RPC
// unchanged — no live *exec.Cmd, no *os.File, no io.Writer.
//
// The broker validates the whole struct (ValidateLaunchParams) at the RPC
// boundary before building anything, so a compromised orchestrator cannot
// steer the broker into running arbitrary code with capabilities — it can
// only supply data the already-unprivileged sandbox sees.
type LaunchParams struct {
	// RunID is the caller's (possibly non-unique) run identifier, used only
	// for the bundle dir's grep-friendly prefix. ContainerID is the unique
	// key; RunID is descriptive.
	RunID string

	// ContainerID is the runsc container id — unique per live Wrap (a fresh
	// subnet index is folded into it), and grep-friendly (it embeds a RunID
	// fragment). It is the per-run lifecycle key: the runsc container id,
	// the per-run cgroup name, and the broker's wait/kill key. The
	// (non-unique) RunID deliberately is NOT that key.
	ContainerID string

	// Rootfs selects the curated rootfs variant by NAME; the broker
	// resolves it against a catalog it owns (name → content hash → path)
	// and mounts the result read-only. Never a path — the empty selector
	// resolves to the "base" variant.
	Rootfs RootfsSelector

	// Env is the sandbox environment as validated key/value pairs. Every
	// Key must be on allowedSandboxEnvKeys; the broker rejects the launch
	// otherwise. The union the proxies + git identity + run metadata need
	// is enumerated there.
	Env []EnvVar

	// Args is the sandbox process argv. Its first two elements MUST be the
	// pinned entrypoint (sandboxNodeBinary + sandboxWrapperEntry); the rest
	// are the wrapper's arguments. validateArgv enforces the pin so the
	// orchestrator can vary arguments but never the executed program.
	Args []string

	// Worktree is the host path bind-mounted read-write at /work, SDKDir
	// the host path bind-mounted read-only at /sdk. Mounts are additional
	// run-data bind mounts (TF binary, agenthost socket, git hooks, shared
	// read-only repo checkouts). These are bind SOURCES into the
	// already-unprivileged sandbox, not the rootfs and not a spec — the
	// broker applies its own fixed mount options.
	Worktree string
	SDKDir   string
	Mounts   []Mount

	// Rlimits is the numeric resource shape; empty uses defaultRlimits.
	Rlimits []Rlimit

	// MemoryLimitMB, when > 0, caps the run via a per-run memory cgroup the
	// launcher creates (fail-open, as before). Zero disables.
	MemoryLimitMB int

	// NetnsPath is the network namespace the broker created for this run in
	// SetupNetwork; the spec joins it instead of creating its own.
	// Validated to the tf-netns naming so a compromised orchestrator cannot
	// point the sandbox at the host netns (which would bypass the egress
	// allowlist).
	NetnsPath string

	// StdioSocketPath is the per-run unix socket the orchestrator listens
	// on and the broker dials to hand the runtime its stdio. Populated by
	// the broker client (which owns the listener); the in-process launcher
	// wires pipes and ignores it.
	StdioSocketPath string

	// ExtraEgressCIDR is the self-host-only additional egress destination
	// (see Config.ExtraEgressCIDR). Validated against the internal denylist
	// at the boundary; empty for every caller today.
	ExtraEgressCIDR string
}
