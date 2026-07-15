package kbstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

func newTestStore(t *testing.T) (*Store, storage.Storage) {
	t.Helper()
	blobs := storage.NewFS(t.TempDir())
	return New(blobs), blobs
}

func TestValidateName(t *testing.T) {
	ok := []string{"notes.md", "diagram.png", "a b.txt", "UPPER.MD", "v1.2.3.json"}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v; want nil", n, err)
		}
	}
	bad := []string{"", "   ", ".", "..", "sub/deep.md", "a/b", ".hidden", "..\\x"}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil; want rejection", n)
		}
	}
}

func TestKeyBuilding_RejectsBadNames(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	// A bad name must fail before any blob is written.
	if err := st.Put(ctx, "org1", "proj1", "sub/deep.md", strings.NewReader("x")); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("Put(nested name) = %v; want ErrInvalidName", err)
	}
	if _, err := st.Get(ctx, "org1", "proj1", ".."); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("Get(..) = %v; want ErrInvalidName", err)
	}
}

func TestKeyConvention(t *testing.T) {
	st, blobs := newTestStore(t)
	ctx := context.Background()
	if err := st.Put(ctx, "orgA", "projB", "note.md", strings.NewReader("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The blob must land at exactly <orgID>/projects/<projectID>/kb/<filename>.
	objs, err := blobs.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("expected 1 blob, got %d: %+v", len(objs), objs)
	}
	if want := "orgA/projects/projB/kb/note.md"; objs[0].Key != want {
		t.Fatalf("key = %q; want %q", objs[0].Key, want)
	}
}

func TestRoundTrip_List_Get_Delete(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	const org, proj = "o", "p"
	files := map[string]string{
		"a.md":   "alpha",
		"b.txt":  "beta",
		"c.json": "{}",
	}
	for name, body := range files {
		if err := st.Put(ctx, org, proj, name, strings.NewReader(body)); err != nil {
			t.Fatalf("Put %q: %v", name, err)
		}
	}
	list, err := st.List(ctx, org, proj)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != len(files) {
		t.Fatalf("List returned %d; want %d", len(list), len(files))
	}
	names := make([]string, 0, len(list))
	for _, f := range list {
		names = append(names, f.Name)
		if int(f.Size) != len(files[f.Name]) {
			t.Errorf("List %q size = %d; want %d", f.Name, f.Size, len(files[f.Name]))
		}
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "a.md,b.txt,c.json" {
		t.Fatalf("names = %q", got)
	}
	// Get round-trips content.
	rc, err := st.Get(ctx, org, proj, "a.md")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "alpha" {
		t.Fatalf("Get a.md = %q; want alpha", b)
	}
	// Delete removes one; List reflects it.
	if err := st.Delete(ctx, org, proj, "b.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list, _ = st.List(ctx, org, proj)
	if len(list) != 2 {
		t.Fatalf("after delete List = %d; want 2", len(list))
	}
}

func TestList_IgnoresSiblingProjectAndNested(t *testing.T) {
	st, blobs := newTestStore(t)
	ctx := context.Background()
	// A sibling project whose id is a prefix of ours must not leak in.
	if err := st.Put(ctx, "o", "p", "mine.md", strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := st.Put(ctx, "o", "p2", "theirs.md", strings.NewReader("y")); err != nil {
		t.Fatalf("Put sibling: %v", err)
	}
	// A nested key written directly (bypassing kbstore) must be filtered out.
	if err := blobs.Put(ctx, "o/projects/p/kb/sub/deep.bin", strings.NewReader("z")); err != nil {
		t.Fatalf("Put nested: %v", err)
	}
	list, err := st.List(ctx, "o", "p")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "mine.md" {
		t.Fatalf("List = %+v; want only mine.md", list)
	}
}

func TestExists(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	ok, err := st.Exists(ctx, "o", "p", "x.md")
	if err != nil || ok {
		t.Fatalf("Exists(absent) = %v,%v; want false,nil", ok, err)
	}
	if err := st.Put(ctx, "o", "p", "x.md", strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ok, err = st.Exists(ctx, "o", "p", "x.md")
	if err != nil || !ok {
		t.Fatalf("Exists(present) = %v,%v; want true,nil", ok, err)
	}
}

func TestDeletePrefix(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	for _, n := range []string{"a.md", "b.md", "c.md"} {
		if err := st.Put(ctx, "o", "p", n, strings.NewReader("x")); err != nil {
			t.Fatalf("Put %q: %v", n, err)
		}
	}
	// A sibling project must survive the prefix delete.
	if err := st.Put(ctx, "o", "other", "keep.md", strings.NewReader("k")); err != nil {
		t.Fatalf("Put sibling: %v", err)
	}
	if err := st.DeletePrefix(ctx, "o", "p"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if list, _ := st.List(ctx, "o", "p"); len(list) != 0 {
		t.Fatalf("after DeletePrefix List = %d; want 0", len(list))
	}
	if list, _ := st.List(ctx, "o", "other"); len(list) != 1 {
		t.Fatalf("sibling project List = %d; want 1", len(list))
	}
}

func TestGetRange(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	body := "0123456789"
	if err := st.Put(ctx, "o", "p", "r.bin", strings.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := st.GetRange(ctx, "o", "p", "r.bin", 2, 3)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "234" {
		t.Fatalf("GetRange(2,3) = %q; want 234", b)
	}
}

func TestDiffManifest(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	const org, proj = "o", "p"

	// Store has: same.md (identical), stale.md (bigger locally), remoteonly.md.
	if err := st.Put(ctx, org, proj, "same.md", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, org, proj, "stale.md", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, org, proj, "remoteonly.md", strings.NewReader("R")); err != nil {
		t.Fatal(err)
	}

	// Disk has: same.md (identical), stale.md (different size), localonly.md,
	// and a dotfile + a subdir that must be ignored.
	dir := t.TempDir()
	writeFile(t, dir, "same.md", "hello")
	writeFile(t, dir, "stale.md", "much longer content")
	writeFile(t, dir, "localonly.md", "L")
	writeFile(t, dir, ".scratch", "ignore me")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	d, err := st.DiffManifest(ctx, org, proj, dir)
	if err != nil {
		t.Fatalf("DiffManifest: %v", err)
	}
	// Pull: remoteonly.md (missing locally) + stale.md (size differs).
	assertSet(t, "Pull", fileInfoNames(d.Pull), "remoteonly.md", "stale.md")
	// RemoveLocal: localonly.md (no remote counterpart).
	assertSet(t, "RemoveLocal", d.RemoveLocal, "localonly.md")
	// Push: localonly.md (missing remotely) + stale.md (size differs).
	assertSet(t, "Push", d.Push, "localonly.md", "stale.md")
	// RemoveRemote: remoteonly.md (no local counterpart).
	assertSet(t, "RemoveRemote", d.RemoveRemote, "remoteonly.md")
}

func TestDiffManifest_MissingLocalDir(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	if err := st.Put(ctx, "o", "p", "a.md", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	d, err := st.DiffManifest(ctx, "o", "p", filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("DiffManifest(missing dir): %v", err)
	}
	// Everything in the store is pull/remove-remote; nothing to push/remove-local.
	assertSet(t, "Pull", fileInfoNames(d.Pull), "a.md")
	assertSet(t, "RemoveRemote", d.RemoveRemote, "a.md")
	if len(d.Push) != 0 || len(d.RemoveLocal) != 0 {
		t.Fatalf("Push=%v RemoveLocal=%v; want empty", d.Push, d.RemoveLocal)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileInfoNames(fs []FileInfo) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}

func assertSet(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if strings.Join(g, ",") != strings.Join(w, ",") {
		t.Errorf("%s = %v; want %v", label, g, w)
	}
}
