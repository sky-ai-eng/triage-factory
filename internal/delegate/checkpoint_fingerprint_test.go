package delegate

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// repoWithLocalCommits builds a worktree whose branch carries n commits no
// remote has, each touching several files of near-identical text, so the
// bundle git writes for them has deltas to search.
func repoWithLocalCommits(t *testing.T, n int) string {
	t.Helper()
	setupGitTestEnv(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitT(t, "", "init", "-q", "--bare", "-b", "main", remote)
	wt := filepath.Join(t.TempDir(), "wt")
	gitT(t, "", "clone", "-q", remote, wt)
	writeFile(t, filepath.Join(wt, "README.md"), "base\n")
	gitT(t, wt, "add", "-A")
	gitT(t, wt, "commit", "-q", "-m", "base")
	gitT(t, wt, "push", "-q", "origin", "HEAD:main")
	for i := 0; i < n; i++ {
		addLocalCommit(t, wt, i)
	}
	return wt
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

func captureFingerprint(t *testing.T, wt string) string {
	t.Helper()
	w := snapshotWrite{wtPath: wt, runtime: domain.ConversationRuntimeNative, reason: snapshotReasonCheckpoint}
	captured, err := captureSnapshot(context.Background(), w)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	defer captured.release()
	if captured.state.Delta == nil || len(captured.state.Delta.Bundle) == 0 && captured.state.BundlePath == "" {
		t.Fatal("the capture carried no bundle; the fixture's local commits are what this test is about")
	}
	fp, err := snapshotFingerprint(context.Background(), captured.state, wt)
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
	wt := repoWithLocalCommits(t, 15)

	seen := map[string]int{}
	for i := 0; i < 30; i++ {
		seen[captureFingerprint(t, wt)]++
	}
	if len(seen) != 1 {
		t.Fatalf("30 captures of one unchanged tree gave %d fingerprints: %v", len(seen), seen)
	}

	// A new commit is a different tree.
	var before string
	for fp := range seen {
		before = fp
	}
	addLocalCommit(t, wt, 99)
	if after := captureFingerprint(t, wt); after == before {
		t.Fatal("a new commit left the fingerprint unchanged; the checkpoint would skip storing it")
	}
}
