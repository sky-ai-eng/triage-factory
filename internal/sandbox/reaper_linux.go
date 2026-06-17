//go:build linux

package sandbox

import (
	"context"
	"os"
)

// reapOrphansImpl walks /var/run/netns for tf-<id>-<idx> entries and
// tears each down. Then sweeps $TMPDIR for tf-bundle-* directories.
// Called once at TF startup from main() so a hard-crashed previous
// TF process doesn't leak resources indefinitely.
//
// Per-entry failures are logged but not returned — best-effort
// cleanup is the contract. The caller (main) should not block boot
// on this.
//
// Subnet indices are MarkInUse'd before teardown and Released after,
// so a concurrent first-Wrap can't allocate the same idx while we're
// mid-cleanup.
func reapOrphansImpl(ctx context.Context) error {
	const netnsDir = "/var/run/netns"

	entries, err := os.ReadDir(netnsDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No /var/run/netns/ at all — possible in a fresh
			// container before any `ip netns add` has ever run.
			// Nothing to reap.
			return reapBundleOrphans(ctx)
		}
		return err
	}

	for _, e := range entries {
		name := e.Name()
		idx, ok := subnetIdxFromNetnsName(name)
		if !ok {
			// Strict regex match prevents collateral damage to
			// unrelated tf-* names a different process might own.
			continue
		}

		sandboxLog.Info("reaping orphan netns", "netns", name, "subnet_idx", idx)

		// Claim the idx before teardown so a concurrent first-call
		// to Wrap can't allocate it mid-cleanup.
		defaultAllocator().MarkInUse(idx)

		netSt := &netState{
			netnsName: name,
			netnsPath: netnsDir + "/" + name,
			// vethHost / vethSandbox not knowable from the netns
			// name alone — but `ip link delete <vh>` requires the
			// name. Workaround: the kernel auto-removes the veth
			// pair when the netns is deleted, so we don't need to
			// know the veth names. teardownNetwork's `ip link
			// delete` call will fail-soft on unknown name; the
			// subsequent `ip netns delete` does the actual work.
		}
		_ = teardownNetwork(ctx, netSt)

		// Match-by-subnet cleanup of the MASQUERADE rule this orphan
		// would have installed. Since 10.42.0.0/16 is exclusively the
		// sandbox's allocation pool, anything matching -s <our subnet>
		// is unambiguously ours regardless of original upstream IF.
		reapIptablesForSubnet(ctx, subnetCIDR(idx))

		// Same for the SKY-395 Part B host-side egress DROP rules this
		// orphan would have installed (matched by its gateway IP).
		reapEgressForSubnet(ctx, idx)

		defaultAllocator().Release(idx)
	}

	return reapBundleOrphans(ctx)
}
