package worktree

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetPreflightCacheForTest wipes the package-level cache and
// preflight-impl override on entry, restoring whatever was there
// before in t.Cleanup. Tests must NOT use t.Parallel() — the cache
// and preflightImpl are package-global by design (one process, one
// SSH config), so parallel writes would race on each other rather
// than on production code.
func resetPreflightCacheForTest(t *testing.T) {
	t.Helper()

	sshPreflightCache.mu.Lock()
	prevEntries := sshPreflightCache.entries
	sshPreflightCache.entries = make(map[string]sshPreflightEntry)
	sshPreflightCache.mu.Unlock()

	prevImpl := preflightImpl
	prevTTL := sshPreflightFailureTTL

	t.Cleanup(func() {
		sshPreflightCache.mu.Lock()
		sshPreflightCache.entries = prevEntries
		sshPreflightCache.mu.Unlock()
		preflightImpl = prevImpl
		sshPreflightFailureTTL = prevTTL
	})
}

func TestCachedPreflightSSH_SuccessCachedForLifetime(t *testing.T) {
	resetPreflightCacheForTest(t)
	var calls atomic.Int32
	preflightImpl = func(_ context.Context, _ string) error {
		calls.Add(1)
		return nil
	}

	for range 5 {
		if err := CachedPreflightSSH(context.Background(), "git@example.com"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("probe ran %d times, want 1 (success should cache for process lifetime)", got)
	}
}

func TestCachedPreflightSSH_FailureCachedUntilTTLExpires(t *testing.T) {
	resetPreflightCacheForTest(t)
	sshPreflightFailureTTL = 50 * time.Millisecond

	var calls atomic.Int32
	sentinel := errors.New("synthesized preflight failure")
	preflightImpl = func(_ context.Context, _ string) error {
		calls.Add(1)
		return sentinel
	}

	// First call probes and caches.
	if err := CachedPreflightSSH(context.Background(), "git@example.com"); !errors.Is(err, sentinel) {
		t.Fatalf("first call: %v, want sentinel", err)
	}
	// Second call within the TTL window must hit the cache.
	if err := CachedPreflightSSH(context.Background(), "git@example.com"); !errors.Is(err, sentinel) {
		t.Fatalf("second call: %v, want sentinel", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("probe ran %d times before TTL expiry, want 1", got)
	}

	// Wait past the TTL and confirm the next call probes again — this
	// is the "user fixed their SSH setup, should re-detect without
	// restart" path that the failure TTL exists to enable.
	time.Sleep(sshPreflightFailureTTL + 25*time.Millisecond)
	if err := CachedPreflightSSH(context.Background(), "git@example.com"); !errors.Is(err, sentinel) {
		t.Fatalf("post-TTL call: %v, want sentinel", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("probe ran %d times total, want 2 (one before TTL, one after)", got)
	}
}

func TestCachedPreflightSSH_DedupesConcurrentCallers(t *testing.T) {
	resetPreflightCacheForTest(t)

	var calls atomic.Int32
	preflightImpl = func(_ context.Context, _ string) error {
		calls.Add(1)
		// Sleep long enough that all goroutines launched below are
		// blocked on the cache mutex when this returns. Without this,
		// the test could pass even if dedup were broken because
		// goroutines might serialize naturally.
		time.Sleep(50 * time.Millisecond)
		return nil
	}

	const goroutines = 20
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := CachedPreflightSSH(context.Background(), "git@example.com"); err != nil {
				t.Errorf("unexpected error from goroutine: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("probe ran %d times, want 1 (concurrent callers must dedupe)", got)
	}
}

func TestCachedPreflightSSH_PerHostKeying(t *testing.T) {
	resetPreflightCacheForTest(t)

	var calls atomic.Int32
	preflightImpl = func(_ context.Context, _ string) error {
		calls.Add(1)
		return nil
	}

	if err := CachedPreflightSSH(context.Background(), "git@host-a"); err != nil {
		t.Fatalf("host-a: %v", err)
	}
	if err := CachedPreflightSSH(context.Background(), "git@host-b"); err != nil {
		t.Fatalf("host-b: %v", err)
	}
	// Repeats of each host should hit the cache.
	for range 3 {
		_ = CachedPreflightSSH(context.Background(), "git@host-a")
		_ = CachedPreflightSSH(context.Background(), "git@host-b")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("probe ran %d times, want 2 (one per distinct host)", got)
	}
}

func TestCachedPreflightSSH_RefusesAnEmptyHost(t *testing.T) {
	resetPreflightCacheForTest(t)

	probed := false
	preflightImpl = func(context.Context, string) error {
		probed = true
		return nil
	}

	if err := CachedPreflightSSH(context.Background(), ""); err == nil {
		t.Fatal("CachedPreflightSSH(\"\") = nil, want a refusal: there is no host to probe")
	}
	if probed {
		t.Error("an empty host was probed; it names no GitHub")
	}
}

func TestSSHHostFromBaseURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"github.com", "https://github.com", "git@github.com"},
		{"github.com with trailing slash", "https://github.com/", "git@github.com"},
		{"GHE host", "https://github.example.com", "git@github.example.com"},
		{"GHE with subdir path", "https://corp.example.com/github", "git@corp.example.com"},
		{"empty has no host", "", ""},
		{"unparseable has no host", "://bad", ""},
		{"no scheme has no host", "github.com", ""},
		{"http (not https)", "http://github.example.com", "git@github.example.com"},
		{"with port", "https://github.example.com:8443", "git@github.example.com"},
		{"with userinfo", "https://user:pass@github.example.com", "git@github.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SSHHostFromBaseURL(tc.in)
			if got != tc.want {
				t.Errorf("SSHHostFromBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPreflightEntry_Validity(t *testing.T) {
	resetPreflightCacheForTest(t)
	sshPreflightFailureTTL = 100 * time.Millisecond

	// Success entries are always valid.
	ok := sshPreflightEntry{err: nil, cachedAt: time.Now().Add(-time.Hour)}
	if !ok.valid() {
		t.Errorf("success entry from an hour ago should be valid")
	}

	// Failure entries expire at the TTL.
	fresh := sshPreflightEntry{err: errors.New("x"), cachedAt: time.Now()}
	if !fresh.valid() {
		t.Errorf("fresh failure entry should be valid")
	}
	stale := sshPreflightEntry{err: errors.New("x"), cachedAt: time.Now().Add(-time.Second)}
	if stale.valid() {
		t.Errorf("failure entry older than TTL should be invalid")
	}
}
