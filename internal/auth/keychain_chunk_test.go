package auth

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// chunkTestLimit is the chunk size these tests run with on every OS; the real
// Windows limit (2560) only changes the numbers, not the logic.
const chunkTestLimit = 16

// withChunking installs the in-memory keyring and a small chunk limit for t.
// The real keychain is never touched.
func withChunking(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	prev := chunkLimit
	chunkLimit = chunkTestLimit
	t.Cleanup(func() { chunkLimit = prev })
}

func payload(n int) string {
	var sb strings.Builder
	for i := 0; sb.Len() < n; i++ {
		sb.WriteByte(byte('a' + i%26))
	}
	return sb.String()[:n]
}

func rawGet(t *testing.T, key string) (string, error) {
	t.Helper()
	return keyring.Get(service, key)
}

func mustPut(t *testing.T, key, value string) {
	t.Helper()
	if err := (keychainBackend{}).put(key, value); err != nil {
		t.Fatalf("put(%q, %d bytes): %v", key, len(value), err)
	}
}

func mustGet(t *testing.T, key string) string {
	t.Helper()
	v, err := (keychainBackend{}).get(key)
	if err != nil {
		t.Fatalf("get(%q): %v", key, err)
	}
	return v
}

func TestKeychainChunking_RoundTrip(t *testing.T) {
	withChunking(t)
	for _, n := range []int{0, 1, chunkTestLimit - 1, chunkTestLimit, chunkTestLimit + 1, 3 * chunkTestLimit, 3*chunkTestLimit + 5} {
		v := payload(n)
		mustPut(t, "k", v)
		if got := mustGet(t, "k"); got != v {
			t.Errorf("len %d: round trip mismatch", n)
		}
	}
}

func TestKeychainChunking_SmallValueStoredExactly(t *testing.T) {
	withChunking(t)
	for _, n := range []int{1, chunkTestLimit} {
		v := payload(n)
		mustPut(t, "k", v)
		raw, err := rawGet(t, "k")
		if err != nil || raw != v {
			t.Fatalf("len %d: raw = %q, %v; want the value itself", n, raw, err)
		}
		if _, err := rawGet(t, "k#1"); err != keyring.ErrNotFound {
			t.Fatalf("len %d: chunk entry exists for an unchunked value: %v", n, err)
		}
	}
}

func TestKeychainChunking_LargeValueUsesChunks(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(2*chunkTestLimit+1))
	base, err := rawGet(t, "k")
	if err != nil || !strings.HasPrefix(base, chunkBasePrefix+"3:") {
		t.Fatalf("base = %q, %v; want a 3-chunk record", base, err)
	}
	for _, k := range []string{"k#1", "k#2", "k#3"} {
		if _, err := rawGet(t, k); err != nil {
			t.Errorf("chunk %s: %v", k, err)
		}
	}
	if _, err := rawGet(t, "k#4"); err != keyring.ErrNotFound {
		t.Errorf("k#4 should not exist: %v", err)
	}
}

func TestKeychainChunking_ShrinkRemovesStaleChunks(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(4*chunkTestLimit))

	mustPut(t, "k", payload(2*chunkTestLimit)) // chunked -> fewer chunks
	if _, err := rawGet(t, "k#3"); err != keyring.ErrNotFound {
		t.Errorf("k#3 survived a shrink to 2 chunks: %v", err)
	}
	if got := mustGet(t, "k"); got != payload(2*chunkTestLimit) {
		t.Error("value wrong after shrink")
	}

	mustPut(t, "k", "tiny") // chunked -> unchunked
	for _, k := range []string{"k#1", "k#2"} {
		if _, err := rawGet(t, k); err != keyring.ErrNotFound {
			t.Errorf("%s survived a shrink to an unchunked value: %v", k, err)
		}
	}
	if got := mustGet(t, "k"); got != "tiny" {
		t.Errorf("got %q, want tiny", got)
	}
}

func TestKeychainChunking_Grow(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", "tiny")
	mustPut(t, "k", payload(5*chunkTestLimit+3))
	if got := mustGet(t, "k"); got != payload(5*chunkTestLimit+3) {
		t.Error("value wrong after growing from unchunked to chunked")
	}
}

func TestKeychainChunking_CorruptedChunkIsAnError(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(3*chunkTestLimit))
	if err := keyring.Set(service, "k#2", "tampered-content"); err != nil {
		t.Fatal(err)
	}
	if _, err := (keychainBackend{}).get("k"); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("get over a corrupted chunk: err = %v, want a digest mismatch", err)
	}
}

func TestKeychainChunking_MissingChunkIsAnError(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(3*chunkTestLimit))
	if err := keyring.Delete(service, "k#3"); err != nil {
		t.Fatal(err)
	}
	if _, err := (keychainBackend{}).get("k"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("get with a missing chunk: err = %v, want a missing-chunk error", err)
	}
}

func TestKeychainChunking_MalformedBaseIsAnError(t *testing.T) {
	withChunking(t)
	if err := keyring.Set(service, "k", chunkBasePrefix+"nonsense"); err != nil {
		t.Fatal(err)
	}
	if _, err := (keychainBackend{}).get("k"); err == nil {
		t.Fatal("get over a malformed chunk record succeeded")
	}
}

func TestKeychainChunking_AbsentKeyReadsEmpty(t *testing.T) {
	withChunking(t)
	if got := mustGet(t, "never"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestKeychainChunking_DeleteRemovesBaseAndChunks(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(3*chunkTestLimit))
	mustPut(t, "other", payload(2*chunkTestLimit)) // must be untouched
	if err := (keychainBackend{}).delete("k"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"k", "k#1", "k#2", "k#3"} {
		if _, err := rawGet(t, k); err != keyring.ErrNotFound {
			t.Errorf("%s survived delete: %v", k, err)
		}
	}
	if got := mustGet(t, "other"); got != payload(2*chunkTestLimit) {
		t.Error("delete of one key damaged another")
	}
	if err := (keychainBackend{}).delete("k"); err != nil {
		t.Errorf("deleting an absent key: %v", err)
	}
}

func TestKeychainChunking_DeleteRemovesOrphanChunksWithoutBase(t *testing.T) {
	withChunking(t)
	for _, k := range []string{"k#1", "k#2"} {
		if err := keyring.Set(service, k, "orphan"); err != nil {
			t.Fatal(err)
		}
	}
	if err := (keychainBackend{}).delete("k"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"k#1", "k#2"} {
		if _, err := rawGet(t, k); err != keyring.ErrNotFound {
			t.Errorf("orphan %s survived delete: %v", k, err)
		}
	}
}

func TestKeychainChunking_SweepRemovesChunksAndOrphans(t *testing.T) {
	withChunking(t)
	mustPut(t, "swept", payload(3*chunkTestLimit))
	for _, k := range []string{"orphaned#1", "orphaned#2"} {
		if err := keyring.Set(service, k, "orphan"); err != nil {
			t.Fatal(err)
		}
	}
	if err := SweepKeychain([]string{"swept", "orphaned", "absent"}); err != nil {
		t.Fatalf("SweepKeychain: %v", err)
	}
	for _, k := range []string{"swept", "swept#1", "swept#3", "orphaned#1", "orphaned#2"} {
		if _, err := rawGet(t, k); err != keyring.ErrNotFound {
			t.Errorf("%s survived the sweep: %v", k, err)
		}
	}
}

func TestKeychainChunking_SmallPutClearsOrphanChunks(t *testing.T) {
	withChunking(t)
	if err := keyring.Set(service, "k#1", "orphan"); err != nil {
		t.Fatal(err)
	}
	mustPut(t, "k", "tiny")
	if _, err := rawGet(t, "k#1"); err != keyring.ErrNotFound {
		t.Errorf("orphan chunk survived a small put: %v", err)
	}
}

// failSetOn makes kr.set fail for the named entry and restores it on cleanup.
func failSetOn(t *testing.T, entry string) {
	t.Helper()
	prev := kr.set
	kr.set = func(svc, user, pw string) error {
		if user == entry {
			return errors.New("injected write failure")
		}
		return prev(svc, user, pw)
	}
	t.Cleanup(func() { kr.set = prev })
}

func TestKeychainChunking_FailedPutRemovesWrittenChunks(t *testing.T) {
	for _, failAt := range []string{"k#3", "k"} { // a chunk write, then the base write
		t.Run(failAt, func(t *testing.T) {
			withChunking(t)
			failSetOn(t, failAt)
			err := (keychainBackend{}).put("k", payload(4*chunkTestLimit))
			if err == nil || !strings.Contains(err.Error(), "injected write failure") {
				t.Fatalf("put err = %v, want the injected failure", err)
			}
			for _, k := range []string{"k", "k#1", "k#2", "k#3", "k#4"} {
				if _, err := rawGet(t, k); err != keyring.ErrNotFound {
					t.Errorf("%s left behind by a failed put: %v", k, err)
				}
			}
		})
	}
}

func TestKeychainChunking_FailedReplaceDoesNotLeaveAStaleBase(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(3*chunkTestLimit))
	failSetOn(t, "k#2")
	if err := (keychainBackend{}).put("k", payload(4*chunkTestLimit)); err == nil {
		t.Fatal("put succeeded despite the injected failure")
	}
	// The old chunks were partly overwritten, so a base pointing at them would
	// read back garbage; absent is the honest state.
	if got, err := (keychainBackend{}).get("k"); err != nil || got != "" {
		t.Errorf("get = %q, %v; want absent", got, err)
	}
}

func TestKeychainChunking_DisabledIsPlainPassthrough(t *testing.T) {
	keyring.MockInit()
	prev := chunkLimit
	chunkLimit = 0
	t.Cleanup(func() { chunkLimit = prev })

	v := payload(10_000)
	mustPut(t, "k", v)
	raw, err := rawGet(t, "k")
	if err != nil || raw != v {
		t.Fatalf("with chunking disabled the value must be stored as-is (err %v)", err)
	}
	// A value that merely looks like a chunk record is returned untouched.
	mustPut(t, "k", chunkBasePrefix+"2:abc")
	if got := mustGet(t, "k"); got != chunkBasePrefix+"2:abc" {
		t.Errorf("got %q", got)
	}
}

func TestPlatformChunkLimit(t *testing.T) {
	want := 0
	if runtime.GOOS == "windows" {
		want = 2560
	}
	if platformChunkLimit != want {
		t.Errorf("platformChunkLimit = %d, want %d", platformChunkLimit, want)
	}
}

func TestRenameReplace_RetriesTransientFailures(t *testing.T) {
	prevRename, prevTransient := renameFile, renameTransient
	t.Cleanup(func() { renameFile, renameTransient = prevRename, prevTransient })
	renameTransient = func(error) bool { return true }

	calls := 0
	renameFile = func(_, _ string) error {
		calls++
		if calls < 3 {
			return errors.New("sharing violation")
		}
		return nil
	}
	if err := renameReplace("a", "b"); err != nil || calls != 3 {
		t.Fatalf("err = %v after %d calls; want success on the third", err, calls)
	}

	calls = 0
	renameFile = func(_, _ string) error { calls++; return errors.New("still busy") }
	if err := renameReplace("a", "b"); err == nil || calls != renameAttempts {
		t.Fatalf("err = %v after %d calls; want failure after %d", err, calls, renameAttempts)
	}
}

func TestRenameReplace_DoesNotRetryOtherFailures(t *testing.T) {
	prevRename, prevTransient := renameFile, renameTransient
	t.Cleanup(func() { renameFile, renameTransient = prevRename, prevTransient })
	renameTransient = func(error) bool { return false }

	calls := 0
	renameFile = func(_, _ string) error { calls++; return errors.New("permanent") }
	if err := renameReplace("a", "b"); err == nil || calls != 1 {
		t.Fatalf("err = %v after %d calls; want one call", err, calls)
	}
}
