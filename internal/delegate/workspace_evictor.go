// Runtime eviction of idle parked workspace trees. A parked tree is kept on
// disk as the warm-resume cache; the other thing that reclaims one is the
// startup sweep (worktree.CleanupWithOptions), which an executor that stays up
// for weeks never reaches. Without this sweep such an executor holds a full
// checkout per parked blueprint run indefinitely, and since disk is what bounds
// how many conversations' workspaces one executor can hold, that accumulation
// quietly costs it the capacity to take new work while its CPU and memory sit
// idle.
//
// What makes deleting a tree safe is that it is a cache and not the original:
// the durable snapshot blob is the copy that matters, and a swept tree
// cold-rehydrates from it (ensureWorkspace's cold path — the same thing that
// happens after every multi-mode restart). So eviction never reasons about the
// tree alone. It evicts only where the snapshot state row says the blob was
// `written` AND the blob store confirms the object is there; a tree whose
// snapshot is pending, failed, or absent is the one copy of the agent's
// uncommitted work and is never touched.
//
// This is the warm copy's bound; the durable copy's is the retention reaper
// beside it (snapshot_reaper.go), and the two are deliberately different
// numbers — hours of idleness costs disk on one machine, where a fortnight of
// retention costs object storage the whole fleet shares.

package delegate

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// DefaultWorkspaceEvictAfterMulti is how long a parked or concluded
// workspace tree sits unused on a multi-mode executor before the sweep
// reclaims it. Hours, not days: the executor is fleet infrastructure whose
// disk is the scarce resource, the blob is the durable copy, and a resume
// past this window pays one cold rehydrate — seconds against a park that
// has already been idle for a working day's morning.
const DefaultWorkspaceEvictAfterMulti = 6 * time.Hour

// DefaultWorkspaceEvictInterval is how often the sweep runs. The TTL is in
// hours, so hourly is plenty frequent (and matches the retention reaper); each
// pass is one indexed query that usually finds nothing.
const DefaultWorkspaceEvictInterval = time.Hour

// removeWorkspaceTree is the privileged removal seam, as a package var so a
// test can assert an eviction goes through it. It must stay worktree.RemoveAt:
// in multi mode the tree is owned by the sandbox identity that ran in it, and
// only the cap-broker can unlink those modes — a plain os.RemoveAll here would
// work on a developer's laptop and fail on every executor.
var removeWorkspaceTree = worktree.RemoveAt

// ParseWorkspaceEvictAfter interprets TF_WORKSPACE_EVICT_AFTER_SEC, a
// non-negative integer number of seconds. `0` disables the sweep, and so does
// an empty value in local mode: local is released software running on the
// user's own machine, where the warm resume is the primary experience and the
// disk being consumed is theirs to spend. Setting the knob there opts in with
// identical semantics. Empty in multi mode is DefaultWorkspaceEvictAfterMulti.
//
// A malformed or negative value returns the mode's default plus an error for
// the caller to log: the knob bounds disk on a long-lived executor, so falling
// back to "no eviction at all" on a typo would silently restore the unbounded
// growth it exists to stop.
//
// localMode is a parameter rather than a runmode.Current() read so both
// defaults are testable without env manipulation.
func ParseWorkspaceEvictAfter(raw string, localMode bool) (time.Duration, error) {
	def := time.Duration(0)
	if !localMode {
		def = DefaultWorkspaceEvictAfterMulti
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def, fmt.Errorf(
			"invalid TF_WORKSPACE_EVICT_AFTER_SEC %q (want a non-negative integer number of seconds, 0 to disable); using %s",
			raw, def)
	}
	return time.Duration(n) * time.Second, nil
}

// RunWorkspaceEvictor sweeps at startup and then on a ticker until ctx is
// cancelled. Wired on dispatcher-capable roles alongside RunSnapshotReaper —
// the trees it reclaims are the ones this process's own engagements left
// behind. A non-positive `after` is the disabled knob and returns immediately;
// a missing store or blob store makes it a logged no-op, since without the blob
// store there is no way to confirm a tree is safely evictable.
func (s *Spawner) RunWorkspaceEvictor(ctx context.Context, interval, after time.Duration) {
	if after <= 0 {
		delegateLog.Info("workspace eviction disabled; parked trees are reclaimed only at startup",
			"env", "TF_WORKSPACE_EVICT_AFTER_SEC")
		return
	}
	if s.conversations == nil || s.Storage() == nil {
		delegateLog.Warn("workspace evictor not started: no store / blob store wired")
		return
	}
	// time.NewTicker panics on a non-positive duration; default rather than
	// crash if a caller / future refactor passes one.
	if interval <= 0 {
		interval = DefaultWorkspaceEvictInterval
	}
	delegateLog.Info("workspace evictor started", "evict_after", after, "interval", interval)
	s.EvictIdleWorkspaces(ctx, after)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.EvictIdleWorkspaces(ctx, after)
		}
	}
}

// EvictIdleWorkspaces reclaims every warm workspace tree on THIS host whose
// snapshot key has been idle longer than `after`.
//
// Enumeration is DB-first and disk second, deliberately: the database knows
// which keys are at rest, unclaimed, and durably snapshotted, and the disk
// knows none of that — walking the runs directory would find directories with
// no way to tell a parked cache from a live engagement's cwd. So the query
// names the candidates fleet-wide and each executor keeps only the ones its own
// filesystem actually holds; an executor that never held a key's tree simply
// stats nothing and moves on.
//
// Best-effort throughout: errors are logged, never fatal.
func (s *Spawner) EvictIdleWorkspaces(ctx context.Context, after time.Duration) {
	if s.conversations == nil || s.Storage() == nil {
		return
	}
	cutoff := time.Now().UTC().Add(-after)
	keys, err := s.conversations.ListEvictableWorkspacesSystem(ctx, cutoff)
	if err != nil {
		delegateLog.Warn("workspace evictor: list evictable workspaces failed", "error", err)
		return
	}
	evicted := 0
	for _, key := range keys {
		evicted += s.evictWorkspace(ctx, key)
	}
	if evicted > 0 {
		delegateLog.Info("workspace evictor: reclaimed idle parked workspaces", "trees", evicted)
	}
}

// evictWorkspace removes the trees of one snapshot key that live on this host,
// returning how many it removed. The row's worktree_path is left pointing at
// the deleted directory on purpose: ensureWorkspace stats that path, misses,
// and falls through to the blob — the same contract the startup sweep relies
// on. Rewriting the column would instead have to invent a value for "the tree
// used to be here".
func (s *Spawner) evictWorkspace(ctx context.Context, key domain.EvictableWorkspace) int {
	keyID := memoryNamespace(key.BlueprintRunID)
	local := localRunTrees(key.WorktreePaths)
	if len(local) == 0 {
		return 0 // another executor's tree (or already gone) — nothing here to reclaim
	}

	// A key some engagement is materializing right now is not idle, whatever
	// the query said a moment ago, so the sweep declines it rather than
	// queueing behind a rehydrate that may take minutes. The next pass sees it
	// again; the tree is not going anywhere.
	unlock, ok := s.workspaceLocks.tryLock(workspaceLockKey(key.OrgID, keyID))
	if !ok {
		return 0
	}
	defer unlock()

	// `written` in the state row is a claim about the past; this is the
	// present. They disagree when a blob was reaped by retention, or when the
	// object store lost it — and in both cases the tree in hand is the only
	// copy left, which is the one thing eviction must never destroy.
	present, err := s.Storage().Exists(ctx, snapshotKey(key.OrgID, keyID))
	if err != nil {
		delegateLog.Warn("workspace evictor: snapshot existence check failed; keeping the warm tree",
			"org", key.OrgID, "key_id", keyID, "error", err)
		return 0
	}
	if !present {
		delegateLog.Warn("workspace evictor: snapshot recorded written but absent from the blob store; keeping the warm tree as the only copy",
			"org", key.OrgID, "key_id", keyID)
		return 0
	}

	// The race the lock exists to close: this executor's own dispatcher can
	// claim a conversation on this key between the enumeration and here, and
	// its ensureWorkspace would then hand a live agent the directory below. A
	// claim minted on ANOTHER executor is not this sweep's problem — that
	// engagement resolves its workspace over there, against its own disk.
	busy, err := s.conversations.HasActiveClaimForBlueprintRunSystem(ctx, key.OrgID, key.BlueprintRunID)
	if err != nil {
		delegateLog.Warn("workspace evictor: active-claim re-check failed; keeping the warm tree",
			"org", key.OrgID, "key_id", keyID, "error", err)
		return 0
	}
	if busy {
		return 0
	}

	removed := 0
	for _, path := range local {
		// The session ghost the agent's runtime left in ~/.claude/projects,
		// keyed by the cwd about to disappear. A no-op wherever the sandbox
		// wrote that tree INSIDE the run root instead (RemoveClaudeProjectDir
		// returns early there), which is why this needs no mode branch of its
		// own: the ghost either lives outside the tree and is cleaned here, or
		// lives inside it and goes with the tree below.
		worktree.RemoveClaudeProjectDir(path)
		if err := removeWorkspaceTree(path, keyID); err != nil {
			delegateLog.Warn("workspace evictor: remove failed", "org", key.OrgID, "key_id", keyID, "path", path, "error", err)
			continue
		}
		delegateLog.Info("workspace evictor: evicted idle parked workspace; a resume will rehydrate from the snapshot",
			"org", key.OrgID, "key_id", keyID, "path", path)
		removed++
	}
	return removed
}

// localRunTrees narrows recorded worktree paths to the ones this host can
// actually evict: a directory that exists here, under TF's own ephemeral run
// namespace. The namespace check is the guard that matters — worktree_path is
// a value some other process wrote, and eviction hands it to a privileged
// removal, so a path that is not one of our run trees is refused rather than
// deleted on the strength of a database column.
func localRunTrees(recorded []string) []string {
	var out []string
	for _, p := range recorded {
		if !worktree.IsRunWorktreePath(p) {
			delegateLog.Warn("workspace evictor: recorded worktree_path is not a run tree; refusing to evict it", "path", p)
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue // not on this host, or already reclaimed
		}
		out = append(out, p)
	}
	return out
}

// workspaceLockKey names one snapshot key's workspace for the per-key lock.
// The org is part of it for the same reason it is part of the blob key: the
// namespace is per-tenant, and two tenants' ids must never share a lock.
func workspaceLockKey(orgID, keyID string) string {
	return orgID + "/" + keyID
}
