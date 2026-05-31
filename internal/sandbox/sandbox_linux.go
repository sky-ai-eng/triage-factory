//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// teardownState collects everything Close needs to undo. Populated
// incrementally during wrap so partial-failure paths can still call
// Close() and have it Just Work via the per-step ENOENT/ESRCH-tolerant
// helpers below.
type teardownState struct {
	subnetIdx       uint8
	netnsPath       string
	hostVethName    string
	sandboxVethName string
	iptablesRule    iptablesRule
	// egressRules are the host-side SKY-395 Part B egress-allowlist
	// rules (filter/INPUT + filter/FORWARD DROPs on the run's veth).
	// The in-netns OUTPUT rules vanish with the netns and aren't
	// tracked here.
	egressRules []iptablesRule
	bundleDir   string
}

// iptablesRule names a single MASQUERADE rule so teardown can
// remove exactly what we added. Stored as the literal -A arguments
// so the teardown -D call mirrors the insertion verbatim.
type iptablesRule struct {
	table string // "nat"
	chain string // "POSTROUTING"
	args  []string
}

// wrap is the Linux-only implementation of the public Wrap entry
// point. Orchestrates the 12-step pipeline from SKY-254's body —
// subnet allocation, netns + veth + iptables, rootfs cache, OCI
// bundle on disk, runsc command construction.
//
// Error paths trigger a partial-state Close() before returning so
// the caller doesn't need to defer Close when err != nil.
func wrap(ctx context.Context, cfg Config) (*exec.Cmd, *Sandbox, error) {
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

	// Step 1-6: netns + veth + addressing inside the netns.
	netSt, err := setupNetwork(ctx, cfg.RunID, idx)
	if err != nil {
		// netSt may be partially populated; record so Close cleans up.
		if netSt != nil {
			td.netnsPath = netSt.netnsPath
			td.hostVethName = netSt.vethHost
			td.sandboxVethName = netSt.vethSandbox
		}
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	td.netnsPath = netSt.netnsPath
	td.hostVethName = netSt.vethHost
	td.sandboxVethName = netSt.vethSandbox
	sb.Subnet = netSt.subnet
	sb.HostIP = hostIP(idx)
	sb.NetnsPath = netSt.netnsPath

	// Step 7-8 (resolv.conf is set up later inside writeBundle as
	// part of the bundle dir; ip_forward + MASQUERADE here).
	rule, err := applyMasquerade(ctx, netSt.subnet, netSt.upstreamIF)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	td.iptablesRule = rule

	// Step 9: egress allowlist (SKY-395 Part B). Restrict the sandbox
	// to its own gateway IP only — closes the cross-tenant
	// sibling-proxy reach and the direct-internet exfil path. Must run
	// after the netns + veth exist (it installs both in-netns and
	// host-side veth rules) and before the proxies bind in step 9.5, so
	// the allowlist is in force the moment the sandbox can send packets.
	egressRules, err := applyEgressPolicy(ctx, netSt.netnsName, netSt.vethHost, sb.HostIP)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: egress policy: %w", err)
	}
	td.egressRules = egressRules

	// Step 9.5: invoke the proxy-configuration callback (SKY-335) so
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
	rootfsPath, err := ensureRootfs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	spec, err := buildSpec(specCfg, netSt.netnsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	bundleDir, err := writeBundle(cfg, spec, rootfsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	td.bundleDir = bundleDir

	// Step 11: construct the runsc command. Caller runs it via
	// Start + Wait; cmd.Cancel handles ctx cancellation.
	//
	// Container ID must be unique per Wrap or runsc rejects the
	// second concurrent start. RunID isn't unique on its own (some
	// callers pass fixed TraceIDs like "scorer-batch"), but the
	// subnet idx is — the allocator gives a fresh idx for every live
	// Wrap. Pair them so the ID stays grep-friendly while being
	// uniquely distinguishable.
	containerID := fmt.Sprintf("tf-%s-%d", truncate(cfg.RunID, 11), idx)
	cmd := newRunscCommand(ctx, bundleDir, containerID)

	releaseOnError = false
	return cmd, sb, nil
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
// (e.g., from wrap's own error path via the deferred closure).
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

	// Order matters: tear down the iptables rule + network BEFORE
	// removing the bundle dir + releasing the subnet idx. If we
	// freed the idx first, a concurrent allocate could pick it
	// before our teardown finished, and the new run would conflict
	// with our still-lingering veth.

	if err := teardownIptables(ctx, t.iptablesRule); err != nil {
		// Best-effort; log via stderr.
		fmt.Fprintf(os.Stderr, "sandbox: teardown iptables: %v\n", err)
	}
	// Host-side egress-allowlist rules (SKY-395 Part B). The in-netns
	// OUTPUT rules go away with the netns delete below, so only the
	// host-side ones need explicit removal.
	for _, r := range t.egressRules {
		if err := teardownIptables(ctx, r); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox: teardown egress rule: %v\n", err)
		}
	}
	netSt := &netState{
		netnsName:   netnsNameFromPath(t.netnsPath),
		netnsPath:   t.netnsPath,
		vethHost:    t.hostVethName,
		vethSandbox: t.sandboxVethName,
	}
	if err := teardownNetwork(ctx, netSt); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: teardown network: %v\n", err)
	}
	if err := cleanupBundle(t.bundleDir); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: cleanup bundle: %v\n", err)
	}
	defaultAllocator().Release(t.subnetIdx)
	s.teardown = nil // mark closed so re-Close is a no-op
	return nil
}

// netnsNameFromPath extracts "tf-abc-7" from
// "/var/run/netns/tf-abc-7". Empty input returns empty.
func netnsNameFromPath(path string) string {
	if path == "" {
		return ""
	}
	// Last path segment.
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return path
	}
	return path[i+1:]
}
