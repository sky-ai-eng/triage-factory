package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// setBareLastUsed backdates a bare's LRU stamp so the reaper's
// coldest-first / TTL logic is deterministic under test. Whitebox access
// to the unexported accounting map.
func setBareLastUsed(dir string, t time.Time) {
	lastUsedMu.Lock()
	lastUsed[dir] = t
	lastUsedMu.Unlock()
}

func seedBare(t *testing.T, owner, repo string) string {
	t.Helper()
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), owner, repo, upstream); err != nil {
		t.Fatalf("seed bare %s/%s: %v", owner, repo, err)
	}
	dir, err := repoDir(owner, repo)
	if err != nil {
		t.Fatalf("repoDir: %v", err)
	}
	return dir
}

// TestDefaultPolicy_ModeDerived pins the one-mechanism/policy-per-mode
// contract: local is unbounded (reaper off), multi carries a real budget +
// TTL that the env knobs override.
func TestDefaultPolicy_ModeDerived(t *testing.T) {
	t.Run("local is unbounded", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeLocal)
		if p := DefaultPolicy(); !p.unbounded() {
			t.Errorf("local DefaultPolicy = %+v, want unbounded", p)
		}
	})

	t.Run("multi has a budget and ttl", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeMulti)
		p := DefaultPolicy()
		if p.unbounded() {
			t.Fatalf("multi DefaultPolicy = %+v, want bounded", p)
		}
		if p.MaxBytes != defaultMultiBudgetBytes || p.TTL != defaultMultiTTL {
			t.Errorf("multi defaults = %+v, want {%d, %s}", p, defaultMultiBudgetBytes, defaultMultiTTL)
		}
	})

	t.Run("env overrides multi defaults", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeMulti)
		t.Setenv(envCacheMaxBytes, "12345")
		t.Setenv(envCacheTTL, "1h")
		p := DefaultPolicy()
		if p.MaxBytes != 12345 {
			t.Errorf("MaxBytes = %d, want 12345", p.MaxBytes)
		}
		if p.TTL != time.Hour {
			t.Errorf("TTL = %s, want 1h", p.TTL)
		}
	})
}

// TestEnforceBudget_NoopUnbounded is the local-mode policy knob: under the
// unbounded zero Policy the reaper never fires even with bares present.
func TestEnforceBudget_NoopUnbounded(t *testing.T) {
	withTestHome(t)
	dir := seedBare(t, "o", "cold")
	// Backdate hard — would be evicted under any finite TTL.
	setBareLastUsed(dir, time.Now().Add(-365*24*time.Hour))

	evicted, reclaimed := EnforceBudget(context.Background(), Policy{})
	if evicted != 0 || reclaimed != 0 {
		t.Errorf("unbounded EnforceBudget evicted=%d reclaimed=%d, want 0/0", evicted, reclaimed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("bare reclaimed under unbounded policy: %v", err)
	}
}

// TestEnforceBudget_EvictsColdestOverBudget drives the disk-budget pass:
// over budget, the coldest bare is reclaimed (and re-seedable), the warmer
// one survives. MaxBytes = total-1 forces exactly one eviction regardless
// of the concrete on-disk sizes.
func TestEnforceBudget_EvictsColdestOverBudget(t *testing.T) {
	withTestHome(t)
	cold := seedBare(t, "o", "cold")
	warm := seedBare(t, "o", "warm")
	setBareLastUsed(cold, time.Now().Add(-2*time.Hour))
	setBareLastUsed(warm, time.Now())

	var total int64
	for _, e := range scanBares() {
		total += e.sizeB
	}

	evicted, reclaimed := EnforceBudget(context.Background(), Policy{MaxBytes: total - 1})
	if evicted != 1 {
		t.Fatalf("evicted=%d, want exactly 1 (coldest)", evicted)
	}
	if reclaimed <= 0 {
		t.Errorf("reclaimed=%d, want > 0", reclaimed)
	}
	if _, err := os.Stat(cold); !os.IsNotExist(err) {
		t.Errorf("coldest bare survived eviction: stat err=%v", err)
	}
	if _, err := os.Stat(warm); err != nil {
		t.Errorf("warm bare wrongly evicted: %v", err)
	}

	// Re-seed is transparent: EnsureBareClone rebuilds the evicted bare.
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "o", "cold", upstream); err != nil {
		t.Errorf("re-seed after eviction: %v", err)
	}
	if _, err := os.Stat(cold); err != nil {
		t.Errorf("bare not re-seeded: %v", err)
	}
}

// TestEnforceBudget_TTLEviction drives the TTL pass independent of the byte
// budget: a bare idle past the TTL is reclaimed even though MaxBytes is 0
// (budget pass off).
func TestEnforceBudget_TTLEviction(t *testing.T) {
	withTestHome(t)
	stale := seedBare(t, "o", "stale")
	fresh := seedBare(t, "o", "fresh")
	setBareLastUsed(stale, time.Now().Add(-48*time.Hour))
	setBareLastUsed(fresh, time.Now())

	evicted, _ := EnforceBudget(context.Background(), Policy{TTL: 24 * time.Hour})
	if evicted != 1 {
		t.Fatalf("evicted=%d, want 1 (the stale bare)", evicted)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale bare survived TTL eviction: stat err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh bare wrongly TTL-evicted: %v", err)
	}
}

// TestEnforceBudget_SkipsInUseBare is the safe-eviction guard: a bare with
// a live worktree (here a curator worktree with an on-disk checkout) is
// never reclaimed, even when it's the coldest and over budget. Once the
// checkout is gone, the same sweep reclaims it.
func TestEnforceBudget_SkipsInUseBare(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "o", "live", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}
	bareDir, _ := repoDir("o", "live")

	projectDir := filepath.Join(t.TempDir(), "proj")
	wt, err := EnsureCuratorWorktree(context.Background(), "o", "live", "main", projectDir)
	if err != nil {
		t.Fatalf("curator worktree: %v", err)
	}
	// Make it the coldest by far so only the in-use guard can save it.
	setBareLastUsed(bareDir, time.Now().Add(-365*24*time.Hour))

	evicted, _ := EnforceBudget(context.Background(), Policy{MaxBytes: 1, TTL: time.Hour})
	if evicted != 0 {
		t.Fatalf("evicted=%d, want 0 — in-use bare must survive", evicted)
	}
	if _, err := os.Stat(bareDir); err != nil {
		t.Errorf("in-use bare was reclaimed: %v", err)
	}

	// Drop the live checkout → the bare is now cold and reclaimable.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	if evicted, _ := EnforceBudget(context.Background(), Policy{MaxBytes: 1}); evicted != 1 {
		t.Fatalf("after checkout removed: evicted=%d, want 1", evicted)
	}
	if _, err := os.Stat(bareDir); !os.IsNotExist(err) {
		t.Errorf("cold bare not reclaimed after checkout gone: stat err=%v", err)
	}
}

// TestOwnerRepoFromBareDir pins the path → (owner, repo) recovery the
// reaper uses to take the per-repo lock before evicting.
func TestOwnerRepoFromBareDir(t *testing.T) {
	owner, repo, ok := ownerRepoFromBareDir("/data/repos/sky-ai-eng/triage-factory.git")
	if !ok || owner != "sky-ai-eng" || repo != "triage-factory" {
		t.Errorf("got (%q, %q, %v), want (sky-ai-eng, triage-factory, true)", owner, repo, ok)
	}
	if _, _, ok := ownerRepoFromBareDir("/data/repos/not-a-bare"); ok {
		t.Error("non-.git dir parsed as a bare")
	}
}
