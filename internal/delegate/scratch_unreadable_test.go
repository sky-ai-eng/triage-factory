package delegate

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestTarScratch_SkipsWhatItCannotRead pins that an entry the snapshotting
// process may not read leaves the scratch archive short of that entry rather
// than failing it: a jailed agent's owner-only file or directory is closed to
// the orchestrator, and the rest of the scratch still has to reach the blob
// whole.
func TestTarScratch_SkipsWhatItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file whatever its mode; the case needs an identity the modes apply to")
	}
	wt := t.TempDir()
	scratch := filepath.Join(wt, worktree.ScratchDir)
	for path, body := range map[string]string{
		"notes.txt":            "kept",
		"nested/kept.txt":      "kept too",
		"closed-dir/inner.txt": "behind a closed directory",
		"closed.txt":           "closed file",
	} {
		full := filepath.Join(scratch, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"closed.txt", "closed-dir"} {
		full := filepath.Join(scratch, p)
		if err := os.Chmod(full, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(full, 0o755) })
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if _, err := tarScratch(context.Background(), tw, wt); err != nil {
		t.Fatalf("tarScratch: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var got []string
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		got = append(got, hdr.Name)
	}
	want := []string{snapScratchPrefix + "nested/kept.txt", snapScratchPrefix + "notes.txt"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("archive members = %v, want %v", got, want)
	}

	_, unreadable, err := worktree.WalkScratch(context.Background(), wt, func(rel, path string, fi os.FileInfo) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		return f.Close()
	})
	if err != nil {
		t.Fatalf("walkScratch: %v", err)
	}
	slices.Sort(unreadable)
	if want := []string{"closed-dir", "closed.txt"}; !slices.Equal(unreadable, want) {
		t.Errorf("unreadable = %v, want %v", unreadable, want)
	}
}
