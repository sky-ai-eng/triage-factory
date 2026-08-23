//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// teardownState collects everything Close needs to undo. Populated
// incrementally during wrap so partial-failure paths can still call
// Close() and have it Just Work via the per-step ENOENT/ESRCH-tolerant
// helpers below. The per-run memory cgroup is NOT here — it is owned by
// the LaunchedRun wrap returns (created at its launch, torn down by its
// Close), so a caller that abandons the run before Close still reclaims it.
type teardownState struct {
	subnetIdx uint8
	// ownsNetwork is true when wrap allocated the subnet + built the network
	// itself (Config.Network nil). When false the caller stood the network up
	// via SetupRunNetwork and owns its teardown, so Close must NOT tear down
	// the veth/netns or release the subnet idx out from under it.
	ownsNetwork bool
	// netSt is the network state defaultOps.SetupNetwork returned —
	// possibly only a partial prefix of it if setup failed partway
	// through. Passed back to defaultOps.TeardownNetwork verbatim.
	// Zero (and never torn down) when ownsNetwork is false.
	netSt NetworkState
	// The per-run OCI bundle is NOT tracked here: spec ownership lives in the
	// launch, so the bundle is built by the cap-broker (which holds the
	// capabilities) and is owned by the LaunchedRun that launch returns — its
	// Close removes the bundle, so a caller that abandons the run before
	// Sandbox.Close still reclaims it.
}

// iptablesRule names a single MASQUERADE rule so teardown can
// remove exactly what we added. Stored as the literal -A arguments
// so the teardown -D call mirrors the insertion verbatim.
//
// Fields are exported (despite the type itself being unexported)
// because iptablesRule embeds into NetworkState, which
// docs/for-agents/specs/privsep/README.md §4 requires to round-trip as JSON over
// a future broker RPC — encoding/json silently drops unexported
// fields, which would make MasqueradeRule/EgressRules serialize to
// "{}" and lose the teardown data.
type iptablesRule struct {
	Table string // "nat"
	Chain string // "POSTROUTING"
	Args  []string
}

// wrap is the Linux-only implementation of the public Wrap entry
// point. Orchestrates the pipeline — subnet allocation, netns + veth +
// iptables, rootfs cache, OCI bundle on disk — then launches the runtime.
//
// Every privileged step (network, rootfs) is reached only through
// defaultOps (PrivilegedOps); the runtime launch crosses the same broker
// via runLauncher (RunLauncher) — see privileged_ops_linux.go. Both are
// installed by SetPrivilegedOps at boot, so the launch always runs in the
// cap-broker with the run's stdio wired across a passed-through socket.
//
// Error paths trigger a partial-state Close() before returning so
// the caller doesn't need to defer Close when err != nil.
func wrap(ctx context.Context, cfg Config) (LaunchedRun, *Sandbox, error) {
	// Fail fast if runsc is missing rather than letting the
	// subsequent exec.CommandContext succeed and then mysteriously
	// fail on Start with "file not found".
	if _, err := exec.LookPath("runsc"); err != nil {
		return nil, nil, ErrRunscMissing
	}

	// The launch is always brokered; a nil launcher means SetPrivilegedOps
	// never ran (a boot-order bug). Fail before any privileged setup rather
	// than after allocating a subnet + netns we'd only tear back down.
	if runLauncher == nil {
		return nil, nil, fmt.Errorf("sandbox: no run launcher installed (cap-broker not started)")
	}

	// Two provenances for the run's network. When the caller stood one up
	// ahead of Wrap (Config.Network, the executor path — proxies + sidecar are
	// already live on its HostIP), reuse it: no allocation, no SetupNetwork, no
	// ConfigureProxies, and Close leaves its teardown to the caller. Otherwise
	// (the self-contained all/local path) allocate + build + own it.
	var (
		idx         uint8
		netSt       NetworkState
		ownsNetwork bool
	)
	if cfg.Network != nil {
		ns, ok := cfg.Network.netSt.(NetworkState)
		if !ok {
			return nil, nil, fmt.Errorf("sandbox: Config.Network carries no usable network state")
		}
		idx, netSt = cfg.Network.Idx, ns
	} else {
		a, err := defaultAllocator().Allocate()
		if err != nil {
			return nil, nil, err // ErrSubnetsExhausted
		}
		idx, ownsNetwork = a, true
	}

	// Local typed pointer to the teardown state — stored on
	// sb.teardown as `any` so the cross-platform Sandbox struct
	// doesn't drag Linux-only types into other builds.
	td := &teardownState{subnetIdx: idx, ownsNetwork: ownsNetwork}
	sb := &Sandbox{
		ConversationID: cfg.ConversationID,
		SubnetIdx:      idx,
		teardown:       td,
	}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			_ = sb.Close()
		}
	}()

	// Step 1-9: netns + veth + addressing, MASQUERADE, ip_forward, and
	// the Part B egress allowlist — the full "Network" privileged-op
	// bucket (spec §5), routed through defaultOps. td.netSt is stored
	// unconditionally so a partial failure still leaves Close() enough
	// to clean up whatever prefix of setup succeeded. Skipped when the
	// caller supplied the network (already built + owned by them).
	if ownsNetwork {
		ns, err := defaultOps.SetupNetwork(ctx, cfg.ConversationID, idx)
		td.netSt = ns
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox: %w", err)
		}
		netSt = ns
	}
	sb.Subnet = netSt.Subnet
	sb.HostIP = netSt.HostIP
	sb.NetnsPath = netSt.NetnsPath

	// Step 9.5: invoke the proxy-configuration callback so
	// the caller can bind per-run LLM / git proxies on sb.HostIP and
	// return the env entries naming them. The proxies have to be
	// listening before the OCI bundle's env is finalized — that env
	// is what the agent process reads from /proc/self/environ — so
	// we sequence this between network-up and bundle-write. Property
	// B holds because the returned slice contains only URLs +
	// placeholders; the real credentials live on the host inside the
	// proxy processes. Skipped on the caller-owned-network path: those
	// proxies were bound before Wrap and their env is already in cfg.Env.
	specCfg := cfg
	if ownsNetwork && cfg.ConfigureProxies != nil {
		proxyEnv, perr := cfg.ConfigureProxies(sb)
		if perr != nil {
			return nil, nil, fmt.Errorf("sandbox: configure proxies: %w", perr)
		}
		if len(proxyEnv) > 0 {
			merged := make([]string, 0, len(cfg.Env)+len(proxyEnv))
			merged = append(merged, cfg.Env...)
			merged = append(merged, proxyEnv...)
			specCfg.Env = merged
		}
	}

	// Step 10: pre-warm the curated rootfs. The privileged bake (download +
	// chroot apk) stays a distinct op with its own budget — as it was before
	// spec ownership moved — so a cold-cache first launch doesn't have to
	// finish the bake AND the launch inside one RPC window. LaunchRun's
	// spec build then hits the warm cache (a single stat). The selector must
	// match the one LaunchRun resolves (the default → "base").
	rootfsSel := RootfsSelector{}
	if _, err := defaultOps.EnsureRootfs(ctx, rootfsSel); err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}

	// Step 11: launch the runtime, routed through runLauncher (the cap-broker
	// client). The orchestrator does not resolve the rootfs, build the spec,
	// or write the bundle — it hands the launch only the validated DATA below
	// (a rootfs SELECTOR, an env ALLOWLIST, numeric limits, the netns it was
	// given, the mounts, the pinned argv). The broker VALIDATES it, builds the
	// spec from its own fixed template, and execs+supervises runsc with stdio
	// wired to a passed-through socket — so a compromised orchestrator can
	// never make the broker exec arbitrary code with capabilities. The
	// returned LaunchedRun owns the run process, its cgroup, and its bundle;
	// the caller drives Start → stream → Wait → Close.
	//
	// Container ID must be unique per Wrap or runsc rejects the second
	// concurrent start. ConversationID isn't unique on its own (some callers pass
	// fixed TraceIDs like "scorer-batch"), but the subnet idx is — the
	// allocator gives a fresh idx for every live Wrap. Pair them so the ID
	// stays grep-friendly while being uniquely distinguishable.
	containerID := fmt.Sprintf("tf-%s-%d", truncate(cfg.ConversationID, containerIDRunFragmentMax), idx)
	// Published on the Sandbox before the launch so an observer (the resource
	// sampler) reads the id this launch actually used rather than re-deriving
	// it. Set even if the launch below fails: the group may already exist and
	// the caller's teardown is what reclaims it.
	sb.ContainerID = containerID
	run, err := runLauncher.LaunchRun(ctx, LaunchParams{
		ConversationID:  cfg.ConversationID,
		MemoryNamespace: cfg.MemoryNamespace,
		ContainerID:     containerID,
		Rootfs:          rootfsSel,
		Env:             stringsToEnvVars(specCfg.Env),
		Args:            cfg.Argv,
		Worktree:        cfg.Worktree,
		Mounts:          cfg.ExtraMounts,
		Rlimits:         cfg.Rlimits,
		MemoryLimitMB:   cfg.MemoryLimitMB,
		NetnsPath:       netSt.NetnsPath,
		ExtraEgressCIDR: cfg.ExtraEgressCIDR,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}

	releaseOnError = false
	return run, sb, nil
}

// logCgroupFailOpenOnce warns exactly once per process that memory
// ceilings are unavailable — every subsequent run would repeat the
// same environmental failure.
var cgroupFailOpenOnce sync.Once

func logCgroupFailOpenOnce(err error) {
	cgroupFailOpenOnce.Do(func() {
		sandboxLog.Warn("per-run memory ceiling unavailable; runs continue without limits", "error", err)
	})
}

// containerIDRunFragmentMax is how much of a ConversationID a container id carries.
// Named rather than repeated as a literal because the boot sweep's matcher
// (tfContainerIDRE) is built from it: the sweep delete-forces what it
// matches, so a matcher looser than what this package can actually mint is
// reach it has no use for.
const containerIDRunFragmentMax = 11

// truncate cuts s to maxLen chars. Used for container IDs that
// runsc imposes a 64-char practical limit on; we play it short.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// Close tears down everything Wrap created. Idempotent — safe to
// call multiple times, safe to call against a partial-init sandbox
// (e.g., from wrap's own error path via the deferred closure). Every
// privileged step is reached only through defaultOps.
func (s *Sandbox) Close() error {
	if s == nil || s.teardown == nil {
		return nil
	}
	t, ok := s.teardown.(*teardownState)
	if !ok || t == nil {
		// Wrong type or nil — shouldn't happen on Linux (wrap always
		// stores *teardownState), but be defensive.
		return nil
	}
	ctx := context.Background()

	// Only tear the network down when wrap built it. On the caller-owned
	// path (Config.Network) the delegate's RunNetwork.Close reverses it —
	// after the sidecar + run are down — so touching it here would race a
	// still-live sidecar's proxies off the veth and double-release the idx.
	if t.ownsNetwork {
		// Order matters: tear down the network BEFORE releasing the subnet
		// idx. If we freed the idx first, a concurrent allocate could pick it
		// before our teardown finished, and the new run would conflict with our
		// still-lingering veth. The per-run OCI bundle is torn down by the
		// LaunchedRun (its Close removes it) — Sandbox no longer owns it, since
		// PS-P3 moved spec/bundle construction into the launch.
		if err := defaultOps.TeardownNetwork(ctx, t.netSt); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox: teardown network: %v\n", err)
		}
		defaultAllocator().Release(t.subnetIdx)
	}
	s.teardown = nil // mark closed so re-Close is a no-op
	return nil
}

// setupRunNetwork is the Linux implementation behind SetupRunNetwork:
// allocate a subnet idx, then build the per-run network through the same
// brokered privileged op wrap uses. On a SetupNetwork error the returned
// (possibly partial) state is torn down and the idx released so the caller
// never has to Close a failed bring-up.
func setupRunNetwork(ctx context.Context, conversationID string) (*RunNetwork, error) {
	if defaultOps == nil {
		return nil, fmt.Errorf("sandbox: no privileged ops installed (cap-broker not started)")
	}
	idx, err := defaultAllocator().Allocate()
	if err != nil {
		return nil, err // ErrSubnetsExhausted
	}
	netSt, serr := defaultOps.SetupNetwork(ctx, conversationID, idx)
	if serr != nil {
		// netSt carries whatever prefix of setup succeeded — reverse it, then
		// hand the idx back so a partial bring-up leaks neither veth nor slot.
		_ = defaultOps.TeardownNetwork(ctx, netSt)
		defaultAllocator().Release(idx)
		return nil, fmt.Errorf("sandbox: %w", serr)
	}
	return &RunNetwork{
		Idx:       idx,
		Subnet:    netSt.Subnet,
		HostIP:    netSt.HostIP,
		NetnsPath: netSt.NetnsPath,
		netSt:     netSt,
	}, nil
}

// Close reverses SetupRunNetwork: tears the veth/netns/iptables down and
// releases the subnet idx. Idempotent. Teardown before release mirrors
// Sandbox.Close's ordering (a freed idx a concurrent Allocate could reclaim
// mid-teardown would collide with the still-lingering veth). Uses a detached
// context so a cancelled run still drains — teardown must run regardless.
//
// The index is a scarce, MaxSandboxes-wide resource that also keys the run's
// netns name and its sidecar uid, so a teardown that stalls narrows the whole
// executor's concurrency and keeps a name reserved that nothing is using. The
// slow case is invisible from the outside — the run has already ended — so it
// is timed here and reported when it exceeds the budget below, rather than
// being inferred later from a run that could not get a slot.
func (n *RunNetwork) Close() error {
	if n == nil || n.closed {
		return nil
	}
	n.closed = true
	ns, _ := n.netSt.(NetworkState)
	started := time.Now()
	if err := defaultOps.TeardownNetwork(context.Background(), ns); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: teardown run network: %v\n", err)
	}
	defaultAllocator().Release(n.Idx)
	if elapsed := time.Since(started); elapsed > slowNetworkTeardown {
		sandboxLog.Warn("run network teardown was slow; its subnet index stayed held for that long and the slot was unavailable to any other run",
			"subnet_idx", n.Idx, "netns", ns.NetnsName, "took", elapsed)
	}
	return nil
}

// slowNetworkTeardown is how long a per-run network teardown may take before
// it is worth a log line. The work is a handful of brokered `ip`/`iptables`
// invocations, so anything past a second is not the teardown being busy.
const slowNetworkTeardown = time.Second
