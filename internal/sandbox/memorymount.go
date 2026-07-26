package sandbox

import (
	"os"
	"path/filepath"
)

// The trusted-mount classification for a step's materialized entity-memory tree.
//
// Prior memory is TF-authored content the agent only ever reads — rendered from
// conversation_memory rows by the orchestrator, never edited in place — so it
// travels the same way a step skill does: a read-only bind mount off an
// orchestrator-owned directory rather than files TF writes into the agent's
// workspace. That is what makes it reach a warm blueprint step at all. A run
// tree belongs to the per-run sandbox uid from its first launch onward, so at
// step 2's setup the capability-less orchestrator cannot create the tree the
// step's handoff would live in; without the mount, every step after the first
// reads an empty directory and starts from nothing.
//
// The broker does not CREATE this source — the orchestrator stages it before the
// launch RPC — so, like the step-skill mount and the agenthost socket, the
// broker VALIDATES a launch's claimed source against its own derivation rather
// than overriding it. Producer and validator share the derivation below.

// TrustedMemoryDestination is the fixed in-sandbox path a launch's materialized
// entity-memory tree is bind-mounted at, read-only. Rootfs-absolute and TF-owned
// beside /opt/tf/skills, and deliberately NOT under /work for the reason that
// destination documents: the worktree's contents are agent-authored between
// steps, so an agent-planted symlink at an in-worktree mount destination could
// redirect the mount elsewhere in the jail.
//
// The agent reaches it at the path its prompt names — <run root>/_tfac/
// entity-memory — through a symlink the run tree carries, planted while the tree
// is still orchestrator-owned (see internal/worktree.EnsureSandboxMemoryLink).
const TrustedMemoryDestination = "/opt/tf/memory"

// memoryStagingBasename is the directory per-launch memory staging dirs live
// under inside os.TempDir(): os.TempDir()/triagefactory-memory/<runID>. Sibling
// of skillStagingBasename, with the same ownership contract: never chowned to a
// sandbox identity, never placed anywhere the agent can reach.
const memoryStagingBasename = "triagefactory-memory"

// MemoryStagingBase returns the parent of every per-run staging dir. The startup
// orphan sweep reads it to reclaim staging dirs whose runs are gone.
func MemoryStagingBase() string {
	return filepath.Join(os.TempDir(), memoryStagingBasename)
}

// TrustedMemorySourcePath returns the host path of runID's own memory staging
// dir — both what the orchestrator materializes into and what the broker
// requires a TrustedMemoryDestination mount's source to resolve to. (The broker
// re-derives it from the symlink-resolved MemoryStagingBase, so the per-run
// component must be a real directory; see validateWorktreeAndMounts.)
//
// Keyed on the run id — a blueprint step's own conversation id, distinct per
// step — not the blueprint run id the steps share. That is the whole mechanism:
// each step mounts a tree materialized for IT, which is what lets step N read
// step N-1's handoff while a stale tree from an earlier step can never be
// mounted in its place.
func TrustedMemorySourcePath(runID string) string {
	return filepath.Join(MemoryStagingBase(), runID)
}
