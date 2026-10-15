package sandbox

// JailUIDBase is the lowest uid in the per-run jail band. JailUID derives each
// engagement's jail identity from its subnet index, exactly as SidecarUID
// derives the sidecar's, so one allocator hands out both and one release rule
// (RunNetwork.Close) takes both back: [JailUIDBase, JailUIDBase+MaxSandboxes),
// one slot per concurrent run.
//
// The jail's identity is per run because it is a host identity as much as an
// in-sandbox one. Inside gVisor the Sentry is the boundary, but on the host the
// jail's uid owns the run tree, and its gid is the group every one of the run's
// sockets and staging directories is granted to. A value shared across runs
// would make any host process holding it a member of every run.
//
// 40000 sits clear of the ranges hosts commonly allocate on their own: 30000
// and up is Nix's build users (nixbld1..32), systemd's dynamic users start at
// 61184, and subordinate-uid ranges for rootless containers at 100000. A jail
// uid some host account also holds would make that account a member of the
// run.
const JailUIDBase = 40000

// ParkedRunTreeUID owns a run tree's root between engagements. The hand-off
// gives the whole tree to the engaging jail's uid; when the engagement ends the
// root alone is given to this uid before the index is released, so a later
// engagement drawing the same index cannot reach a tree it never held. Nothing
// runs as this uid: it exists to own, and a tree root it owns admits only the
// run-tree group (the orchestrator) and the privileged side.
//
// One root chown is enough. The root is 0770 under the orchestrator's group,
// so a process that owns entries deeper in the tree but not the root, and is
// not in that group, cannot traverse to them.
const ParkedRunTreeUID = JailUIDBase - 1

func init() {
	// Constant math, checked once: a collision between the bands would make a
	// run's sidecar and another run's jail one identity, which is the sharing
	// the per-run bands exist to end.
	if JailUIDBase < SidecarUIDBase+MaxSandboxes && SidecarUIDBase < JailUIDBase+MaxSandboxes {
		panic("sandbox: the jail uid band overlaps the sidecar uid band")
	}
	if IsSidecarUID(ParkedRunTreeUID) || IsJailUID(ParkedRunTreeUID) {
		panic("sandbox: ParkedRunTreeUID falls inside a per-run band")
	}
}

// JailUID returns the uid the jail at this subnet index runs as, and the owner
// its run tree is handed to.
func JailUID(subnetIdx uint8) int {
	return JailUIDBase + int(subnetIdx)
}

// JailGID returns the jail's gid at this subnet index: the same numeric value
// as its uid. It is the group the run's sockets, its socket directories and its
// staging directories are granted to. No other host process carries it: the
// run's sidecar creates its socket and cert in a directory setgid to it.
func JailGID(subnetIdx uint8) int {
	return JailUID(subnetIdx)
}

// IsJailUID reports whether uid falls inside the per-run jail band — the
// broker's check on every uid a caller names for a jail, a run-tree hand-off or
// a capture.
func IsJailUID(uid int) bool {
	return uid >= JailUIDBase && uid < JailUIDBase+MaxSandboxes
}

// jailSubnetIdx inverts JailUID for a uid inside the band.
func jailSubnetIdx(uid int) (uint8, bool) {
	if !IsJailUID(uid) {
		return 0, false
	}
	return uint8(uid - JailUIDBase), true
}

// SidecarJailGID returns the jail gid of the run whose sidecar runs as
// sidecarUID — the group the run's socket and cert must carry when that
// sidecar opens them to the jail. False for a uid outside the sidecar band,
// which is every process that is not a run's sidecar.
func SidecarJailGID(sidecarUID int) (int, bool) {
	if !IsSidecarUID(sidecarUID) {
		return 0, false
	}
	return JailGID(uint8(sidecarUID - SidecarUIDBase)), true
}
