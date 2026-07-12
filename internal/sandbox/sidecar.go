package sandbox

import "context"

// SidecarUIDBase is the lowest uid in the per-run credential-sidecar band.
// SidecarUID derives each run's sidecar uid from its subnet index, giving a
// contiguous [SidecarUIDBase, SidecarUIDBase+MaxSandboxes) range — one slot
// per concurrent run, matching the subnet allocator's own ceiling. Distinct
// from WorktreeUID (10000, the fixed uid every gVisor-jailed agent runs as
// *inside* its own sandbox namespace — safe to share because gVisor's Sentry
// boundary isolates them, not a host uid) and the orchestrator's uid (10001
// by default, operator-configurable via TF_ORCHESTRATOR_UID). The sidecar is
// a plain host process with no jail, so distinct real uids ARE the isolation
// boundary here: two sidecars at different uids cannot ptrace or read one
// another even though they share the host's process table. 20000 leaves a
// wide, unambiguous gap above 10000-10001 for an operator who repoints
// TF_ORCHESTRATOR_UID within its own small documented range.
const SidecarUIDBase = 20000

func init() {
	// Assert the reserved band never overlaps the agent uid's own range at
	// compile-checkable constant math — a silent collision here would let a
	// run's sidecar and another run's jailed agent share a uid, defeating
	// the ptrace boundary this whole scheme exists to provide.
	if SidecarUIDBase < WorktreeUID+MaxSandboxes {
		panic("sandbox: SidecarUIDBase collides with the WorktreeUID band")
	}
}

// SidecarUID returns the uid (and gid — the sidecar uses the same numeric
// value for both, like WorktreeUID/WorktreeGID) the run at this subnet
// index's credential sidecar runs as.
func SidecarUID(subnetIdx uint8) int {
	return SidecarUIDBase + int(subnetIdx)
}

// SidecarConfig is the caller-supplied input to LaunchSidecar: just enough
// to derive the per-run uid and a grep-friendly process identity. Mirrors
// Config/Wrap's shape deliberately — LaunchSidecar is the sidecar's analog
// of Wrap, translated on Linux into the narrow, broker-validated
// SidecarLaunchParams exactly as Wrap translates Config into LaunchParams.
type SidecarConfig struct {
	// RunID identifies the agent run, purely for the sidecar's grep-friendly
	// container id (see wrap's own RunID doc) — never trusted for anything
	// privileged.
	RunID string

	// SubnetIdx is THIS run's subnet index, already allocated by Wrap
	// (Sandbox.SubnetIdx) — the same index that names the run's netns and
	// veth pair. Reusing it (rather than a second allocation) is what makes
	// "no two live sidecars share a uid" follow directly from the subnet
	// allocator's own uniqueness guarantee.
	SubnetIdx uint8
}

// LaunchedSidecar is a live, broker-supervised run-sidecar process. Phase 1
// exposes only teardown — the sidecar does no work yet (see
// docs/for-agents/specs/per-run-credential-isolation), so there is nothing
// for a caller to steer or wait on beyond "make it go away."
type LaunchedSidecar interface {
	// Close kills the sidecar and blocks until the broker has reaped it
	// (draining the broker's registry entry), then releases local resources
	// (the stdio socket + its listener). Idempotent.
	Close() error
}

// LaunchSidecar starts this run's capless credential-sidecar process,
// broker-spawned at the uid SidecarUID(cfg.SubnetIdx) derives. Callers
// invoke it once Wrap has returned a live Sandbox (so cfg.SubnetIdx is the
// real allocated index, not a value the caller invented) and tear it down
// via the returned LaunchedSidecar's Close — typically folded into the same
// cleanup sequence that closes the Sandbox itself, ordered so the sidecar is
// killed BEFORE the subnet index is released back to the allocator (else a
// concurrent new run could be handed the same uid while this run's sidecar
// is still exiting).
//
// Non-Linux: returns ErrUnsupportedPlatform without doing any work, exactly
// like Wrap — the credential-isolation epic this belongs to is multi-mode +
// Linux only (see the spec's scope note).
func LaunchSidecar(ctx context.Context, cfg SidecarConfig) (LaunchedSidecar, error) {
	return launchSidecar(ctx, cfg)
}
