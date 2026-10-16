package worktree

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestWalkScratch_VisitThatFindsTheFileGoneOrDeclinesIt: a file removed between
// the walk's stat and the visit's open is skipped quietly, and one the visit
// declines is listed as unreadable; neither fails the walk.
func TestWalkScratch_VisitThatFindsTheFileGoneOrDeclinesIt(t *testing.T) {
	wt := t.TempDir()
	for _, n := range []string{"gone", "declined", "kept"} {
		p := filepath.Join(wt, ScratchDir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var visited []string
	_, unreadable, err := WalkScratch(context.Background(), wt, func(rel, _ string, _ os.FileInfo) error {
		switch rel {
		case "gone":
			return &os.PathError{Op: "open", Path: rel, Err: fs.ErrNotExist}
		case "declined":
			return fmt.Errorf("%w: too long", ErrScratchSkipped)
		}
		visited = append(visited, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkScratch: %v", err)
	}
	if len(visited) != 1 || visited[0] != "kept" {
		t.Errorf("visited = %v", visited)
	}
	if len(unreadable) != 1 || unreadable[0] != "declined" {
		t.Errorf("unreadable = %v, want only the declined file", unreadable)
	}
}

func TestScratchExcluded(t *testing.T) {
	for rel, want := range map[string]bool{
		"knowledge/team/k.md": true, "entity-memory/e.md": true, "ci-logs/run1/log.txt": true,
		"ci-logs": true, "notes.txt": false, "sub/knowledge/k.md": false, "knowledge-notes.txt": false,
	} {
		if got := ScratchExcluded(rel); got != want {
			t.Errorf("ScratchExcluded(%q) = %v, want %v", rel, got, want)
		}
	}
}
