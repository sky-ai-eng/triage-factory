package worktree

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// This file is the bounded, evictable layer of the one bare+worktree
// cache both modes go through (TFAC-60). The seed/refresh/account half
// lives in worktree.go / worktree_pr.go; here is the
// account-and-evict half: per-bare last-used accounting, a disk-budget +
// TTL reaper, a reconcile pass against the tracked set, and the
// safe-eviction guard that never reclaims a bare with a live worktree.
//
// "One mechanism, policy per mode": the SAME reaper runs in both modes.
// DefaultPolicy hands local no byte budget and no TTL, so a tracked
// repository's bare stays warm on the user's disk however long it sits
// unused; local's caller adds the tracked set (Policy.Tracked), so a bare
// no tracked repository owns is reclaimed. Multi gets a real per-pod byte
// budget + cold-bare TTL, which reclaim those bares too.

const (
	// envCacheMaxBytes overrides the multi-mode per-pod disk budget (bytes).
	envCacheMaxBytes = "TF_BARE_CACHE_MAX_BYTES"
	// envCacheTTL overrides the multi-mode cold-bare TTL (a Go duration, e.g.
	// "336h").
	envCacheTTL = "TF_BARE_CACHE_TTL"

	// defaultMultiBudgetBytes is the per-pod bare-cache disk budget in multi
	// mode when TF_BARE_CACHE_MAX_BYTES is unset. Bares are cloned blobless
	// (git clone --filter=blob:none), so the at-rest cost is mostly the
	// commit/tree graph plus whatever blobs faulted in on checkout; 20 GiB
	// holds a large set of mid-size repos while leaving headroom on a typical
	// pod volume. Operators tune it via the env knob.
	defaultMultiBudgetBytes int64 = 20 << 30 // 20 GiB

	// defaultMultiTTL evicts a bare untouched for this long even when the pod
	// is under its byte budget, so a cold tenant's clones don't sit on disk
	// indefinitely. Off (0) in local.
	defaultMultiTTL = 14 * 24 * time.Hour

	// defaultReaperInterval is how often StartReaper sweeps when the caller
	// passes a non-positive interval.
	defaultReaperInterval = 15 * time.Minute

	// untrackedGrace is how long the reconcile pass keeps a bare outside the
	// tracked set, measured from the bare's last use (see touchBare). The
	// warm and new runs stop using a bare once its repository is untracked,
	// so the bare goes at most a week after the untrack, and sooner if it
	// had already sat unused. Untracking and re-tracking a repository within
	// that window keeps its clone; a clone left behind by a host move or a
	// deleted registry row costs at most a week of disk.
	untrackedGrace = 7 * 24 * time.Hour
)

// Policy controls the bounded bare+worktree cache. The zero value is
// "unbounded": the reaper never evicts anything.
type Policy struct {
	// MaxBytes is the per-pod on-disk budget for all bares. <= 0 disables
	// the disk-budget pass (unbounded).
	MaxBytes int64
	// TTL evicts any bare untouched longer than this, independent of the
	// byte budget. <= 0 disables the TTL pass.
	TTL time.Duration
	// Tracked supplies the reconcile pass's keep-set: a bare whose
	// repository id is not in it is evicted once it has gone untrackedGrace
	// without use. nil disables the pass.
	Tracked TrackedSet
}

// TrackedSet returns the registry ids of the repositories currently
// tracked. ok=false means there is no set to reconcile against this sweep
// (the read failed, or there is nothing to read it from), and the
// reconcile pass evicts nothing; the reaper never treats a failed read as
// an empty set.
type TrackedSet func(ctx context.Context) (ids map[string]bool, ok bool)

// unbounded reports whether this policy disables eviction entirely — every
// pass off. EnforceBudget short-circuits on it.
func (p Policy) unbounded() bool { return p.MaxBytes <= 0 && p.TTL <= 0 && p.Tracked == nil }

// DefaultPolicy returns the cache policy for the current runmode.
//
//   - Local: the unbounded zero Policy — no byte budget and no TTL, so local
//     keeps warming + persisting every tracked repo, never silently evicting
//     the user's own repos off their own disk. The caller sets Tracked,
//     since the tracked set is a database read this package cannot make.
//   - Multi: a per-pod byte budget (TF_BARE_CACHE_MAX_BYTES, default
//     defaultMultiBudgetBytes) plus a cold-bare TTL (TF_BARE_CACHE_TTL,
//     default defaultMultiTTL). This is the launch-blocking bound: N orgs
//     can't share a bare (cross-org sharing is a tenancy boundary), so
//     at-rest storage is additive across tenants and must be reclaimable.
func DefaultPolicy() Policy {
	if runmode.Current() != runmode.ModeMulti {
		return Policy{}
	}
	p := Policy{MaxBytes: defaultMultiBudgetBytes, TTL: defaultMultiTTL}
	if v := strings.TrimSpace(os.Getenv(envCacheMaxBytes)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			p.MaxBytes = n
		} else {
			worktreeLog.Warn("invalid cache max bytes; using default", "env", envCacheMaxBytes, "value", v, "error", err, "default", p.MaxBytes)
		}
	}
	if v := strings.TrimSpace(os.Getenv(envCacheTTL)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			p.TTL = d
		} else {
			worktreeLog.Warn("invalid cache ttl; using default", "env", envCacheTTL, "value", v, "error", err, "default", p.TTL)
		}
	}
	return p
}

// lastUsed records the wall-clock time each bare (keyed by its on-disk
// path) was last seeded, refreshed, or read through one of the worktree
// entry points. The reaper sorts coldest-first on this. Bares this process
// has not touched have no entry; scanBares falls back to the bare dir's
// mtime for them, which touchBare also stamps, so the fallback is the
// bare's last use before the process started.
var (
	lastUsedMu sync.Mutex
	lastUsed   = map[string]time.Time{}
)

// touchBare stamps bareDir as used now. Called from every cache entry
// point (seed, PR/branch worktree setup) so the LRU
// reflects real access, not just clone time.
//
// The stamp is also written to the dir's mtime, because git changes the
// bare's root dir only when it creates or replaces a file directly in it
// (a config write, the first worktree), and a fetch does neither. Without
// it, a restart would make a bare look as cold as its last such write, and
// a bare outside the tracked set would lose its untrackedGrace to the
// restart.
func touchBare(bareDir string) {
	if bareDir == "" {
		return
	}
	now := time.Now()
	lastUsedMu.Lock()
	lastUsed[bareDir] = now
	lastUsedMu.Unlock()
	if err := os.Chtimes(bareDir, now, now); err != nil {
		worktreeLog.Debug("stamp bare mtime failed", "dir", bareDir, "error", err)
	}
}

// bareLastUsed returns the recorded last-used time for bareDir, or
// fallback (the caller passes the dir mtime) when this process never
// touched it.
func bareLastUsed(bareDir string, fallback time.Time) time.Time {
	lastUsedMu.Lock()
	t, ok := lastUsed[bareDir]
	lastUsedMu.Unlock()
	if ok {
		return t
	}
	return fallback
}

// forgetBare drops bareDir's LRU entry — called after eviction so a
// re-seed at the same path starts with a fresh stamp rather than
// inheriting the evicted generation's coldness.
func forgetBare(bareDir string) {
	lastUsedMu.Lock()
	delete(lastUsed, bareDir)
	lastUsedMu.Unlock()
}

// bareEntry is one bare clone the reaper is accounting for: its dir, its
// on-disk size, and its effective last-used time.
type bareEntry struct {
	dir      string
	sizeB    int64
	lastUsed time.Time
}

// EnforceBudget reclaims bare clones until the cache satisfies policy. It
// is the single reaper both modes call.
//
// Local passes DefaultPolicy plus its tracked set, so only the reconcile
// pass runs. Multi passes a real budget + TTL and no tracked set.
//
// Three passes, all skipping any bare that has a live worktree (an
// in-flight delegation run reading it):
//
//   - Reconcile: evict every bare whose repository id policy.Tracked does
//     not return, once it has gone untrackedGrace without use.
//   - TTL: evict every bare untouched longer than policy.TTL.
//   - Budget: while the total on-disk size exceeds policy.MaxBytes, evict
//     the coldest (oldest last-used) bare first.
//
// A re-seed on next use is transparent (EnsureBareClone is idempotent and
// every caller already self-seeds), so eviction is always safe given the
// one invariant — never reclaim an in-use bare — which evictBare enforces
// under the per-repo lock. Returns the number of bares evicted and the
// bytes reclaimed.
func EnforceBudget(ctx context.Context, policy Policy) (evicted int, reclaimed int64) {
	if policy.unbounded() {
		return 0, 0
	}

	entries := scanBares()

	// Reconcile pass: drop a bare no tracked repository owns once it has sat
	// unused through the grace period.
	var untracked int
	if policy.Tracked != nil && len(entries) > 0 {
		if tracked, ok := policy.Tracked(ctx); ok {
			cutoff := time.Now().Add(-untrackedGrace)
			var n int
			var b int64
			entries, n, b = evictWhere(ctx, entries, func(e bareEntry) bool {
				id, ok := repositoryIDFromBareDir(e.dir)
				return ok && !tracked[id] && e.lastUsed.Before(cutoff)
			})
			untracked = n
			evicted += n
			reclaimed += b
		}
	}

	// TTL pass: drop anything colder than the TTL regardless of budget.
	if policy.TTL > 0 {
		cutoff := time.Now().Add(-policy.TTL)
		var n int
		var b int64
		entries, n, b = evictWhere(ctx, entries, func(e bareEntry) bool {
			return e.lastUsed.Before(cutoff)
		})
		evicted += n
		reclaimed += b
	}
	if ctx.Err() != nil {
		return evicted, reclaimed
	}

	// Sum from what the passes above left so the budget pass starts from an
	// accurate footprint regardless of what they evicted or what disappeared
	// underneath them.
	var total int64
	for _, e := range entries {
		total += e.sizeB
	}

	// Budget pass: evict coldest-first until under the byte budget.
	if policy.MaxBytes > 0 && total > policy.MaxBytes {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].lastUsed.Before(entries[j].lastUsed)
		})
		for _, e := range entries {
			if ctx.Err() != nil || total <= policy.MaxBytes {
				break
			}
			if n := evictBare(e); n > 0 {
				evicted++
				reclaimed += n
				total -= n
				continue
			}
			// evictBare returned 0: if the bare vanished externally, drop its
			// size from total so we don't over-evict the next coldest. An
			// in-use bare leaves total untouched (its disk is still occupied).
			if _, err := os.Stat(e.dir); os.IsNotExist(err) {
				total -= e.sizeB
			}
		}
	}

	if evicted > 0 {
		worktreeLog.Info("cache reaper evicted bares",
			"evicted", evicted, "untracked", untracked, "reclaimed_bytes", reclaimed, "budget", budgetLabel(policy.MaxBytes), "ttl", ttlLabel(policy.TTL))
	}
	return evicted, reclaimed
}

// evictWhere evicts every entry evict selects and returns the entries still
// on disk, with the count and bytes it reclaimed. It stops evicting when ctx
// is cancelled and returns the rest as kept.
func evictWhere(ctx context.Context, entries []bareEntry, evict func(bareEntry) bool) (kept []bareEntry, evicted int, reclaimed int64) {
	kept = make([]bareEntry, 0, len(entries))
	for i, e := range entries {
		if ctx.Err() != nil {
			return append(kept, entries[i:]...), evicted, reclaimed
		}
		if evict(e) {
			if n := evictBare(e); n > 0 {
				evicted++
				reclaimed += n
				continue // evicted — drop from kept
			}
			// evictBare returned 0: either the bare is in use (keep it)
			// or it vanished externally between the scan and now (drop
			// it). Distinguish so an already-gone bare doesn't linger in
			// the kept set and inflate the budget pass's total.
			if _, err := os.Stat(e.dir); os.IsNotExist(err) {
				forgetBare(e.dir)
				continue
			}
		}
		kept = append(kept, e)
	}
	return kept, evicted, reclaimed
}

// budgetLabel renders a byte budget for the reaper log line — "off" when
// the disk-budget pass is disabled (<= 0), the byte count otherwise.
func budgetLabel(maxBytes int64) string {
	if maxBytes <= 0 {
		return "off"
	}
	return strconv.FormatInt(maxBytes, 10)
}

// ttlLabel renders a TTL for the reaper log line — "off" when the TTL pass
// is disabled (<= 0), the duration otherwise.
func ttlLabel(ttl time.Duration) string {
	if ttl <= 0 {
		return "off"
	}
	return ttl.String()
}

// StartReaper runs EnforceBudget on a ticker until ctx is cancelled,
// after one immediate sweep. Started in both modes: local's sweeps run the
// reconcile pass against the tracked set its caller supplies, multi's run
// the per-pod budget + TTL from DefaultPolicy. A non-positive interval
// falls back to defaultReaperInterval.
func StartReaper(ctx context.Context, policy Policy, interval time.Duration) {
	if interval <= 0 {
		interval = defaultReaperInterval
	}
	go func() {
		EnforceBudget(ctx, policy)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				EnforceBudget(ctx, policy)
			}
		}
	}()
}

// scanBares walks every cache root and returns one entry per bare
// (<repository id>.git dir) with its on-disk size and effective last-used
// time. The root walk SkipDirs at each .git boundary so it doesn't
// recurse looking for (impossible) nested bares; the per-bare footprint
// is then summed by dirSize, which does traverse that bare's tree.
func scanBares() []bareEntry {
	var out []bareEntry
	for _, root := range bareCacheRoots() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				// A missing root is the normal "this cache tier has no bares
				// yet" state, not an error — e.g. the sentinel-org root
				// (<StateRoot>/repos = /data/repos) in multi mode, where real
				// orgs' bares live under orgs/<id>/repos and the sentinel tier
				// never materializes. filepath.Walk surfaces that as an
				// IsNotExist on the root itself; skip it silently rather than
				// logging boot + per-tick noise (TFAC-397).
				if path == root && os.IsNotExist(err) {
					return nil
				}
				// A walk error on a subdir that exists but can't be read skips
				// whatever bares live under it — the safe direction for
				// eviction (don't reclaim what you can't see), but log it so a
				// misconfigured permissions setup is diagnosable rather than
				// silently under-accounting disk.
				worktreeLog.Warn("cache scan walk failed", "path", path, "error", err)
				return nil
			}
			if info.IsDir() && strings.HasSuffix(path, ".git") {
				out = append(out, bareEntry{
					dir:      path,
					sizeB:    dirSize(path),
					lastUsed: bareLastUsed(path, info.ModTime()),
				})
				return filepath.SkipDir
			}
			return nil
		})
	}
	return out
}

// bareCacheRoots returns every directory tree that may hold bare clones.
// Today repoDir resolves every bare under the sentinel org's root
// (<StateRoot>/repos); once a real orgID is threaded through the
// cache, multi mode lays them under <StateRoot>/orgs/<orgID>/repos, so we
// also glob that tier. Both are scanned so the reaper bounds disk
// regardless of which layout produced the bare. Returns nil when the
// state root can't be resolved (missing $HOME) — the reaper then no-ops
// rather than panicking.
func bareCacheRoots() []string {
	if _, err := paths.StateRootErr(); err != nil {
		return nil
	}
	roots := []string{paths.BareCacheRoot(runmode.LocalDefaultOrgID)}
	if runmode.Current() == runmode.ModeMulti {
		if matches, err := filepath.Glob(filepath.Join(paths.StateRoot(), "orgs", "*", "repos")); err == nil {
			roots = append(roots, matches...)
		}
	}
	return roots
}

// dirSize sums the size of every regular file under dir. Used to account
// a bare's on-disk footprint for the budget pass.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// repositoryIDFromBareDir recovers the repository id from a bare clone path
// of the form <...>/<repository id>.git. Used to take the per-repo lock
// before evicting.
func repositoryIDFromBareDir(dir string) (string, bool) {
	base := filepath.Base(dir)
	if !strings.HasSuffix(base, ".git") {
		return "", false
	}
	id := strings.TrimSuffix(base, ".git")
	if !validRepositoryID(id) {
		return "", false
	}
	return id, true
}

// bareHasLiveWorktrees reports whether bareDir has any linked worktree
// with an on-disk checkout — a live delegation run reading this bare.
// Safe eviction (TFAC-60) must never reclaim such a bare. Reads the bare's
// worktrees/ admin dir directly rather than shelling out to `git worktree
// list` so the reaper's guard stays cheap and lock-free; recordedWorktreePath
// (cleanup.go) parses each entry's gitdir pointer.
func bareHasLiveWorktrees(bareDir string) bool {
	entries, err := os.ReadDir(filepath.Join(bareDir, "worktrees"))
	if err != nil {
		// A missing admin dir means no linked worktrees → safe to evict.
		// Any OTHER error (permission, I/O) means we couldn't evaluate the
		// guard; fail safe by reporting "in use" so eviction skips a bare
		// we can't prove is cold.
		return !os.IsNotExist(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		wt := recordedWorktreePath(filepath.Join(bareDir, "worktrees", e.Name()))
		if wt == "" {
			continue
		}
		if _, err := os.Stat(wt); err != nil {
			continue // checkout gone → this entry isn't live
		}
		// The checkout exists — live by mere existence.
		return true
	}
	return false
}

// evictBare reclaims one bare and its derived (cold) worktree checkouts.
// Takes the per-repo lock and re-checks the live-worktree guard under it,
// so a seed or `git worktree add` that started concurrently wins the race
// — the bare stays. Returns the bytes reclaimed, or 0 when the bare was
// skipped (in use) or already gone.
func evictBare(entry bareEntry) int64 {
	id, ok := repositoryIDFromBareDir(entry.dir)
	if !ok {
		return 0
	}
	mu := lockRepo(id)
	mu.Lock()
	defer mu.Unlock()

	// Re-check under the lock: a worktree may have been added between the
	// scan and now. Never reclaim a bare backing a live checkout.
	if bareHasLiveWorktrees(entry.dir) {
		return 0
	}
	if _, err := os.Stat(entry.dir); err != nil {
		forgetBare(entry.dir)
		return 0 // already gone
	}

	// The guard above proved no checkout is live, so every worktree this
	// bare still registers is cold scratch — remove those dirs too so an
	// eviction doesn't strand a worktree pointing at a now-gone bare.
	// Re-stat the size under the lock to account a fault-in between
	// scan and now.
	removeRegisteredWorktrees(entry.dir)
	size := dirSize(entry.dir)
	if err := os.RemoveAll(entry.dir); err != nil {
		worktreeLog.Error("evict bare failed", "repository_id", id, "error", err)
		return 0
	}
	forgetBare(entry.dir)
	worktreeLog.Debug("evicted bare",
		"repository_id", id, "bytes", size, "idle", time.Since(entry.lastUsed).Round(time.Second))
	return size
}

// removeRegisteredWorktrees deletes the checkout directories of every
// worktree registered against bareDir, read from each admin entry's
// gitdir pointer. Only called from evictBare AFTER the live-worktree
// guard passed, so every checkout here is cold. Best-effort.
func removeRegisteredWorktrees(bareDir string) {
	entries, err := os.ReadDir(filepath.Join(bareDir, "worktrees"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		wt := recordedWorktreePath(filepath.Join(bareDir, "worktrees", e.Name()))
		if wt == "" {
			continue
		}
		if err := os.RemoveAll(wt); err != nil {
			worktreeLog.Warn("evict: remove derived worktree failed", "worktree", wt, "error", err)
		}
	}
}
