package worktree

import (
	"context"
	"log"
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
// lives in worktree.go / curator.go / worktree_pr.go; here is the
// account-and-evict half: per-bare last-used accounting, a disk-budget +
// TTL reaper, and the safe-eviction guard that never reclaims a bare
// with a live worktree.
//
// "One mechanism, policy per mode": the SAME reaper runs in both modes,
// but DefaultPolicy hands local an unbounded Policy so every sweep is a
// cheap no-op (the reaper-off knob is literally Policy.unbounded()),
// while multi gets a real per-pod byte budget + cold-bare TTL. Running
// it in local too is deliberate — it keeps the eviction path exercised
// in local dev daily rather than only in prod.

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
)

// Policy controls the bounded bare+worktree cache. The zero value is
// "unbounded" — the local-mode default, under which the reaper never
// evicts anything (today's warm-and-persist behavior).
type Policy struct {
	// MaxBytes is the per-pod on-disk budget for all bares. <= 0 disables
	// the disk-budget pass (unbounded).
	MaxBytes int64
	// TTL evicts any bare untouched longer than this, independent of the
	// byte budget. <= 0 disables the TTL pass.
	TTL time.Duration
}

// unbounded reports whether this policy disables eviction entirely — both
// passes off. EnforceBudget short-circuits on it, which is exactly the
// "reaper off in local" knob.
func (p Policy) unbounded() bool { return p.MaxBytes <= 0 && p.TTL <= 0 }

// DefaultPolicy returns the cache policy for the current runmode.
//
//   - Local: the unbounded zero Policy — the reaper is a no-op, so local
//     keeps warming + persisting every configured repo like today, never
//     silently evicting the user's own repos off their own disk.
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
			log.Printf("[worktree] invalid %s=%q: %v (using default %d)", envCacheMaxBytes, v, err, p.MaxBytes)
		}
	}
	if v := strings.TrimSpace(os.Getenv(envCacheTTL)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			p.TTL = d
		} else {
			log.Printf("[worktree] invalid %s=%q: %v (using default %s)", envCacheTTL, v, err, p.TTL)
		}
	}
	return p
}

// lastUsed records the wall-clock time each bare (keyed by its on-disk
// path) was last seeded, refreshed, or read through one of the worktree
// entry points. The reaper sorts coldest-first on this. Bares seeded
// before this process started have no entry; scanBares falls back to the
// bare dir's mtime for them.
var (
	lastUsedMu sync.Mutex
	lastUsed   = map[string]time.Time{}
)

// touchBare stamps bareDir as used now. Called from every cache entry
// point (seed, curator refresh, PR/branch worktree setup) so the LRU
// reflects real access, not just clone time.
func touchBare(bareDir string) {
	if bareDir == "" {
		return
	}
	lastUsedMu.Lock()
	lastUsed[bareDir] = time.Now()
	lastUsedMu.Unlock()
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
// Local passes the unbounded DefaultPolicy, so this returns immediately —
// the reaper-off-in-local knob is exactly policy.unbounded(). Multi
// passes a real budget + TTL.
//
// Two passes, both skipping any bare that has a live worktree (an
// in-flight delegation run or a live curator session reading it):
//
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
	var total int64
	for _, e := range entries {
		total += e.sizeB
	}

	// TTL pass: drop anything colder than the TTL regardless of budget.
	if policy.TTL > 0 {
		cutoff := time.Now().Add(-policy.TTL)
		kept := make([]bareEntry, 0, len(entries))
		for _, e := range entries {
			if ctx.Err() != nil {
				return evicted, reclaimed
			}
			if e.lastUsed.Before(cutoff) {
				if n := evictBare(e); n > 0 {
					evicted++
					reclaimed += n
					total -= n
					continue
				}
			}
			kept = append(kept, e)
		}
		entries = kept
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
			}
		}
	}

	if evicted > 0 {
		log.Printf("[worktree] cache reaper evicted %d bare(s), reclaimed %d bytes (budget %d, ttl %s)",
			evicted, reclaimed, policy.MaxBytes, policy.TTL)
	}
	return evicted, reclaimed
}

// StartReaper runs EnforceBudget on a ticker until ctx is cancelled,
// after one immediate sweep. Started in both modes so the eviction path
// is exercised in local dev daily; under local's unbounded policy every
// tick is a cheap no-op. Multi gets the real per-pod budget + TTL via
// DefaultPolicy. A non-positive interval falls back to
// defaultReaperInterval.
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
// (owner/repo .git dir) with its on-disk size and effective last-used
// time. The root walk SkipDirs at each .git boundary so it doesn't
// recurse looking for (impossible) nested bares; the per-bare footprint
// is then summed by dirSize, which does traverse that bare's tree.
func scanBares() []bareEntry {
	var out []bareEntry
	for _, root := range bareCacheRoots() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
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
// (<StateRoot>/repos); once SKY-406 threads a real orgID through the
// cache, multi mode lays them under <StateRoot>/orgs/<orgID>/repos, so we
// also glob that tier. Both are scanned so the reaper bounds disk
// regardless of which layout produced the bare. Returns nil when the
// state root can't be resolved (missing $HOME) — the reaper then no-ops
// rather than panicking.
func bareCacheRoots() []string {
	if _, err := paths.StateRootErr(); err != nil {
		return nil
	}
	roots := []string{paths.BareCacheRoot(runmode.LocalDefaultOrg)}
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

// ownerRepoFromBareDir recovers (owner, repo) from a bare clone path of
// the form <...>/<owner>/<repo>.git. Used to take the per-repo lock
// before evicting.
func ownerRepoFromBareDir(dir string) (owner, repo string, ok bool) {
	base := filepath.Base(dir)
	if !strings.HasSuffix(base, ".git") {
		return "", "", false
	}
	repo = strings.TrimSuffix(base, ".git")
	owner = filepath.Base(filepath.Dir(dir))
	if owner == "" || owner == "." || owner == string(filepath.Separator) || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

// bareHasLiveWorktrees reports whether bareDir has any linked worktree
// with an on-disk checkout — a live delegation run or a live curator
// session reading this bare. Safe eviction (TFAC-60) must never reclaim
// such a bare. Reads the bare's worktrees/ admin dir directly rather than
// shelling out to `git worktree list` so the reaper's guard stays cheap
// and lock-free; recordedWorktreePath (cleanup.go) parses each entry's
// gitdir pointer.
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
		if _, err := os.Stat(wt); err == nil {
			return true
		}
	}
	return false
}

// evictBare reclaims one bare and its derived (cold) worktree checkouts.
// Takes the per-repo lock and re-checks the live-worktree guard under it,
// so a seed or `git worktree add` that started concurrently wins the race
// — the bare stays. Returns the bytes reclaimed, or 0 when the bare was
// skipped (in use) or already gone.
func evictBare(entry bareEntry) int64 {
	owner, repo, ok := ownerRepoFromBareDir(entry.dir)
	if !ok {
		return 0
	}
	mu := lockRepo(owner, repo)
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
	// eviction doesn't strand a curator worktree pointing at a now-gone
	// bare. Re-stat the size under the lock to account a fault-in between
	// scan and now.
	removeRegisteredWorktrees(entry.dir)
	size := dirSize(entry.dir)
	if err := os.RemoveAll(entry.dir); err != nil {
		log.Printf("[worktree] evict %s/%s: %v", owner, repo, err)
		return 0
	}
	forgetBare(entry.dir)
	log.Printf("[worktree] evicted bare %s/%s (%d bytes, idle %s)",
		owner, repo, size, time.Since(entry.lastUsed).Round(time.Second))
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
			log.Printf("[worktree] evict: remove derived worktree %s: %v", wt, err)
		}
	}
}
