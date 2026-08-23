//go:build linux

package sandbox

import (
	"context"
)

// PrivilegedOps is the set of operations that need root-equivalent Linux
// capabilities (CAP_NET_ADMIN, CAP_SYS_ADMIN, CAP_SYS_CHROOT): network
// setup/teardown, rootfs bake, boot-time orphan reap, and the run-tree
// ownership/capture ops. The cap-broker holds those capabilities and
// serves this interface over its socket RPC; hostOps (hostops_linux.go)
// is the in-process implementation the broker process dispatches onto.
//
// Every method's parameters and returns are serializable values (strings,
// ints, structs) that cross the JSON RPC unchanged — no live *exec.Cmd, no
// *os.File. The run launch is a separate concern (RunLauncher below): it
// crosses a passed-through socket rather than the request/response RPC,
// because the run's stdio must never enter the broker's address space.
//
// wrap(), Close(), reapOrphansImpl(), and the run-tree helpers reach the
// broker through this interface; the orchestrator installs the broker's
// IPC client via SetPrivilegedOps at boot.
type PrivilegedOps interface {
	// SetupNetwork creates the per-run netns + veth pair, applies the
	// MASQUERADE + egress-allowlist iptables rules, and ensures
	// ip_forward is enabled — the full "Network" bucket from the
	// privileged-op audit (spec §5). Returns the state TeardownNetwork
	// needs to reverse it; on error the returned state carries
	// whatever prefix of setup succeeded, so a caller that stores it
	// unconditionally (even on error) can still clean up partial state.
	SetupNetwork(ctx context.Context, conversationID string, subnetIdx uint8) (NetworkState, error)

	// TeardownNetwork reverses SetupNetwork: removes the iptables
	// rules, deletes the veth pair + netns. Idempotent and best-effort
	// — safe to call against partial or zero-value state.
	TeardownNetwork(ctx context.Context, state NetworkState) error

	// EnsureRootfs idempotently bakes (or returns the cached path to)
	// the curated rootfs matching selector. v1 has exactly one curated
	// variant; selector exists now so a broker-owned catalog can grow
	// without changing this signature later.
	EnsureRootfs(ctx context.Context, selector RootfsSelector) (rootfsPath string, err error)

	// ReapOrphans sweeps leftover netns/veth/iptables/cgroup and runsc
	// container state left by a previous, hard-crashed process. Called
	// once at TF startup.
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

	// CaptureRunDelta runs the parked-run capture
	// (`snapshot-capture <worktree> <staging-dir> [session-id]`) in a child
	// dropped to the sandbox uid/gid inside an empty network namespace. The
	// large members land in the validated, parent-owned staging directory;
	// stdout carries only their JSON manifest. The
	// child runs as the sandbox uid for two reasons that share the one
	// drop: a hostile .git filter fires only as the agent, and the SDK's
	// owner-only session transcript is readable. Both halves of that
	// confinement — the setuid and the CLONE_NEWNET — need capabilities
	// the orchestrator no longer holds, so the exec runs privileged-side
	// whole. sessionID is empty when the run carries no session.
	CaptureRunDelta(ctx context.Context, worktree, stagingDir, sessionID string) ([]byte, error)
}

// SidecarLaunchParams is the serializable, validated input to
// SidecarLauncher.LaunchSidecar — the narrow RPC-shaped translation of the
// cross-platform SidecarConfig, exactly as LaunchParams is Config's
// RPC-shaped translation. UID/GID are computed orchestrator-side
// (SidecarUID(cfg.SubnetIdx)) but are NOT trusted merely for arriving: the
// broker re-validates them fall inside the reserved sidecar band
// (ValidateSidecarLaunchParams) before ever handing them to a setuid
// Credential, so a compromised orchestrator cannot ask the broker to start
// the sidecar as root, as the orchestrator's own uid, or as an arbitrary
// uid outside the reserved band.
type SidecarLaunchParams struct {
	// ContainerID is this sidecar's unique registry key in the broker's run
	// table — distinct from the run's own ContainerID (LaunchRun's), so the
	// two can never collide keying the same map.
	ContainerID string

	// UID and GID are the uid/gid the broker execs the sidecar as via
	// syscall.Credential. Always equal (see SidecarUID's doc); carried as
	// two fields anyway so the wire shape says exactly what a Credential
	// needs, with no implicit "gid mirrors uid" the broker has to assume.
	UID, GID int

	// SubnetIdx is the run's subnet index — the same one that named its netns
	// and veth pair. The broker derives the shared-origin bind address from it
	// (HostVethIP(SubnetIdx):SharedOriginPort) and passes the resulting listener
	// down to the sidecar, which cannot bind a privileged port itself.
	//
	// It is not an independent input: ValidateSidecarLaunchParams requires
	// UID == SidecarUID(SubnetIdx), so the index the broker binds against is the
	// same one the uid it execs at was derived from. That is what keeps this
	// from becoming a second, unvalidated way to name an address — a caller can
	// only ask the broker to bind the veth IP of the subnet whose sidecar slot
	// it is already asking to occupy.
	SubnetIdx uint8

	// StdioSocketPath is the per-run unix socket the orchestrator listens
	// on and the broker dials to hand the sidecar its stdio — the same
	// fd-passthrough pattern LaunchRun's StdioSocketPath uses, so the
	// sidecar's bytes never enter the broker's address space. Like
	// LaunchRun's field, this crosses the RPC unvalidated: dialing a
	// caller-chosen local path costs the broker nothing a compromised
	// orchestrator couldn't already do from its own process (see
	// ValidateCaptureStdoutSocketPath's doc for the identical reasoning).
	StdioSocketPath string
}

// SidecarLauncher launches (and cross-process supervises) one run's
// credential-sidecar process — the sibling of RunLauncher for the per-run
// sidecar harness. Deliberately its own interface, not a second
// method bolted onto RunLauncher: the two launch entirely different
// programs (a gVisor-jailed runsc child vs. a plain capless host process at
// a per-run uid) through different broker RPCs, and keeping them distinct
// interfaces keeps that difference visible at the type level.
type SidecarLauncher interface {
	// LaunchSidecar asks the broker to exec the run-sidecar subcommand as
	// the validated per-run uid, with its stdio wired to a socket this
	// process listens on, and hand back a LaunchedSidecar the caller closes
	// when the run ends.
	LaunchSidecar(ctx context.Context, p SidecarLaunchParams) (LaunchedSidecar, error)
}

// RunLauncher launches (and cross-process supervises) one prepared run.
// It is deliberately NOT part of PrivilegedOps: the request/response RPC
// that backs PrivilegedOps carries only serializable data, but a launch
// hands the runtime a passed-through stdio socket the broker never reads,
// so the run's bytes never enter the broker's address space. The only
// implementation is the cap-broker's IPC client (brokerrun_linux.go) — the
// launch is always brokered; there is no in-process launcher.
type RunLauncher interface {
	// LaunchRun asks the broker to build the OCI bundle from the validated
	// params, exec+supervise runsc with its stdio wired to a socket this
	// process listens on, and hand back a LaunchedRun the caller drives.
	LaunchRun(ctx context.Context, p LaunchParams) (LaunchedRun, error)
}

// SandboxOps is the full set the orchestrator installs at boot via
// SetPrivilegedOps: the broker-served privileged operations plus the
// always-brokered run launch and the always-brokered sidecar launch. The
// cap-broker IPC client is the sole implementation.
type SandboxOps interface {
	PrivilegedOps
	RunLauncher
	SidecarLauncher
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
	// ConversationID is the caller's (possibly non-unique) run identifier, used only
	// for the bundle dir's grep-friendly prefix. ContainerID is the unique
	// key; ConversationID is descriptive.
	ConversationID string

	// MemoryNamespace is the run's blueprint run id — the second run-tree key
	// worktreeScope accepts, so a cold-rehydrated worktree (rebuilt at
	// RunTreeRoot(memoryNamespace)) passes the launch-time pin. Empty for a
	// no-blueprint run. Descriptive of the run like ConversationID, not a lifecycle key.
	MemoryNamespace string

	// ContainerID is the runsc container id — unique among LIVE wraps (a
	// fresh subnet index is folded into it), and grep-friendly (it embeds a
	// ConversationID fragment). It is the per-run lifecycle key: the runsc container
	// id, the per-run cgroup name, and the broker's wait/kill key. The
	// (non-unique) ConversationID deliberately is NOT that key.
	//
	// Sequential engagements of one run DO recur on the same id — same
	// ConversationID, and the allocator hands back the lowest free index — which is
	// safe because the launch clears any state left under it first
	// (DeleteRuntimeState). Deterministic ids are what keep an engagement's
	// retries grep-able as one thing; uniquifying them per attempt would
	// trade that away and orphan a state dir per kill.
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
	// pinned entrypoint (TrustedToolHostBinaryDestination + toolHostServeVerb);
	// the rest are the tool host's arguments. validateArgv enforces the pin
	// so the orchestrator can vary arguments but never the executed program.
	Args []string

	// Worktree is the host path bind-mounted read-write at /work. Mounts are
	// additional run-data bind mounts (TF binary, agenthost socket, git
	// hooks, shared read-only repo checkouts). These are bind SOURCES into
	// the already-unprivileged sandbox, not the rootfs and not a spec — the
	// broker applies its own fixed mount options.
	Worktree string
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
	// the broker client (which owns the listener) at launch time.
	StdioSocketPath string

	// ExtraEgressCIDR is the self-host-only additional egress destination
	// (see Config.ExtraEgressCIDR). Validated against the internal denylist
	// at the boundary; empty for every caller today.
	ExtraEgressCIDR string
}
