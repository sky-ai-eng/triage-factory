//go:build linux

package sandbox

import (
	"os"
	"syscall"
)

// The per-run files a run's cell keeps under the socket root, and the two
// moments they are removed.
//
// Every one of them is keyed by RUN ID — the deterministic pre-launch key the
// mount spec, the broker's validation and the jail path all share — so
// sequential engagements of one conversation reuse the exact same paths. That
// makes "tolerate a stale predecessor of the same name" a requirement of cell
// bring-up rather than a nicety, and a graceful teardown can never be the
// mechanism: the engagement that leaves the files behind is precisely the one
// that was killed.
//
// WHY THE ORCHESTRATOR DOES THIS. The socket root is sticky (cmd/capbroker's
// listen and runBroker pin it root:tf-sandbox 01731 / orchestrator:tf-sandbox
// 01730 — group-write, so every run's sidecar can create its own entries,
// sticky so no run's sidecar can unlink another's). Sticky restricts unlink to
// an entry's own owner and to the DIRECTORY's owner, and the per-run files are
// owned by the sidecar uid of the engagement that created them —
// SidecarUID(subnetIdx), a different uid for every subnet index. So a
// successor sidecar cannot clear its predecessor's files, and neither can the
// sidecar clear its own before it is launched. The orchestrator can: it owns
// the socket root in the split deployment, and is root in the one that doesn't
// split. It is also the process that sequences the cell — it clears, then
// brings the cell up — which is what makes the clear a barrier rather than a
// race.

// ClearRunCellFiles removes every per-run file left under the socket root by an
// earlier engagement of runID: the agenthost socket, the gh-injector cert
// beside it, and the tool-host socket directory.
//
// Called at cell bring-up, before the credential sidecar is launched, so each
// of those is created fresh by — and therefore owned by — the engagement that
// serves it. This is the correctness half of the remove-first rule; the
// teardown-side ReleaseRunCellFiles below is hygiene.
//
// Unconditional: anything found here is by definition a predecessor's. At most
// one claim is active per conversation (schema-enforced), and the run id is the
// conversation's, so no other live engagement can be serving these paths — a
// predecessor whose teardown has not finished yet is being killed either way,
// and its files are exactly what would otherwise be inherited.
//
// Best-effort and silent on a missing path: the common case is that nothing is
// there at all.
func ClearRunCellFiles(runID string) {
	_ = os.Remove(TrustedAgentHostSocketPath(runID))
	_ = os.Remove(TrustedGHInjectorCertPath(runID))
	// Orchestrator-created, unlike the two above, and already recreated from
	// scratch by the run that needs it (agentproc's prepareToolHostSocket) —
	// swept here too so one call clears the whole per-run family rather than
	// most of it.
	_ = os.RemoveAll(TrustedToolHostSocketDir(runID))
}

// ReleaseRunCellFiles removes the files THIS cell's credential sidecar created
// — the agenthost socket and the gh-injector cert — at cell teardown, so a
// tmpfs socket root does not accumulate one orphaned pair per run id forever.
// Hygiene, not correctness: bring-up's ClearRunCellFiles is what makes a stale
// predecessor survivable, and this call is allowed to do nothing.
//
// subnetIdx is the cell's subnet index, and the guard: an entry is removed only
// while it is still owned by SidecarUID(subnetIdx). A successor engagement of
// the same conversation may already have re-created these paths — that overlap
// is the whole reason bring-up clears them — and deleting the live files out
// from under it would trade this bug for a worse one.
//
// The guard is exact, given one ordering requirement the caller owns: call this
// BEFORE releasing the subnet index. While the index is held, no other cell can
// have been assigned it, so no other cell's sidecar can be running at this uid,
// so an entry owned by it is this engagement's own. Once the index is released
// that argument evaporates, and so does the guard.
func ReleaseRunCellFiles(runID string, subnetIdx uint8) {
	uid := SidecarUID(subnetIdx)
	removeIfOwnedBy(TrustedAgentHostSocketPath(runID), uid)
	removeIfOwnedBy(TrustedGHInjectorCertPath(runID), uid)
}

// removeIfOwnedBy unlinks path only if it exists and its owner is uid.
// Lstat, not Stat: the decision is about the entry itself, and a symlink is
// never something this codepath created.
func removeIfOwnedBy(path string, uid int) {
	info, err := os.Lstat(path)
	if err != nil {
		return // already gone, or unreadable — either way, not ours to remove
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != uid {
		return
	}
	if err := os.Remove(path); err != nil {
		sandboxLog.Warn("removing this run's cell file failed; it will be cleared by the next engagement's bring-up",
			"path", path, "error", err)
	}
}
