package delegate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// repoWithLocalCommits builds a run root holding one checkout whose branch
// carries n commits no remote has, each touching several files of
// near-identical text, so the bundle git writes for them has deltas to search.
// It returns the root and the checkout.
func repoWithLocalCommits(t *testing.T, n int) (string, snapshotCheckout) {
	t.Helper()
	setupGitTestEnv(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitT(t, "", "init", "-q", "--bare", "-b", "main", remote)
	root := t.TempDir()
	co := snapshotCheckout{repoID: "acme/repo", slug: "ref-main", rel: "acme/repo/ref-main"}
	co.path = filepath.Join(root, filepath.FromSlash(co.rel))
	gitT(t, "", "clone", "-q", remote, co.path)
	wt := co.path
	writeFile(t, filepath.Join(wt, "README.md"), "base\n")
	gitT(t, wt, "add", "-A")
	gitT(t, wt, "commit", "-q", "-m", "base")
	gitT(t, wt, "push", "-q", "origin", "HEAD:main")
	for i := 0; i < n; i++ {
		addLocalCommit(t, wt, i)
	}
	return root, co
}

func addLocalCommit(t *testing.T, wt string, i int) {
	t.Helper()
	for f := 0; f < 6; f++ {
		var b strings.Builder
		for line := 0; line < 400; line++ {
			fmt.Fprintf(&b, "file %d line %d: the quick brown fox jumps over the lazy dog, revision %d\n", f, line, i)
		}
		writeFile(t, filepath.Join(wt, "src", fmt.Sprintf("f%d.txt", f)), b.String())
	}
	gitT(t, wt, "add", "-A")
	gitT(t, wt, "commit", "-q", "-m", fmt.Sprintf("local work %d", i))
}

func captureFingerprint(t *testing.T, root string, checkouts ...snapshotCheckout) string {
	t.Helper()
	w := snapshotWrite{wtPath: root, runtime: domain.ConversationRuntimeNative, reason: snapshotReasonCheckpoint, checkouts: checkouts}
	captured, err := captureSnapshot(context.Background(), w)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	defer captured.release()
	if len(captured.checkouts) == 0 {
		t.Fatal("the capture carried no checkout; the fixture's local commits are what this test is about")
	}
	fp, err := snapshotFingerprint(context.Background(), captured, root)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return fp
}

// TestSnapshotFingerprint_StableAcrossCapturesOfAnUnchangedTree: the bundle
// of an agent's unpushed commits is not byte-for-byte reproducible, since git
// packs it on several threads. A fingerprint over those bytes reads an
// unchanged tree as changed, and every checkpoint uploads again. What the
// bundle carries is the same every time, and that is what the fingerprint
// has to see.
func TestSnapshotFingerprint_StableAcrossCapturesOfAnUnchangedTree(t *testing.T) {
	root, co := repoWithLocalCommits(t, 15)

	seen := map[string]int{}
	for i := 0; i < 30; i++ {
		seen[captureFingerprint(t, root, co)]++
	}
	if len(seen) != 1 {
		t.Fatalf("30 captures of one unchanged tree gave %d fingerprints: %v", len(seen), seen)
	}

	// A new commit is a different tree.
	var before string
	for fp := range seen {
		before = fp
	}
	addLocalCommit(t, co.path, 99)
	if after := captureFingerprint(t, root, co); after == before {
		t.Fatal("a new commit left the fingerprint unchanged; the checkpoint would skip storing it")
	}
}

// TestSnapshotFingerprint_ScratchRewrittenWithItsOldTimes: a tool can replace
// a scratch file with same-sized content and put its old modification time
// back — cp -p does, an archive extraction does. Size and modification time
// then match the last checkpoint's, and a fingerprint of those alone would
// skip storing the new content and record the skip as covering it: a restore
// then brings back the old file with nothing saying it is old.
func TestSnapshotFingerprint_ScratchRewrittenWithItsOldTimes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the change stamp reads Linux's stat")
	}
	for _, tc := range []struct {
		name    string
		rewrite func(t *testing.T, path string)
	}{
		{"in place, like cp -p", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("bravo\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"replaced by a new file, like an extraction", func(t *testing.T, path string) {
			tmp := path + ".new"
			if err := os.WriteFile(tmp, []byte("bravo\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, co := repoWithLocalCommits(t, 1)
			path := filepath.Join(root, "_tfac", "notes.txt")
			writeFile(t, path, "alpha\n")
			old, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			before := captureFingerprint(t, root, co)
			if again := captureFingerprint(t, root, co); again != before {
				t.Fatal("an unchanged scratch file changed the fingerprint")
			}

			// Past the coarsest clock tick a filesystem stamps change times
			// with, so the rewrite's change time differs from the write's.
			time.Sleep(50 * time.Millisecond)
			tc.rewrite(t, path)
			if err := os.Chtimes(path, old.ModTime(), old.ModTime()); err != nil {
				t.Fatal(err)
			}
			if now, _ := os.Stat(path); now.Size() != old.Size() || !now.ModTime().Equal(old.ModTime()) {
				t.Fatalf("fixture: size %d mtime %v, want the old %d %v", now.Size(), now.ModTime(), old.Size(), old.ModTime())
			}
			if after := captureFingerprint(t, root, co); after == before {
				t.Fatal("new scratch content under the old size and modification time left the fingerprint unchanged")
			}
		})
	}
}

// TestSnapshotFingerprint_SeesEveryCheckout: an uncommitted edit inside any
// checkout under the root is a different workspace. A fingerprint that read
// only the root's own git — and the root has none — would call a checkpoint
// taken during such an edit unchanged, and record the skip as covering it.
func TestSnapshotFingerprint_SeesEveryCheckout(t *testing.T) {
	root, first := repoWithLocalCommits(t, 1)
	second := snapshotCheckout{repoID: "acme/other", slug: "default", rel: "acme/other/default"}
	second.path = filepath.Join(root, filepath.FromSlash(second.rel))
	gitT(t, "", "clone", "-q", first.path, second.path)

	before := captureFingerprint(t, root, first, second)
	if again := captureFingerprint(t, root, first, second); again != before {
		t.Fatal("an unchanged pair of checkouts changed the fingerprint")
	}
	writeFile(t, filepath.Join(second.path, "README.md"), "edited in the second checkout\n")
	edited := captureFingerprint(t, root, first, second)
	if edited == before {
		t.Fatal("an uncommitted edit in the second checkout left the fingerprint unchanged")
	}
	writeFile(t, filepath.Join(first.path, "untracked.txt"), "new in the first\n")
	if captureFingerprint(t, root, first, second) == edited {
		t.Fatal("an untracked file in the first checkout left the fingerprint unchanged")
	}
}

// TestSnapshotFingerprint_SeesAnLFSObjectArrive: a run can commit a Git LFS
// pointer before its object is in the store. The object arriving later is a
// change a checkpoint has to store, though no commit moved.
func TestSnapshotFingerprint_SeesAnLFSObjectArrive(t *testing.T) {
	root, co := repoWithLocalCommits(t, 1)
	const content = "the run's new asset\n"
	const oid = "3b83b1604b9aa647686a75a479e0a6da7471a96ac3228dac703c071d85f9dc9e"
	commitLFSPointer(t, co.path, "asset.bin", oid, len(content))
	before := captureFingerprint(t, root, co)
	writeFile(t, filepath.Join(co.path, ".git", "lfs", "objects", oid[:2], oid[2:4], oid), content)
	if after := captureFingerprint(t, root, co); after == before {
		t.Fatal("an LFS object arriving left the fingerprint unchanged; the checkpoint would skip storing it")
	}
}

// commitLFSPointer commits a Git LFS pointer to name at the checkout dir, with
// git-lfs's filter off so it is committed as written.
func commitLFSPointer(t *testing.T, dir, name, oid string, size int) {
	t.Helper()
	writeFile(t, filepath.Join(dir, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	writeFile(t, filepath.Join(dir, name), fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size))
	lfsOff := []string{"-c", "filter.lfs.process=", "-c", "filter.lfs.clean=", "-c", "filter.lfs.required=false"}
	gitT(t, dir, append(lfsOff, "add", ".gitattributes", name)...)
	gitT(t, dir, append(lfsOff, "commit", "-q", "-m", "add "+name)...)
}
