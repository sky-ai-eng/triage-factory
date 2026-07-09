//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// teardownState collects everything Close needs to undo. Populated
// incrementally during wrap so partial-failure paths can still call
// Close() and have it Just Work via the per-step ENOENT/ESRCH-tolerant
// helpers below. The per-run memory cgroup is NOT here — it is owned by
// the LaunchedRun wrap returns (created at its launch, torn down by its
// Close), so a caller that abandons the run before Close still reclaims it.
type teardownState struct {
	subnetIdx uint8
	// netSt is the network state defaultOps.SetupNetwork returned —
	// possibly only a partial prefix of it if setup failed partway
	// through. Passed back to defaultOps.TeardownNetwork verbatim.
	netSt     NetworkState
	bundleDir string
}

// iptablesRule names a single MASQUERADE rule so teardown can
// remove exactly what we added. Stored as the literal -A arguments
// so the teardown -D call mirrors the insertion verbatim.
//
// Fields are exported (despite the type itself being unexported)
// because iptablesRule embeds into NetworkState, which
// docs/specs/privsep/README.md §4 requires to round-trip as JSON over
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
// Every privileged step (network, rootfs, cgroup, the runtime launch) is
// reached only through defaultOps (PrivilegedOps) — see
// privileged_ops_linux.go. LaunchRun execs runsc in process by default, or
// across the socket in the cap-broker when TF_PRIVSEP is on.
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

	idx, err := defaultAllocator().Allocate()
	if err != nil {
		return nil, nil, err // ErrSubnetsExhausted
	}

	// Local typed pointer to the teardown state — stored on
	// sb.teardown as `any` so the cross-platform Sandbox struct
	// doesn't drag Linux-only types into other builds.
	td := &teardownState{subnetIdx: idx}
	sb := &Sandbox{
		RunID:    cfg.RunID,
		teardown: td,
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
	// to clean up whatever prefix of setup succeeded.
	netSt, err := defaultOps.SetupNetwork(ctx, cfg.RunID, idx)
	td.netSt = netSt
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
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
	// proxy processes.
	specCfg := cfg
	if cfg.ConfigureProxies != nil {
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

	// Step 10: rootfs + OCI bundle.
	rootfsPath, err := defaultOps.EnsureRootfs(ctx, RootfsSelector{})
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	spec, err := buildSpec(specCfg, netSt.NetnsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	bundleDir, err := writeBundle(cfg, spec, rootfsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	td.bundleDir = bundleDir

	// Step 11: launch the runtime, routed through defaultOps. In process
	// this builds the runsc command and clone3s it into a per-run memory
	// cgroup (runsc keeps --ignore-cgroups; TF owns the group, so the whole
	// sandbox tree — sentry + gofer + app memfd — is under the ceiling from
	// the first instruction). Under TF_PRIVSEP this instead crosses to the
	// cap-broker, which execs+supervises runsc with its stdio wired to a
	// passed-through socket. Either way the returned LaunchedRun owns the
	// run process and its cgroup; the caller drives Start → stream → Wait →
	// Close.
	//
	// Container ID must be unique per Wrap or runsc rejects the second
	// concurrent start. RunID isn't unique on its own (some callers pass
	// fixed TraceIDs like "scorer-batch"), but the subnet idx is — the
	// allocator gives a fresh idx for every live Wrap. Pair them so the ID
	// stays grep-friendly while being uniquely distinguishable.
	containerID := fmt.Sprintf("tf-%s-%d", truncate(cfg.RunID, 11), idx)
	run, err := defaultOps.LaunchRun(ctx, LaunchParams{
		BundleDir:     bundleDir,
		ContainerID:   containerID,
		MemoryLimitMB: cfg.MemoryLimitMB,
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

	// Order matters: tear down the network BEFORE removing the bundle
	// dir + releasing the subnet idx. If we freed the idx first, a
	// concurrent allocate could pick it before our teardown finished,
	// and the new run would conflict with our still-lingering veth.
	if err := defaultOps.TeardownNetwork(ctx, t.netSt); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: teardown network: %v\n", err)
	}
	if err := cleanupBundle(t.bundleDir); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: cleanup bundle: %v\n", err)
	}
	defaultAllocator().Release(t.subnetIdx)
	s.teardown = nil // mark closed so re-Close is a no-op
	return nil
}
