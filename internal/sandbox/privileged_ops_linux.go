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
	// — safe to call against partial or zero-value state. It refuses a
	// state that is not exactly one SetupNetwork reported, and acts on
	// no name in it as given: every name and rule is derived from the
	// network's namespace name.
	TeardownNetwork(ctx context.Context, state NetworkState) error

	// AdmitJailPorts opens TCP ports on the gateway of conversationID's
	// network at subnetIdx to its jail: the listeners of the run's
	// credential sidecar the jail was told to use. SetupNetwork leaves
	// every other connection from the jail to the host dropped, so these
	// are the only ones it can complete. The veth and gateway are derived
	// here from (conversationID, subnetIdx) rather than taken from the
	// caller, and the ports are validated (ValidateJailPorts).
	// TeardownNetwork removes the rules with the network's others, found
	// by veth and gateway.
	AdmitJailPorts(ctx context.Context, conversationID string, subnetIdx uint8, ports []int) error

	// EnsureRootfs idempotently bakes (or returns the cached path to)
	// the curated rootfs matching selector. v1 has exactly one curated
	// variant; selector exists now so a broker-owned catalog can grow
	// without changing this signature later.
	EnsureRootfs(ctx context.Context, selector RootfsSelector) (rootfsPath string, err error)

	// ReapOrphans sweeps leftover netns/veth/iptables/cgroup and runsc
	// container state left by a previous, hard-crashed process. Called
	// once at TF startup.
	ReapOrphans(ctx context.Context) error

	// ChownRunTree hands a run tree's ownership to a jail identity
	// (jailUID, with the run-tree group) so the jailed agent can write its
	// own worktree — a recursive chown, which requires CAP_CHOWN and so
	// lives on the privileged side once the orchestrator's capabilities are
	// dropped at exec. With subpath == "" the whole root is chowned
	// recursively (every claim); a non-empty subpath is the mid-run
	// `workspace add` case — the intermediate directories between root and
	// subpath are chowned shallowly and the subpath tree recursively,
	// exactly ChownWorkspaceCheckoutForSandbox's contract, and root must
	// already be held by jailUID. Validated at this boundary (jailUID inside
	// the jail band; see validateRunTreeRoot for the tree): a compromised
	// orchestrator must not be able to point a CAP_CHOWN-holding broker at
	// /etc, or at an identity of its choosing.
	ChownRunTree(ctx context.Context, root, subpath string, jailUID int) error

	// ParkRunTree gives a run tree's root to ParkedRunTreeUID at the end of
	// the engagement that held it under jailUID, so the engagement's jail
	// identity can be released with no file still carrying it (see
	// ParkedRunTreeUID). The root only, with its mode re-asserted, and only
	// while jailUID owns it: a root another engagement now holds, one no jail
	// identity owns and a missing one are left alone.
	ParkRunTree(ctx context.Context, root string, jailUID int) error

	// PrepareRunSockets creates conversationID's per-run socket directories
	// for the engagement at subnetIdx — the sidecar's (RunSocketDir) and the
	// tool host's (TrustedToolHostSocketDir) — replacing whatever an earlier
	// engagement left there. The socket root is writable by root alone, so
	// this is the only way either directory comes to exist.
	PrepareRunSockets(ctx context.Context, conversationID string, subnetIdx uint8) error

	// ReleaseRunSockets removes the directories PrepareRunSockets made for
	// the engagement at subnetIdx, each only while it still carries that
	// engagement's identity.
	ReleaseRunSockets(ctx context.Context, conversationID string, subnetIdx uint8) error

	// GrantRunStaging gives an orchestrator-owned staging directory (a step
	// skill or an entity-memory tree a launch mounts read-only) to the jail
	// at subnetIdx: group JailGID(subnetIdx), no access for others. The
	// orchestrator carries no jail gid, so the chgrp is privileged.
	GrantRunStaging(ctx context.Context, dir string, subnetIdx uint8) error

	// RemoveRunTree removes a run tree (run root, scratch cwd, parked
	// worktree) and everything under it. After a sandboxed run the tree
	// is owned by a jail uid — files the agent created arrive via the
	// (privileged) gofer with modes the unprivileged orchestrator cannot
	// unlink through — so removal, like the chown above, is a privileged
	// op. Missing path is a no-op success (idempotent, matching the
	// os.RemoveAll callers it replaces). Same boundary validation as
	// ChownRunTree, on the tree's top level only: the contents must be
	// removed regardless of what uids the run left inside.
	RemoveRunTree(ctx context.Context, path string) error
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

// RunCapturer runs one capture in a jail the cap-broker builds. Like the run
// launch it is not part of PrivilegedOps: the manifest crosses a passed-through
// socket, and the capture's members go to a staging file, so neither passes
// through the broker's address space. There is no in-process implementation,
// and so no capture that runs on the host.
type RunCapturer interface {
	// CaptureRunTree runs the capture and returns its JSON manifest.
	CaptureRunTree(ctx context.Context, p CaptureParams) ([]byte, error)
}

// SandboxOps is the full set the orchestrator installs at boot via
// SetPrivilegedOps: the broker-served privileged operations plus the
// always-brokered run launch, sidecar launch and capture. The cap-broker IPC
// client is the sole implementation.
type SandboxOps interface {
	PrivilegedOps
	RunLauncher
	SidecarLauncher
	RunCapturer
}

// NetworkState is the serializable per-run network state SetupNetwork
// returns and TeardownNetwork consumes. Every field is a plain string,
// so the whole thing round-trips as JSON over the broker RPC and
// compares with ==.
type NetworkState struct {
	// Subnet, HostIP, NetnsPath are SetupNetwork's documented return
	// per spec §4.
	Subnet    string
	HostIP    string
	NetnsPath string

	// The names of what SetupNetwork built. TeardownNetwork does not act
	// on them as given: it derives every name from NetnsName and refuses
	// a state that disagrees.
	NetnsName   string
	VethHost    string
	VethSandbox string
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

	// WorkspaceKey is the run's task id — the one run-tree key worktreeScope
	// accepts, so both a first launch and a cold-rehydrated worktree (each
	// built at RunTreeRoot(WorkspaceKey)) pass the launch-time pin. Empty for
	// a taskless run. Descriptive of the run like ConversationID, not a
	// lifecycle key.
	WorkspaceKey string

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

	// JailUID and JailGID are the identity the jail's process runs as —
	// JailUID/JailGID of the run's subnet index, computed orchestrator-side
	// like SidecarLaunchParams.UID and, like it, not trusted for arriving:
	// ValidateLaunchParams requires both inside the jail band, equal, and
	// derived from the index NetnsPath names. That tie is what refuses a
	// launch naming another run's jail identity, which would hand this run's
	// agent the owner of another run's tree and the group of its sockets.
	JailUID, JailGID int

	// StdioSocketPath is the per-run unix socket the orchestrator listens
	// on and the broker dials to hand the runtime its stdio. Populated by
	// the broker client (which owns the listener) at launch time.
	StdioSocketPath string

	// ExtraEgressCIDR is the self-host-only additional egress destination
	// (see Config.ExtraEgressCIDR). Validated against the internal denylist
	// at the boundary; empty for every caller today.
	ExtraEgressCIDR string
}
