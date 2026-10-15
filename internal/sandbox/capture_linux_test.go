//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureTestJail is the jail identity the capture tests' trees are handed
// to, and so the identity the capture jail runs as.
var captureTestJail = struct{ uid, gid int }{JailUID(9), JailGID(9)}

// captureTestTree returns a directory that passes validateRunTreeRoot and is
// held by a jail identity, as every tree a capture is asked for is: t.TempDir()
// is absolute, clean, ≥2 components deep and symlink-free on Linux, and it is
// chowned to captureTestJail. Handing it over needs root, so a test that needs
// one skips without it.
func captureTestTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chown(dir, captureTestJail.uid, captureTestJail.gid); err != nil {
		t.Skipf("a capture needs a tree held by a jail identity, and handing one over needs root: %v", err)
	}
	return dir
}

func captureTestStaging(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tf-capture-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, CaptureStreamFile)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o700)
		_ = os.RemoveAll(dir)
	})
	return dir
}

// TestTailBuffer_SingleLargeWriteNeverExceedsMax pins that a single Write
// bigger than max never transiently holds more than max bytes: the naive
// "append then trim" approach would briefly allocate len(t.buf)+len(p),
// defeating the point of a hard cap against a hostile child dumping a large
// chunk to stderr in one write.
func TestTailBuffer_SingleLargeWriteNeverExceedsMax(t *testing.T) {
	tb := &tailBuffer{max: 16}
	big := bytes.Repeat([]byte{'x'}, 1<<20) // 1 MiB in a single Write call
	n, err := tb.Write(big)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(big) {
		t.Errorf("Write returned n=%d, want %d", n, len(big))
	}
	if len(tb.buf) != tb.max {
		t.Errorf("tb.buf len = %d, want exactly max=%d", len(tb.buf), tb.max)
	}
	if !tb.truncated {
		t.Error("truncated flag not set for an over-cap single write")
	}
}

// TestTailBuffer_AccumulatesAcrossWritesWithinMax pins the untruncated
// happy path: multiple writes that together fit within max concatenate in
// order with no truncation marker.
func TestTailBuffer_AccumulatesAcrossWritesWithinMax(t *testing.T) {
	tb := &tailBuffer{max: 8}
	_, _ = tb.Write([]byte("abcd"))
	_, _ = tb.Write([]byte("efgh"))
	if got := tb.String(); got != "abcdefgh" {
		t.Errorf("String() = %q, want %q", got, "abcdefgh")
	}
	if tb.truncated {
		t.Error("truncated flag set when total writes exactly fill max")
	}
}

// TestTailBuffer_KeepsOnlyLastMaxBytes pins the "tail" half of the name: once
// writes exceed max, only the most recent max bytes survive.
func TestTailBuffer_KeepsOnlyLastMaxBytes(t *testing.T) {
	tb := &tailBuffer{max: 4}
	_, _ = tb.Write([]byte("abcdefgh"))
	if got := tb.String(); !strings.HasSuffix(got, "efgh") {
		t.Errorf("String() = %q, want it to end with tail %q", got, "efgh")
	}
	if !tb.truncated {
		t.Error("truncated flag not set")
	}
}

// TestReadCaptureManifest_UnderCapByteIdentical pins the happy path of the
// shared cap both capture paths enforce: data at or under CaptureManifestMaxBytes
// round-trips byte-identical.
func TestReadCaptureManifest_UnderCapByteIdentical(t *testing.T) {
	origCap := CaptureManifestMaxBytes
	CaptureManifestMaxBytes = 1024
	t.Cleanup(func() { CaptureManifestMaxBytes = origCap })

	want := bytes.Repeat([]byte{0xAB}, 1024)
	got, err := ReadCaptureManifest(bytes.NewReader(want))
	if err != nil {
		t.Fatalf("ReadCaptureManifest: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %d bytes, want %d identical bytes", len(got), len(want))
	}
}

// TestReadCaptureManifest_OverCapErrors pins the loss-contract half of the cap:
// data exceeding CaptureManifestMaxBytes by even one byte is rejected outright
// rather than silently truncated — a truncated-but-otherwise-valid-looking
// bundle would be worse than no delta at all (the park degrades to
// snapshot-less either way, but a truncated one could look intact).
func TestReadCaptureManifest_OverCapErrors(t *testing.T) {
	origCap := CaptureManifestMaxBytes
	CaptureManifestMaxBytes = 1024
	t.Cleanup(func() { CaptureManifestMaxBytes = origCap })

	over := bytes.Repeat([]byte{0xCD}, 1025)
	if _, err := ReadCaptureManifest(bytes.NewReader(over)); err == nil {
		t.Error("expected an over-cap error, got nil")
	}
}

func TestOpenCaptureStaging_RejectsWritableOrReplaceableMembers(t *testing.T) {
	valid := captureTestStaging(t)
	f, err := openCaptureStaging(valid)
	if err != nil {
		t.Fatalf("valid staging rejected: %v", err)
	}
	_ = f.Close()

	wrongMode := captureTestStaging(t)
	if err := os.Chmod(wrongMode, 0o701); err != nil {
		t.Fatal(err)
	}
	if _, err := openCaptureStaging(wrongMode); err == nil {
		t.Error("accepted a staging directory accessible by path to the sandbox uid")
	}

	symlinked := captureTestStaging(t)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("do not truncate"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(symlinked, CaptureStreamFile)
	if err := os.Remove(bundle); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, bundle); err != nil {
		t.Fatal(err)
	}
	if _, err := openCaptureStaging(symlinked); err == nil {
		t.Error("accepted a symlink in place of a fixed staging member")
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "do not truncate" {
		t.Errorf("symlink target = %q, %v; broker must not truncate it", got, err)
	}

	hardLinkStaging := captureTestStaging(t)
	victim = filepath.Join(t.TempDir(), "hardlink-victim")
	if err := os.WriteFile(victim, []byte("do not truncate"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle = filepath.Join(hardLinkStaging, CaptureStreamFile)
	if err := os.Remove(bundle); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, bundle); err != nil {
		t.Fatal(err)
	}
	if _, err := openCaptureStaging(hardLinkStaging); err == nil {
		t.Error("accepted a hard link in place of a fixed staging member")
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "do not truncate" {
		t.Errorf("hardlink target = %q, %v; broker must not truncate it", got, err)
	}
}

func TestOpenCaptureStaging_AnchorsDirectoryAcrossRename(t *testing.T) {
	original := captureTestStaging(t)
	dirFD, wantUID, err := openCaptureStagingDir(original)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFd(dirFD)

	moved := original + "-moved"
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	replacement := captureTestStaging(t)
	if err := os.Symlink(replacement, original); err != nil {
		t.Fatal(err)
	}

	member, err := openCaptureStagingMember(dirFD, CaptureStreamFile, wantUID)
	if err != nil {
		t.Fatalf("open the member through the anchored directory: %v", err)
	}
	defer func() { _ = member.Close() }()
	if _, err := member.WriteString("anchored"); err != nil {
		t.Fatal(err)
	}
	gotMoved, err := os.ReadFile(filepath.Join(moved, CaptureStreamFile))
	if err != nil {
		t.Fatal(err)
	}
	gotReplacement, err := os.ReadFile(filepath.Join(replacement, CaptureStreamFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMoved) != "anchored" || len(gotReplacement) != 0 {
		t.Errorf("moved member = %q, replacement member = %q; want writes confined to anchored directory", gotMoved, gotReplacement)
	}
}

func TestOpenCaptureStagingMember_AnchorsInodeAcrossReplacement(t *testing.T) {
	dir := captureTestStaging(t)
	dirFD, wantUID, err := openCaptureStagingDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFd(dirFD)
	pinnedFD, err := pinCaptureStagingMember(dirFD, CaptureStreamFile)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFd(pinnedFD)

	bundle := filepath.Join(dir, CaptureStreamFile)
	moved := bundle + ".moved"
	if err := os.Rename(bundle, moved); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("do not truncate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, bundle); err != nil {
		t.Fatal(err)
	}

	f, err := openPinnedCaptureStagingMember(pinnedFD, CaptureStreamFile, wantUID)
	if err != nil {
		t.Fatalf("reopen pinned member after replacement: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString("anchored"); err != nil {
		t.Fatal(err)
	}
	gotMoved, err := os.ReadFile(moved)
	if err != nil {
		t.Fatal(err)
	}
	gotVictim, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMoved) != "anchored" || string(gotVictim) != "do not truncate" {
		t.Errorf("pinned member = %q, replacement target = %q; want writes confined to pinned inode", gotMoved, gotVictim)
	}
}

// TestValidateCaptureCheckouts pins the shape of a checkout the broker will put
// in an argv: exactly three plain segments, none an option or a traversal, none
// repeated.
func TestValidateCaptureCheckouts(t *testing.T) {
	if err := ValidateCaptureCheckouts([]string{"acme/app/default", "acme/app/pr-7", "o/r/ref-feat~x"}); err != nil {
		t.Fatalf("valid checkouts rejected: %v", err)
	}
	for _, bad := range []string{"", "acme/app", "acme/app/default/x", "../app/default", "acme/../default", "acme/app/..", "-acme/app/default", "acme//default", "/acme/app/default", "acme/app/de\\fault", "acme/app/.", "acme/app/de\x00fault"} {
		if err := ValidateCaptureCheckouts([]string{bad}); err == nil {
			t.Errorf("accepted checkout %q", bad)
		}
	}
	if err := ValidateCaptureCheckouts([]string{"a/b/c", "a/b/c"}); err == nil {
		t.Error("accepted a checkout named twice")
	}
	many := make([]string, CaptureCheckoutsMax+1)
	for i := range many {
		many[i] = fmt.Sprintf("o/r/s%d", i)
	}
	if err := ValidateCaptureCheckouts(many); err == nil {
		t.Error("accepted more checkouts than the cap")
	}
}

// TestLaunchCaptureJail_RefusesWhatItCannotRead pins the gate in front of
// the jail: a tree that is not a valid run tree, or that no jail identity
// holds, never reaches a launch.
func TestLaunchCaptureJail_RefusesWhatItCannotRead(t *testing.T) {
	for _, path := range []string{
		"",                               // empty
		"relative/path",                  // not absolute
		"/",                              // the root itself
		"/tmp",                           // one component — never a run tree
		"/tmp/../etc/cron.d",             // non-clean
		"/nonexistent-tf-capture-test/x", // no such tree
	} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		if _, err := LaunchCaptureJail(context.Background(), CaptureParams{Root: path, StagingDir: captureTestStaging(t)}, w); err == nil {
			t.Errorf("LaunchCaptureJail(%q) = nil error, want validation rejection", path)
		}
	}

	// A tree the orchestrator itself owns has no jail identity to be read as.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	_, err = LaunchCaptureJail(context.Background(), CaptureParams{Root: t.TempDir(), StagingDir: captureTestStaging(t)}, w)
	if err == nil || !strings.Contains(err.Error(), "not a jail identity") {
		t.Errorf("LaunchCaptureJail over a tree no jail holds = %v, want a refusal naming the missing identity", err)
	}
}
