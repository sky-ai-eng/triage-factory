package projectclassify

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// TestReadProjectKBFromStore pins the multi-mode classifier KB read: only .md
// files are concatenated, in lexical order, with the same "## name" header the
// on-disk path uses, so classifier prompts stay identical across modes.
func TestReadProjectKBFromStore(t *testing.T) {
	kb := kbstore.New(storage.NewFS(t.TempDir()))
	ctx := context.Background()
	const org, proj = "o", "p"
	seed := map[string]string{
		"b.md":      "beta body",
		"a.md":      "alpha body",
		"notes.txt": "ignored, not markdown",
		"c.png":     "\x89PNG",
	}
	for name, body := range seed {
		if err := kb.Put(ctx, org, proj, name, strings.NewReader(body)); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}

	out, truncated, err := readProjectKBFromStore(ctx, kb, org, proj)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if truncated {
		t.Fatalf("unexpected truncation")
	}
	// Lexical order: a.md before b.md; non-.md excluded.
	if !strings.Contains(out, "## a.md") {
		t.Fatalf("expected a.md in KB prompt; got:\n%s", out)
	}
	if strings.Index(out, "## a.md") >= strings.Index(out, "## b.md") {
		t.Fatalf("expected a.md before b.md; got:\n%s", out)
	}
	if strings.Contains(out, "notes.txt") || strings.Contains(out, "c.png") {
		t.Fatalf("non-markdown leaked into KB prompt:\n%s", out)
	}
	if !strings.Contains(out, "alpha body") || !strings.Contains(out, "beta body") {
		t.Fatalf("markdown bodies missing:\n%s", out)
	}
}

// TestReadProjectKBFromStore_EmptyProject returns an empty string, no error,
// for a project with no KB — the same contract the on-disk path has for a
// missing dir.
func TestReadProjectKBFromStore_EmptyProject(t *testing.T) {
	kb := kbstore.New(storage.NewFS(t.TempDir()))
	out, truncated, err := readProjectKBFromStore(context.Background(), kb, "o", "empty")
	if err != nil || truncated || out != "" {
		t.Fatalf("empty project = (%q, %v, %v); want (\"\", false, nil)", out, truncated, err)
	}
}
