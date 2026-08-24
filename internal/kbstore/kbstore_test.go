package kbstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

const (
	testOrg  = "org-1"
	testTeam = "team-a"
)

// TestKB_Conformance runs the one behavioral contract both backends owe
// against each of them. Anything backend-specific (the on-disk layout, the
// object key convention) lives in its own test below — what is here is what a
// caller may rely on without knowing which deployment answered.
func TestKB_Conformance(t *testing.T) {
	t.Run("local", func(t *testing.T) { runKBConformance(t, NewLocalAt(t.TempDir())) })
	t.Run("object", func(t *testing.T) { runKBConformance(t, NewObject(storage.NewFS(t.TempDir()))) })
}

func runKBConformance(t *testing.T, kb KB) {
	t.Helper()
	ctx := context.Background()

	t.Run("empty kb lists nothing", func(t *testing.T) {
		got, err := kb.List(ctx, testOrg, "team-never-written", Roots(), "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("List on an unwritten KB = %v; want empty", got)
		}
	})

	t.Run("round trip and list ordering", func(t *testing.T) {
		team := "team-roundtrip"
		put(t, kb, team, RootShared, "conventions.md", "shared conventions")
		put(t, kb, team, RootPrivate, "notes/deploy.md", "how we deploy")
		put(t, kb, team, RootPrivate, "architecture.md", "the shape of it")

		got, err := kb.List(ctx, testOrg, team, Roots(), "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		want := []string{"private/architecture.md", "private/notes/deploy.md", "shared/conventions.md"}
		if diff := refStrings(got); !equalStrings(diff, want) {
			t.Fatalf("List = %v; want %v (private before shared, then path)", diff, want)
		}
		if got[0].Size != int64(len("the shape of it")) {
			t.Errorf("Size = %d; want %d", got[0].Size, len("the shape of it"))
		}
		if got[0].ModTime.IsZero() {
			t.Errorf("ModTime is zero; a listing entry must carry one")
		}

		body := read(t, kb, team, Ref{Root: RootPrivate, Path: "notes/deploy.md"})
		if body != "how we deploy" {
			t.Fatalf("Open = %q; want %q", body, "how we deploy")
		}
	})

	t.Run("roots scope the read", func(t *testing.T) {
		team := "team-scoped"
		put(t, kb, team, RootPrivate, "secret.md", "ours alone")
		put(t, kb, team, RootShared, "public.md", "for everyone")

		got, err := kb.List(ctx, testOrg, team, []Root{RootShared}, "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if !equalStrings(refStrings(got), []string{"shared/public.md"}) {
			t.Fatalf("shared-only List = %v; want just the shared entry", refStrings(got))
		}

		// An empty root set is an empty answer, never "everything" — a
		// visibility gate that resolved to nothing must not widen.
		none, err := kb.List(ctx, testOrg, team, nil, "")
		if err != nil {
			t.Fatalf("List with no roots: %v", err)
		}
		if len(none) != 0 {
			t.Fatalf("List with no roots = %v; want empty", refStrings(none))
		}
	})

	t.Run("prefix filter matches segments, not bytes", func(t *testing.T) {
		team := "team-prefix"
		put(t, kb, team, RootPrivate, "notes/a.md", "a")
		put(t, kb, team, RootPrivate, "notes/deep/b.md", "b")
		put(t, kb, team, RootPrivate, "notes-archive/c.md", "c")

		got, err := kb.List(ctx, testOrg, team, Roots(), "notes")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		want := []string{"private/notes/a.md", "private/notes/deep/b.md"}
		if !equalStrings(refStrings(got), want) {
			t.Fatalf("prefix List = %v; want %v (notes-archive is a sibling, not a child)", refStrings(got), want)
		}
	})

	t.Run("stat and overwrite", func(t *testing.T) {
		team := "team-stat"
		ref := Ref{Root: RootShared, Path: "guide.md"}
		put(t, kb, team, ref.Root, ref.Path, "v1")
		put(t, kb, team, ref.Root, ref.Path, "v2 is longer")

		info, err := kb.Stat(ctx, testOrg, team, ref)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Size != int64(len("v2 is longer")) {
			t.Fatalf("Stat.Size = %d; want %d (a Put overwrites)", info.Size, len("v2 is longer"))
		}
		if got := read(t, kb, team, ref); got != "v2 is longer" {
			t.Fatalf("Open after overwrite = %q", got)
		}
	})

	t.Run("missing entries report ErrNotFound", func(t *testing.T) {
		team := "team-missing"
		if _, err := kb.Stat(ctx, testOrg, team, Ref{Root: RootShared, Path: "nope.md"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Stat on a missing entry = %v; want ErrNotFound", err)
		}
		if _, err := kb.Open(ctx, testOrg, team, Ref{Root: RootShared, Path: "nope.md"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Open on a missing entry = %v; want ErrNotFound", err)
		}
	})

	t.Run("delete removes a subtree and reports the count", func(t *testing.T) {
		team := "team-delete"
		put(t, kb, team, RootPrivate, "docs/a.md", "a")
		put(t, kb, team, RootPrivate, "docs/sub/b.md", "b")
		put(t, kb, team, RootPrivate, "docs-other/c.md", "c")

		n, err := kb.Delete(ctx, testOrg, team, Ref{Root: RootPrivate, Path: "docs"})
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if n != 2 {
			t.Fatalf("Delete count = %d; want 2", n)
		}
		got, err := kb.List(ctx, testOrg, team, Roots(), "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if !equalStrings(refStrings(got), []string{"private/docs-other/c.md"}) {
			t.Fatalf("after Delete = %v; want the sibling folder untouched", refStrings(got))
		}

		// Deleting a path that holds nothing is an answer, not an error: the
		// caller asked for it to be gone and it is.
		if n, err := kb.Delete(ctx, testOrg, team, Ref{Root: RootPrivate, Path: "docs"}); err != nil || n != 0 {
			t.Fatalf("second Delete = %d, %v; want 0, nil", n, err)
		}
	})

	t.Run("move publishes a whole folder", func(t *testing.T) {
		team := "team-publish"
		put(t, kb, team, RootPrivate, "runbooks/deploy.md", "deploy steps")
		put(t, kb, team, RootPrivate, "runbooks/oncall/rota.md", "who is on")

		n, err := kb.Move(ctx, testOrg, team,
			Ref{Root: RootPrivate, Path: "runbooks"},
			Ref{Root: RootShared, Path: "runbooks"})
		if err != nil {
			t.Fatalf("Move: %v", err)
		}
		if n != 2 {
			t.Fatalf("Move count = %d; want 2", n)
		}
		got, err := kb.List(ctx, testOrg, team, Roots(), "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		want := []string{"shared/runbooks/deploy.md", "shared/runbooks/oncall/rota.md"}
		if !equalStrings(refStrings(got), want) {
			t.Fatalf("after publish = %v; want %v (the subtree shape is preserved)", refStrings(got), want)
		}
		if body := read(t, kb, team, Ref{Root: RootShared, Path: "runbooks/oncall/rota.md"}); body != "who is on" {
			t.Fatalf("moved content = %q; want %q", body, "who is on")
		}
	})

	t.Run("move renames within a root", func(t *testing.T) {
		team := "team-rename"
		put(t, kb, team, RootPrivate, "draft.md", "words")
		if _, err := kb.Move(ctx, testOrg, team,
			Ref{Root: RootPrivate, Path: "draft.md"},
			Ref{Root: RootPrivate, Path: "final/notes.md"}); err != nil {
			t.Fatalf("Move: %v", err)
		}
		got, _ := kb.List(ctx, testOrg, team, Roots(), "")
		if !equalStrings(refStrings(got), []string{"private/final/notes.md"}) {
			t.Fatalf("after rename = %v", refStrings(got))
		}
	})

	t.Run("move refuses a collision and changes nothing", func(t *testing.T) {
		team := "team-collide"
		put(t, kb, team, RootPrivate, "guide.md", "private version")
		put(t, kb, team, RootShared, "guide.md", "shared version")

		_, err := kb.Move(ctx, testOrg, team,
			Ref{Root: RootPrivate, Path: "guide.md"},
			Ref{Root: RootShared, Path: "guide.md"})
		if !errors.Is(err, ErrExists) {
			t.Fatalf("colliding Move = %v; want ErrExists", err)
		}
		if body := read(t, kb, team, Ref{Root: RootPrivate, Path: "guide.md"}); body != "private version" {
			t.Fatalf("source after refused Move = %q; want it untouched", body)
		}
		if body := read(t, kb, team, Ref{Root: RootShared, Path: "guide.md"}); body != "shared version" {
			t.Fatalf("destination after refused Move = %q; want it untouched", body)
		}
	})

	t.Run("move of an empty path is not found", func(t *testing.T) {
		_, err := kb.Move(ctx, testOrg, "team-nothing",
			Ref{Root: RootPrivate, Path: "gone"},
			Ref{Root: RootShared, Path: "gone"})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Move of nothing = %v; want ErrNotFound", err)
		}
	})

	t.Run("move into its own subtree is refused", func(t *testing.T) {
		team := "team-selfmove"
		put(t, kb, team, RootPrivate, "docs/a.md", "a")
		_, err := kb.Move(ctx, testOrg, team,
			Ref{Root: RootPrivate, Path: "docs"},
			Ref{Root: RootPrivate, Path: "docs/inner"})
		if !errors.Is(err, ErrInvalidName) {
			t.Fatalf("self-nesting Move = %v; want ErrInvalidName", err)
		}
	})

	t.Run("teams never see each other", func(t *testing.T) {
		put(t, kb, "team-one", RootShared, "mine.md", "one")
		put(t, kb, "team-two", RootShared, "mine.md", "two")
		got, _ := kb.List(ctx, testOrg, "team-one", Roots(), "")
		if !equalStrings(refStrings(got), []string{"shared/mine.md"}) {
			t.Fatalf("team-one List = %v", refStrings(got))
		}
		if body := read(t, kb, "team-one", Ref{Root: RootShared, Path: "mine.md"}); body != "one" {
			t.Fatalf("team-one content = %q; want its own", body)
		}
	})

	t.Run("an invalid path is refused at every door", func(t *testing.T) {
		team := "team-invalid"
		bad := Ref{Root: RootPrivate, Path: "../escape.md"}
		if err := kb.Put(ctx, testOrg, team, bad, strings.NewReader("x")); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Put with traversal = %v; want ErrInvalidName", err)
		}
		if _, err := kb.Open(ctx, testOrg, team, bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Open with traversal = %v; want ErrInvalidName", err)
		}
		if _, err := kb.Stat(ctx, testOrg, team, bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Stat with traversal = %v; want ErrInvalidName", err)
		}
		if _, err := kb.Delete(ctx, testOrg, team, bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Delete with traversal = %v; want ErrInvalidName", err)
		}
		unknown := Ref{Root: Root("public"), Path: "a.md"}
		if err := kb.Put(ctx, testOrg, team, unknown, strings.NewReader("x")); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Put with an unknown root = %v; want ErrInvalidName", err)
		}
		if _, err := kb.List(ctx, testOrg, team, []Root{"public"}, ""); !errors.Is(err, ErrInvalidName) {
			t.Errorf("List with an unknown root = %v; want ErrInvalidName", err)
		}
		if _, err := kb.List(ctx, testOrg, team, Roots(), "../x"); !errors.Is(err, ErrInvalidName) {
			t.Errorf("List with a traversal prefix = %v; want ErrInvalidName", err)
		}
	})
}

// TestObjectKB_KeyConvention pins the multi-mode key layout. It is the
// contract an operator reads a bucket listing against, and the thing a
// re-key would silently change.
func TestObjectKB_KeyConvention(t *testing.T) {
	root := t.TempDir()
	kb := NewObject(storage.NewFS(root))
	put(t, kb, testTeam, RootShared, "notes/a.md", "a")
	put(t, kb, testTeam, RootPrivate, "b.md", "b")

	for _, want := range []string{
		filepath.Join(root, testOrg, "teams", testTeam, "kb", "shared", "notes", "a.md"),
		filepath.Join(root, testOrg, "teams", testTeam, "kb", "private", "b.md"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected key at %s: %v", want, err)
		}
	}
}

// TestLocalKB_OnDiskLayout pins the local-mode on-disk layout — the paths a
// local operator edits their team's knowledge at with an ordinary editor.
func TestLocalKB_OnDiskLayout(t *testing.T) {
	root := t.TempDir()
	kb := NewLocalAt(root)
	put(t, kb, testTeam, RootPrivate, "notes/deploy.md", "steps")

	want := filepath.Join(root, "teams", testTeam, "kb", "private", "notes", "deploy.md")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected file at %s: %v", want, err)
	}
}

// TestLocalKB_PrunesEmptyFolders guards the one way the two backends could
// disagree about what a KB contains: the object store has no directories at
// all, so a hollow one left behind locally would list as a folder on one
// deployment and not the other.
func TestLocalKB_PrunesEmptyFolders(t *testing.T) {
	root := t.TempDir()
	kb := NewLocalAt(root)
	put(t, kb, testTeam, RootPrivate, "docs/sub/a.md", "a")
	if _, err := kb.Delete(context.Background(), testOrg, testTeam, Ref{Root: RootPrivate, Path: "docs/sub/a.md"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, gone := range []string{
		filepath.Join(root, "teams", testTeam, "kb", "private", "docs", "sub"),
		filepath.Join(root, "teams", testTeam, "kb", "private", "docs"),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned (err=%v)", gone, err)
		}
	}
	// The root itself survives: it is the team's KB, not a folder in it.
	if _, err := os.Stat(filepath.Join(root, "teams", testTeam, "kb", "private")); err != nil {
		t.Errorf("the private root should survive a prune: %v", err)
	}
}

// TestLocalKB_PrunesBranchingSubtrees: emptying a folder that branches leaves
// several empty leaves at once, and a prune that only climbs from one of them
// stops at the first non-empty parent with the siblings still there. An object
// store has no empty folders at all, so anything left here would make the same
// KB list differently depending on the deployment mode.
func TestLocalKB_PrunesBranchingSubtrees(t *testing.T) {
	root := t.TempDir()
	kb := NewLocalAt(root)
	put(t, kb, testTeam, RootPrivate, "docs/sub/a.md", "a")
	put(t, kb, testTeam, RootPrivate, "docs/other/b.md", "b")
	put(t, kb, testTeam, RootPrivate, "keep/c.md", "c")

	if n, err := kb.Delete(context.Background(), testOrg, testTeam, Ref{Root: RootPrivate, Path: "docs"}); err != nil || n != 2 {
		t.Fatalf("Delete = %d, %v; want 2, nil", n, err)
	}

	base := filepath.Join(root, "teams", testTeam, "kb", "private")
	for _, gone := range []string{"docs/sub", "docs/other", "docs"} {
		if _, err := os.Stat(filepath.Join(base, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("%s survived the prune (err=%v)", gone, err)
		}
	}
	// A sibling that still holds a document is untouched, and so is the root.
	if _, err := os.Stat(filepath.Join(base, "keep", "c.md")); err != nil {
		t.Errorf("a populated sibling folder was pruned: %v", err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Errorf("the visibility root should survive a prune: %v", err)
	}
}

// TestLocalKB_FolderIsNotAnEntry: local mode has real directories, so it is
// the only backend that could hand one back as though it were a document. It
// must not — an object store has no folder key to serve, and a KB whose
// contents depended on the deployment mode would be the one place this layout
// leaked its implementation. (The assertion is backend-specific rather than a
// conformance case for exactly that reason: there is no folder on the object
// side to point at.)
func TestLocalKB_FolderIsNotAnEntry(t *testing.T) {
	kb := NewLocalAt(t.TempDir())
	put(t, kb, testTeam, RootPrivate, "folder/inside.md", "x")
	folder := Ref{Root: RootPrivate, Path: "folder"}
	if _, err := kb.Stat(context.Background(), testOrg, testTeam, folder); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat on a folder = %v; want ErrNotFound", err)
	}
	if rc, err := kb.Open(context.Background(), testOrg, testTeam, folder); !errors.Is(err, ErrNotFound) {
		if err == nil {
			_ = rc.Close()
		}
		t.Fatalf("Open on a folder = %v; want ErrNotFound", err)
	}
}

// TestLocalKB_SymlinkIsNeverAnEntry is the security case, and it is about a
// `..` that lives on the DISK rather than in the path string. ValidatePath
// rejects traversal in a name; it is no defense at all against a symlink an
// operator's sync tool, editor or repo checkout dropped in the tree. Before the
// backend was confined, such a link was invisible to List and fully readable by
// exact path — and a link as an INTERMEDIATE component let a delete remove a
// file outside the knowledge base entirely.
//
// Every door is asserted, because a guarantee that holds on the listing and not
// on the read is not a guarantee.
func TestLocalKB_SymlinkIsNeverAnEntry(t *testing.T) {
	root := t.TempDir()
	kb := NewLocalAt(root)
	ctx := context.Background()

	// A real document, so the KB tree exists and the assertions below are
	// about the links rather than about an empty store.
	put(t, kb, testTeam, RootPrivate, "real.md", "ours")

	outside := t.TempDir()
	secret := filepath.Join(outside, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY MATERIAL"), 0o600); err != nil {
		t.Fatalf("write the target file: %v", err)
	}
	dir := filepath.Join(root, "teams", testTeam, "kb", "private")
	if err := os.Symlink(secret, filepath.Join(dir, "leaf.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "folder")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, tc := range []struct {
		what string
		ref  Ref
	}{
		{"a symlinked leaf", Ref{Root: RootPrivate, Path: "leaf.md"}},
		{"a file under a symlinked folder", Ref{Root: RootPrivate, Path: "folder/id_rsa"}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if _, err := kb.Stat(ctx, testOrg, testTeam, tc.ref); !errors.Is(err, ErrNotFound) {
				t.Errorf("Stat = %v; want ErrNotFound — nothing outside the KB is an entry", err)
			}
			if rc, err := kb.Open(ctx, testOrg, testTeam, tc.ref); err == nil {
				body, _ := io.ReadAll(rc)
				_ = rc.Close()
				t.Errorf("Open streamed %q from outside the knowledge base", body)
			} else if !errors.Is(err, ErrNotFound) {
				t.Errorf("Open = %v; want ErrNotFound", err)
			}
			if n, err := kb.Delete(ctx, testOrg, testTeam, tc.ref); err == nil && n > 0 {
				t.Errorf("Delete removed %d entries through a symlink", n)
			}
		})
	}

	// The targets are untouched: nothing was read out of them and nothing was
	// removed. This is the assertion the delete escape would have failed.
	if body, err := os.ReadFile(secret); err != nil || string(body) != "PRIVATE KEY MATERIAL" {
		t.Errorf("the file outside the KB was disturbed: %q, %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "id_rsa")); err != nil {
		t.Errorf("a file outside the KB was deleted through the KB: %v", err)
	}

	// And the listing still shows only the team's own document.
	got, err := kb.List(ctx, testOrg, testTeam, Roots(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !equalStrings(refStrings(got), []string{"private/real.md"}) {
		t.Fatalf("List = %v; a symlink is not a KB entry", refStrings(got))
	}
}

// TestLocalKB_WritesCannotEscapeEither: a symlinked folder is a write escape
// too — MkdirAll and a rename through one would place a document outside the
// team's tree, or replace a file that is not the KB's to replace.
func TestLocalKB_WritesCannotEscapeEither(t *testing.T) {
	root := t.TempDir()
	kb := NewLocalAt(root)
	outside := t.TempDir()

	put(t, kb, testTeam, RootPrivate, "real.md", "ours")
	dir := filepath.Join(root, "teams", testTeam, "kb", "private")
	if err := os.Symlink(outside, filepath.Join(dir, "folder")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := kb.Put(context.Background(), testOrg, testTeam,
		Ref{Root: RootPrivate, Path: "folder/planted.md"}, strings.NewReader("planted"))
	if err == nil {
		if _, statErr := os.Stat(filepath.Join(outside, "planted.md")); statErr == nil {
			t.Fatalf("Put wrote a document outside the knowledge base")
		}
	}
	if _, statErr := os.Stat(filepath.Join(outside, "planted.md")); statErr == nil {
		t.Errorf("a file was planted outside the KB tree")
	}
}

// TestLocalKB_SkipsSymlinks: a symlink in a local KB could address anything on
// the operator's disk, and the object backend cannot produce one, so it is not
// a KB entry in either.
func TestLocalKB_SkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	kb := NewLocalAt(root)
	put(t, kb, testTeam, RootPrivate, "real.md", "real")

	dir := filepath.Join(root, "teams", testTeam, "kb", "private")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := kb.List(context.Background(), testOrg, testTeam, Roots(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !equalStrings(refStrings(got), []string{"private/real.md"}) {
		t.Fatalf("List = %v; a symlink is not a KB entry", refStrings(got))
	}
}

// TestObjectKB_RollbackAttemptsEveryCopy: a failed multi-file move has to undo
// ALL of its copies. Stopping at the first delete that fails would strand the
// rest at destination keys nothing names — the one error naming the entry that
// failed would be the only record they exist at all.
func TestObjectKB_RollbackAttemptsEveryCopy(t *testing.T) {
	inner := storage.NewFS(t.TempDir())
	flaky := &flakyStorage{Storage: inner}
	kb := NewObject(flaky)
	ctx := context.Background()

	for _, name := range []string{"a.md", "b.md", "c.md", "d.md"} {
		put(t, kb, testTeam, RootPrivate, "docs/"+name, name)
	}
	// The last copy fails, so a, b and c must be rolled back. The delete that
	// is refused is a.md — deliberately the EARLIEST, because a rollback that
	// stops at its first failure would abandon b.md and c.md at destination
	// keys nothing names. Failing the last one instead proves nothing: every
	// ordering has already deleted the rest by then.
	flaky.failPutContaining = "shared/docs/d.md"
	flaky.failDeleteContaining = "shared/docs/a.md"

	_, err := kb.Move(ctx, testOrg, testTeam,
		Ref{Root: RootPrivate, Path: "docs"}, Ref{Root: RootShared, Path: "docs"})
	if err == nil {
		t.Fatal("expected the move to fail")
	}

	// Every copy the move made is gone except the one whose delete was refused,
	// and the error names that one so an operator can clean it up.
	left, listErr := kb.List(ctx, testOrg, testTeam, []Root{RootShared}, "")
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if !equalStrings(refStrings(left), []string{"shared/docs/a.md"}) {
		t.Fatalf("after the rollback the destination holds %v; want only the entry whose delete was refused", refStrings(left))
	}
	if !strings.Contains(err.Error(), "a.md") {
		t.Errorf("the error does not name the stranded entry: %v", err)
	}

	// And the sources are untouched: the knowledge is still readable where it was.
	src, _ := kb.List(ctx, testOrg, testTeam, []Root{RootPrivate}, "")
	if len(src) != 4 {
		t.Errorf("sources = %v; a refused move must not remove any", refStrings(src))
	}
}

// flakyStorage fails one Put and one Delete, chosen by key substring, so a
// rollback path can be driven without a real object store.
type flakyStorage struct {
	storage.Storage
	failPutContaining    string
	failDeleteContaining string
}

func (f *flakyStorage) Put(ctx context.Context, key string, r io.Reader) error {
	if f.failPutContaining != "" && strings.Contains(key, f.failPutContaining) {
		return errors.New("object store refused the write")
	}
	return f.Storage.Put(ctx, key, r)
}

func (f *flakyStorage) Delete(ctx context.Context, key string) error {
	if f.failDeleteContaining != "" && strings.Contains(key, f.failDeleteContaining) {
		return errors.New("object store refused the delete")
	}
	return f.Storage.Delete(ctx, key)
}

func TestValidatePath(t *testing.T) {
	ok := []string{"a.md", "notes/a.md", "a/b/c/d.md", "Design Doc.md", "重要.md"}
	for _, p := range ok {
		if err := ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) = %v; want nil", p, err)
		}
	}
	bad := []string{
		"",                               // empty
		"/a.md",                          // leading separator
		"a.md/",                          // trailing separator
		"a//b.md",                        // doubled separator
		"../a.md",                        // traversal
		"a/../../b.md",                   // traversal mid-path
		".hidden.md",                     // dot file
		"notes/.hidden/a.md",             // dot folder
		"a\\b.md",                        // OS separator inside a segment
		"a\x00b.md",                      // control character
		strings.Repeat("x", 300) + ".md", // segment cap
		strings.Repeat("a/", MaxDepth) + "deep.md",                   // depth cap
		strings.Repeat(strings.Repeat("d", 100)+"/", 15) + "leaf.md", // path cap
	}
	for _, p := range bad {
		if err := ValidatePath(p); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidatePath(%q) = %v; want ErrInvalidName", p, err)
		}
	}
	// The empty prefix is the legitimate "everything under this root".
	if err := ValidatePrefix(""); err != nil {
		t.Errorf("ValidatePrefix(\"\") = %v; want nil", err)
	}
}

func TestParseRoot(t *testing.T) {
	for _, in := range []string{"private", "shared"} {
		if got, err := ParseRoot(in); err != nil || string(got) != in {
			t.Errorf("ParseRoot(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "Private", "SHARED", "public", "kb", "private/"} {
		if _, err := ParseRoot(in); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ParseRoot(%q) = %v; want ErrInvalidName", in, err)
		}
	}
}

// TestNew_PicksTheBackendForTheMode pins the mode branch: a local deployment
// gets plain files under its own state root and never constructs an object
// backend, and a multi one gets the shared object seam.
func TestNew_PicksTheBackendForTheMode(t *testing.T) {
	stateRoot := t.TempDir()
	paths.SetForTest(t, stateRoot)

	runmode.SetForTest(t, runmode.ModeLocal)
	local := New(nil) // a local deployment has no blob store to hand in
	put(t, local, testTeam, RootPrivate, "a.md", "local")
	if _, err := os.Stat(filepath.Join(stateRoot, "teams", testTeam, "kb", "private", "a.md")); err != nil {
		t.Fatalf("local mode should write under the state root: %v", err)
	}

	blobRoot := t.TempDir()
	runmode.SetForTest(t, runmode.ModeMulti)
	multi := New(storage.NewFS(blobRoot))
	put(t, multi, testTeam, RootPrivate, "b.md", "multi")
	if _, err := os.Stat(filepath.Join(blobRoot, testOrg, "teams", testTeam, "kb", "private", "b.md")); err != nil {
		t.Fatalf("multi mode should write through the blob store: %v", err)
	}
}

// --- helpers ------------------------------------------------------------

func put(t *testing.T, kb KB, teamID string, root Root, path, body string) {
	t.Helper()
	if err := kb.Put(context.Background(), testOrg, teamID, Ref{Root: root, Path: path}, strings.NewReader(body)); err != nil {
		t.Fatalf("Put %s/%s: %v", root, path, err)
	}
}

func read(t *testing.T, kb KB, teamID string, ref Ref) string {
	t.Helper()
	rc, err := kb.Open(context.Background(), testOrg, teamID, ref)
	if err != nil {
		t.Fatalf("Open %s: %v", ref, err)
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rc); err != nil {
		t.Fatalf("read %s: %v", ref, err)
	}
	return buf.String()
}

func refStrings(entries []FileInfo) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Ref().String()
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
