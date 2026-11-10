package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// liveRecord parses the chunk record stored as key's base entry.
func liveRecord(t *testing.T, key string) chunkRecord {
	t.Helper()
	base, err := rawGet(t, key)
	if err != nil {
		t.Fatalf("base %s: %v", key, err)
	}
	rec, err := parseChunkBase(base)
	if err != nil {
		t.Fatalf("base %s = %q: %v", key, base, err)
	}
	return rec
}

func assertAbsent(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if v, err := rawGet(t, n); err != keyring.ErrNotFound {
			t.Errorf("%s still present (%q, %v)", n, v, err)
		}
	}
}

func rawSet(t *testing.T, name, value string) {
	t.Helper()
	if err := keyring.Set(service, name, value); err != nil {
		t.Fatal(err)
	}
}

// writeV1 stores value in the layout earlier builds wrote: chunks "<key>#<i>"
// and a "tfchunks:v1:<N>:<digest>" base.
func writeV1(t *testing.T, key, value string) {
	t.Helper()
	n := (len(value) + chunkTestLimit - 1) / chunkTestLimit
	for i := 1; i <= n; i++ {
		rawSet(t, fmt.Sprintf("%s#%d", key, i), value[(i-1)*chunkTestLimit:min(i*chunkTestLimit, len(value))])
	}
	sum := sha256.Sum256([]byte(value))
	rawSet(t, key, fmt.Sprintf("tfchunks:v1:%d:%s", n, hex.EncodeToString(sum[:])))
}

// trackSets records every entry name written through kr.set.
func trackSets(t *testing.T) *[]string {
	t.Helper()
	var names []string
	prev := kr.set
	kr.set = func(svc, user, pw string) error {
		names = append(names, user)
		return prev(svc, user, pw)
	}
	t.Cleanup(func() { kr.set = prev })
	return &names
}

// failSetWhere makes kr.set fail for entries match accepts.
func failSetWhere(t *testing.T, match func(entry string) bool) {
	t.Helper()
	prev := kr.set
	kr.set = func(svc, user, pw string) error {
		if match(user) {
			return errors.New("injected write failure")
		}
		return prev(svc, user, pw)
	}
	t.Cleanup(func() { kr.set = prev })
}

// failDeleteWhere makes kr.delete fail for entries match accepts, until the
// returned func restores it.
func failDeleteWhere(t *testing.T, match func(entry string) bool) (restore func()) {
	t.Helper()
	prev := kr.delete
	kr.delete = func(svc, user string) error {
		if match(user) {
			return errors.New("injected delete failure")
		}
		return prev(svc, user)
	}
	restored := false
	restore = func() {
		if !restored {
			kr.delete = prev
			restored = true
		}
	}
	t.Cleanup(restore)
	return restore
}

func isChunkEntry(entry string) bool { return strings.Contains(entry, "#") }

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

func TestKeychainChunking_LargeValueUsesGenerationTaggedChunks(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(2*chunkTestLimit+1))
	rec := liveRecord(t, "k")
	if rec.n != 3 || !validChunkGen(rec.gen) {
		t.Fatalf("record = %+v; want a 3-chunk v2 record with a generation", rec)
	}
	for i := 1; i <= 3; i++ {
		if _, err := rawGet(t, fmt.Sprintf("k#%s#%d", rec.gen, i)); err != nil {
			t.Errorf("chunk %d: %v", i, err)
		}
	}
	assertAbsent(t, fmt.Sprintf("k#%s#4", rec.gen), "k#1", "k#orphans")
}

func TestKeychainChunking_EachWriteGetsANewGeneration(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(3*chunkTestLimit))
	first := liveRecord(t, "k")
	mustPut(t, "k", payload(3*chunkTestLimit)) // the same value again
	second := liveRecord(t, "k")
	if first.gen == second.gen {
		t.Fatalf("a rewrite reused generation %s", first.gen)
	}
	assertAbsent(t, fmt.Sprintf("k#%s#1", first.gen), fmt.Sprintf("k#%s#3", first.gen))
	if got := mustGet(t, "k"); got != payload(3*chunkTestLimit) {
		t.Error("value wrong after a rewrite")
	}
}

func TestKeychainChunking_ShrinkRemovesStaleChunks(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(4*chunkTestLimit))
	big := liveRecord(t, "k")

	mustPut(t, "k", payload(2*chunkTestLimit)) // chunked -> fewer chunks
	for i := 1; i <= 4; i++ {
		assertAbsent(t, fmt.Sprintf("k#%s#%d", big.gen, i))
	}
	if got := mustGet(t, "k"); got != payload(2*chunkTestLimit) {
		t.Error("value wrong after shrink")
	}

	small := liveRecord(t, "k")
	mustPut(t, "k", "tiny") // chunked -> unchunked
	assertAbsent(t, fmt.Sprintf("k#%s#1", small.gen), fmt.Sprintf("k#%s#2", small.gen), "k#orphans")
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
	rec := liveRecord(t, "k")
	rawSet(t, fmt.Sprintf("k#%s#2", rec.gen), "tampered-content")
	if _, err := (keychainBackend{}).get("k"); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("get over a corrupted chunk: err = %v, want a digest mismatch", err)
	}
}

func TestKeychainChunking_MissingChunkIsAnError(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(3*chunkTestLimit))
	rec := liveRecord(t, "k")
	if err := keyring.Delete(service, fmt.Sprintf("k#%s#3", rec.gen)); err != nil {
		t.Fatal(err)
	}
	if _, err := (keychainBackend{}).get("k"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("get with a missing chunk: err = %v, want a missing-chunk error", err)
	}
}

// A read that races a write in another process finds the generation it was
// reading gone; reading the base again lands on the new one.
func TestKeychainChunking_GetRetriesAfterAConcurrentWrite(t *testing.T) {
	withChunking(t)
	old, replacement := payload(3*chunkTestLimit), strings.ToUpper(payload(2*chunkTestLimit))
	mustPut(t, "k", old)
	oldRec := liveRecord(t, "k")

	prev := kr.get
	swapped := false
	kr.get = func(svc, user string) (string, error) {
		if !swapped && user == fmt.Sprintf("k#%s#2", oldRec.gen) {
			swapped = true
			// What another process's put does: new generation, base, then the
			// old generation removed.
			const gen = "0badc0de"
			rawSet(t, "k#"+gen+"#1", replacement[:chunkTestLimit])
			rawSet(t, "k#"+gen+"#2", replacement[chunkTestLimit:])
			sum := sha256.Sum256([]byte(replacement))
			rawSet(t, "k", fmt.Sprintf("%s%s:2:%s", chunkBasePrefix, gen, hex.EncodeToString(sum[:])))
			for i := 1; i <= 3; i++ {
				_ = keyring.Delete(service, fmt.Sprintf("k#%s#%d", oldRec.gen, i))
			}
		}
		return prev(svc, user)
	}
	t.Cleanup(func() { kr.get = prev })

	if got, err := (keychainBackend{}).get("k"); err != nil || got != replacement {
		t.Fatalf("get = %q, %v; want the replacement value", got, err)
	}
	if !swapped {
		t.Fatal("the concurrent write never happened")
	}
}

func TestKeychainChunking_MalformedBaseIsAnError(t *testing.T) {
	withChunking(t)
	for _, base := range []string{
		chunkBasePrefix + "nonsense",
		chunkBasePrefix + "zzzzzzzz:2:abc", // not a generation
		chunkBasePrefixV1 + "nonsense",
	} {
		rawSet(t, "k", base)
		if _, err := (keychainBackend{}).get("k"); err == nil {
			t.Errorf("get over malformed record %q succeeded", base)
		}
	}
}

func TestKeychainChunking_ChunkCountIsCapped(t *testing.T) {
	withChunking(t)
	for _, base := range []string{
		fmt.Sprintf("%s0badc0de:%d:abc", chunkBasePrefix, maxChunks+1),
		fmt.Sprintf("%s%d:abc", chunkBasePrefixV1, maxChunks+1),
	} {
		rawSet(t, "k", base)
		_, err := (keychainBackend{}).get("k")
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Errorf("get over %q: err = %v, want a chunk-count error", base, err)
		}
	}
	// The cap itself parses.
	if _, err := parseChunkBase(fmt.Sprintf("%s0badc0de:%d:abc", chunkBasePrefix, maxChunks)); err != nil {
		t.Errorf("a record at the cap was refused: %v", err)
	}

	// A value that would need more chunks than the cap is refused before
	// anything is written.
	sets := trackSets(t)
	if err := (keychainBackend{}).put("big", payload((maxChunks+1)*chunkTestLimit)); err == nil {
		t.Fatal("put of a value past the chunk cap succeeded")
	}
	if len(*sets) != 0 {
		t.Errorf("a refused put wrote %v", *sets)
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
	rec := liveRecord(t, "k")
	if err := (keychainBackend{}).delete("k"); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, "k", fmt.Sprintf("k#%s#1", rec.gen), fmt.Sprintf("k#%s#2", rec.gen), fmt.Sprintf("k#%s#3", rec.gen), "k#orphans")
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
		rawSet(t, k, "orphan")
	}
	if err := (keychainBackend{}).delete("k"); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, "k#1", "k#2")
}

// The orphan probe runs to the cap rather than stopping at the first gap.
func TestKeychainChunking_DeleteProbesPastAGap(t *testing.T) {
	withChunking(t)
	for _, k := range []string{"k#1", "k#4", fmt.Sprintf("k#%d", maxChunks)} {
		rawSet(t, k, "orphan")
	}
	if err := (keychainBackend{}).delete("k"); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, "k#1", "k#4", fmt.Sprintf("k#%d", maxChunks))
}

// A delete that cannot remove the base must not leave a base whose chunks are
// gone: nothing else is touched and the value still reads.
func TestKeychainChunking_FailedBaseDeleteLeavesTheValueReadable(t *testing.T) {
	withChunking(t)
	v := payload(3 * chunkTestLimit)
	mustPut(t, "k", v)
	restore := failDeleteWhere(t, func(e string) bool { return e == "k" })

	if err := (keychainBackend{}).delete("k"); err == nil || !strings.Contains(err.Error(), "injected delete failure") {
		t.Fatalf("delete err = %v, want the injected failure", err)
	}
	restore()
	if got := mustGet(t, "k"); got != v {
		t.Errorf("value after a failed delete = %q, want it intact", got)
	}
}

// With the base gone and a chunk undeletable, the delete reports it, and the
// next delete (or the uninstall sweep) finishes the job.
func TestKeychainChunking_FailedChunkDeleteIsFinishedLater(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(3*chunkTestLimit))
	rec := liveRecord(t, "k")
	stuck := fmt.Sprintf("k#%s#2", rec.gen)
	restore := failDeleteWhere(t, func(e string) bool { return e == stuck })

	if err := (keychainBackend{}).delete("k"); err == nil {
		t.Fatal("delete succeeded with a chunk that could not be removed")
	}
	if got := mustGet(t, "k"); got != "" {
		t.Errorf("a value with no base reads %q, want absent", got)
	}
	restore()
	if err := SweepKeychain([]string{"k"}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	assertAbsent(t, stuck, "k#orphans")
}

func TestKeychainChunking_SweepRemovesChunksAndOrphans(t *testing.T) {
	withChunking(t)
	mustPut(t, "swept", payload(3*chunkTestLimit))
	rec := liveRecord(t, "swept")
	for _, k := range []string{"orphaned#1", "orphaned#2"} {
		rawSet(t, k, "orphan")
	}
	if err := SweepKeychain([]string{"swept", "orphaned", "absent"}); err != nil {
		t.Fatalf("SweepKeychain: %v", err)
	}
	assertAbsent(t, "swept", fmt.Sprintf("swept#%s#1", rec.gen), fmt.Sprintf("swept#%s#3", rec.gen), "orphaned#1", "orphaned#2")
}

// The sweep removes a generation that only the orphan record names, and v1
// orphans past a gap.
func TestKeychainChunking_SweepRemovesEveryGeneration(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(3*chunkTestLimit))
	live := liveRecord(t, "k")
	rawSet(t, "k#cafef00d#1", "orphan")
	rawSet(t, "k#cafef00d#9", "orphan") // past a gap
	rawSet(t, orphanRecordName("k"), "cafef00d")
	rawSet(t, "k#2", "legacy orphan")
	rawSet(t, "k#7", "legacy orphan past a gap")

	if err := SweepKeychain([]string{"k"}); err != nil {
		t.Fatalf("SweepKeychain: %v", err)
	}
	assertAbsent(t, "k", fmt.Sprintf("k#%s#1", live.gen), "k#cafef00d#1", "k#cafef00d#9", "k#orphans", "k#2", "k#7")
}

func TestKeychainChunking_SmallPutClearsOrphanChunks(t *testing.T) {
	withChunking(t)
	rawSet(t, "k#1", "orphan")
	mustPut(t, "k", "tiny")
	assertAbsent(t, "k#1")
}

// The value is stored by the time housekeeping runs, so a chunk that cannot be
// removed is a warning, not a failed put.
func TestKeychainChunking_SmallPutSucceedsWhenCleanupFails(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(3*chunkTestLimit))
	rec := liveRecord(t, "k")
	stuck := fmt.Sprintf("k#%s#1", rec.gen)
	restore := failDeleteWhere(t, func(e string) bool { return e == stuck })

	if err := (keychainBackend{}).put("k", "tiny"); err != nil {
		t.Fatalf("put with failing cleanup = %v, want success", err)
	}
	if got := mustGet(t, "k"); got != "tiny" {
		t.Errorf("got %q, want tiny", got)
	}
	restore()
	if err := (keychainBackend{}).delete("k"); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, stuck, "k#orphans")
}

func TestKeychainChunking_ChunkedPutSucceedsWhenCleanupFails(t *testing.T) {
	withChunking(t)
	mustPut(t, "k", payload(3*chunkTestLimit))
	old := liveRecord(t, "k")
	oldChunk := fmt.Sprintf("k#%s#3", old.gen)
	restore := failDeleteWhere(t, func(e string) bool { return e == oldChunk })

	next := strings.ToUpper(payload(2 * chunkTestLimit))
	if err := (keychainBackend{}).put("k", next); err != nil {
		t.Fatalf("put with failing cleanup = %v, want success", err)
	}
	if got := mustGet(t, "k"); got != next {
		t.Errorf("got %q, want the new value", got)
	}
	// The generation that could not be removed is named for a later attempt.
	if raw, err := rawGet(t, orphanRecordName("k")); err != nil || !strings.Contains(raw, old.gen) {
		t.Errorf("orphan record = %q, %v; want it to name %s", raw, err, old.gen)
	}
	restore()
	mustPut(t, "k", payload(2*chunkTestLimit)) // the next write retries
	assertAbsent(t, oldChunk, orphanRecordName("k"))
}

// A write that fails part way removes only its own chunks: the previous value
// keeps its base and its chunks and reads back.
func TestKeychainChunking_FailedPutPreservesThePreviousValue(t *testing.T) {
	old := payload(3 * chunkTestLimit)
	for name, fail := range map[string]func(entry string) bool{
		"a chunk write":  func(e string) bool { return strings.HasSuffix(e, "#3") && strings.Count(e, "#") == 2 },
		"the base write": func(e string) bool { return e == "k" },
	} {
		t.Run(name, func(t *testing.T) {
			withChunking(t)
			mustPut(t, "k", old)
			before := liveRecord(t, "k")
			sets := trackSets(t)
			failSetWhere(t, fail)

			err := (keychainBackend{}).put("k", strings.ToUpper(payload(4*chunkTestLimit)))
			if err == nil || !strings.Contains(err.Error(), "injected write failure") {
				t.Fatalf("put err = %v, want the injected failure", err)
			}
			if got := mustGet(t, "k"); got != old {
				t.Errorf("previous value after a failed put = %q, want it intact", got)
			}
			if after := liveRecord(t, "k"); after != before {
				t.Errorf("base changed from %+v to %+v", before, after)
			}
			for _, n := range *sets {
				if strings.HasPrefix(n, "k#") && !strings.HasPrefix(n, "k#"+before.gen) && n != orphanRecordName("k") {
					assertAbsent(t, n)
				}
			}
			assertAbsent(t, orphanRecordName("k"))
		})
	}
}

// A write that fails and cannot clean up after itself leaves the new
// generation named, and the sweep removes it.
func TestKeychainChunking_FailedPutWithFailedCleanupIsSweepable(t *testing.T) {
	withChunking(t)
	sets := trackSets(t)
	failSetWhere(t, func(e string) bool { return strings.HasSuffix(e, "#3") })
	restore := failDeleteWhere(t, isChunkEntry)

	if err := (keychainBackend{}).put("k", payload(4*chunkTestLimit)); err == nil {
		t.Fatal("put succeeded despite the injected failure")
	}
	restore()
	if raw, err := rawGet(t, orphanRecordName("k")); err != nil || !validChunkGen(raw) {
		t.Fatalf("orphan record = %q, %v; want the abandoned generation", raw, err)
	}
	if err := SweepKeychain([]string{"k"}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, n := range *sets {
		assertAbsent(t, n)
	}
}

// A value written by an earlier build still reads, and the next write replaces
// it with the generation-tagged layout and removes the old chunks.
func TestKeychainChunking_V1RecordReadsAndIsReplacedByV2(t *testing.T) {
	withChunking(t)
	v1 := payload(3*chunkTestLimit + 4)
	writeV1(t, "k", v1)
	if got := mustGet(t, "k"); got != v1 {
		t.Fatalf("v1 value read as %q", got)
	}

	next := strings.ToUpper(payload(2*chunkTestLimit + 1))
	mustPut(t, "k", next)
	if rec := liveRecord(t, "k"); rec.gen == "" || rec.n != 3 {
		t.Errorf("record after the write = %+v, want a v2 record", rec)
	}
	assertAbsent(t, "k#1", "k#2", "k#3", "k#4")
	if got := mustGet(t, "k"); got != next {
		t.Errorf("got %q, want the new value", got)
	}

	// A small write replaces a v1 value too.
	writeV1(t, "j", v1)
	mustPut(t, "j", "tiny")
	assertAbsent(t, "j#1", "j#2", "j#3", "j#4")
	if got := mustGet(t, "j"); got != "tiny" {
		t.Errorf("got %q, want tiny", got)
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
	mustPut(t, "k", chunkBasePrefix+"0badc0de:2:abc")
	if got := mustGet(t, "k"); got != chunkBasePrefix+"0badc0de:2:abc" {
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
