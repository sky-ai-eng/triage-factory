package curator

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// countingStorage wraps a Storage and counts Put/Delete calls so a test can
// assert the syncer's dedup actually suppresses redundant uploads.
type countingStorage struct {
	inner storage.Storage
	puts  atomic.Int64
	dels  atomic.Int64
}

func (c *countingStorage) Put(ctx context.Context, key string, r io.Reader) error {
	c.puts.Add(1)
	return c.inner.Put(ctx, key, r)
}
func (c *countingStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return c.inner.Get(ctx, key)
}
func (c *countingStorage) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	return c.inner.GetRange(ctx, key, off, length)
}
func (c *countingStorage) Delete(ctx context.Context, key string) error {
	c.dels.Add(1)
	return c.inner.Delete(ctx, key)
}
func (c *countingStorage) Exists(ctx context.Context, key string) (bool, error) {
	return c.inner.Exists(ctx, key)
}
func (c *countingStorage) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	return c.inner.List(ctx, prefix)
}

const (
	syncTestOrg  = "org-sync"
	syncTestProj = "proj-sync"
)

// setupSyncer pins runmode + state-root paths (so KnowledgeDir resolves under
// the watched root), creates the project's knowledge-base dir, and starts a
// KBSyncer. Returns the syncer, the counting store wrapper, the recording hub,
// and the on-disk knowledge-base dir the test writes into.
func setupSyncer(t *testing.T) (*KBSyncer, *countingStorage, *recordingHub, string) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	stateRoot := t.TempDir()
	paths.SetForTest(t, stateRoot)

	cs := &countingStorage{inner: storage.NewFS(t.TempDir())}
	kb := kbstore.New(cs)
	rec := &recordingHub{}

	kbDir, err := KnowledgeDir(syncTestOrg, syncTestProj)
	if err != nil {
		t.Fatalf("resolve kb dir: %v", err)
	}
	kbDir = filepath.Join(kbDir, kbDirName)
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}

	root, err := ProjectsWatchRoot()
	if err != nil {
		t.Fatalf("watch root: %v", err)
	}
	resolve := func(pid string) string {
		if pid == syncTestProj {
			return syncTestOrg
		}
		return ""
	}
	s, err := NewKBSyncer(kb, rec, root, resolve)
	if err != nil {
		t.Fatalf("new syncer: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, cs, rec, kbDir
}

// waitForStoreNames polls the store until exactly the wanted flat names are
// present for the sync test project, or the deadline fires.
func waitForStoreNames(t *testing.T, kb *kbstore.Store, want ...string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list, err := kb.List(context.Background(), syncTestOrg, syncTestProj)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		got := map[string]bool{}
		for _, f := range list {
			got[f.Name] = true
		}
		if len(got) == len(want) {
			ok := true
			for _, n := range want {
				if !got[n] {
					ok = false
					break
				}
			}
			if ok {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	list, _ := kb.List(context.Background(), syncTestOrg, syncTestProj)
	t.Fatalf("timed out waiting for store names %v; got %+v", want, list)
}

func TestKBSyncer_UploadsOnWrite(t *testing.T) {
	_, cs, rec, kbDir := setupSyncer(t)
	kb := kbstore.New(cs)

	if err := os.WriteFile(filepath.Join(kbDir, "note.md"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitForStoreNames(t, kb, "note.md")

	rc, err := kb.Get(context.Background(), syncTestOrg, syncTestProj, "note.md")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("stored content = %q; want hello", b)
	}
	// The batch broadcasts at least a pending event and a completion event.
	waitForEvent(t, rec, syncTestProj, 1)
}

func TestKBSyncer_DeletePropagates(t *testing.T) {
	_, cs, _, kbDir := setupSyncer(t)
	kb := kbstore.New(cs)

	path := filepath.Join(kbDir, "doomed.md")
	if err := os.WriteFile(path, []byte("bye"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitForStoreNames(t, kb, "doomed.md")

	if err := os.Remove(path); err != nil {
		t.Fatalf("rm: %v", err)
	}
	// Store should end up empty.
	waitForStoreNames(t, kb)
}

func TestKBSyncer_ManifestDedup(t *testing.T) {
	_, cs, _, kbDir := setupSyncer(t)
	kb := kbstore.New(cs)

	path := filepath.Join(kbDir, "same.md")
	if err := os.WriteFile(path, []byte("identical bytes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitForStoreNames(t, kb, "same.md")
	firstPuts := cs.puts.Load()
	if firstPuts < 1 {
		t.Fatalf("expected at least one Put, got %d", firstPuts)
	}

	// Rewrite the SAME content — an fs event fires, but the content hash
	// matches the manifest, so the syncer must not re-upload.
	time.Sleep(30 * time.Millisecond)
	if err := os.WriteFile(path, []byte("identical bytes"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	// Give the debounce + a batch a chance to run.
	time.Sleep(400 * time.Millisecond)
	if got := cs.puts.Load(); got != firstPuts {
		t.Fatalf("identical rewrite triggered %d extra Put(s) (total %d); want no re-upload", got-firstPuts, got)
	}

	// A real content change DOES re-upload.
	if err := os.WriteFile(path, []byte("different content now"), 0o644); err != nil {
		t.Fatalf("change: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && cs.puts.Load() == firstPuts {
		time.Sleep(20 * time.Millisecond)
	}
	if cs.puts.Load() <= firstPuts {
		t.Fatalf("changed content did not re-upload (puts still %d)", cs.puts.Load())
	}
}

// waitForStoreCount polls until the store holds exactly n files for the sync
// test project.
func waitForStoreCount(t *testing.T, kb *kbstore.Store, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, err := kb.List(context.Background(), syncTestOrg, syncTestProj)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) == n {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	list, _ := kb.List(context.Background(), syncTestOrg, syncTestProj)
	t.Fatalf("timed out waiting for %d store files; got %d", n, len(list))
}

// TestKBSyncer_OverflowFullReconcile drives more changed paths than the
// pending cap through observe(), forcing the batch to fall back to a full
// directory reconcile — every on-disk file must still land in the store.
func TestKBSyncer_OverflowFullReconcile(t *testing.T) {
	s, cs, _, kbDir := setupSyncer(t)
	kb := kbstore.New(cs)

	const n = kbPendingCap + 12
	for i := 0; i < n; i++ {
		name := fileName(i)
		if err := os.WriteFile(filepath.Join(kbDir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		// Drive observe directly so the pending set is guaranteed to exceed the
		// cap in one window, deterministically tripping the overflow path.
		s.observe(syncTestProj, name, filepath.Join(kbDir, name))
	}
	waitForStoreCount(t, kb, n)
}

func fileName(i int) string {
	// zero-padded so names sort stably; content == name keeps sizes distinct.
	return "f" + pad3(i) + ".md"
}

func pad3(i int) string {
	s := []byte("000")
	for p := 2; p >= 0 && i > 0; p-- {
		s[p] = byte('0' + i%10)
		i /= 10
	}
	return string(s)
}

func TestMaterializeProjectKB(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	kb := kbstore.New(storage.NewFS(t.TempDir()))
	ctx := context.Background()
	const org, proj = "o", "p"

	// Store has a.md (missing locally) and stale.md (different size locally).
	if err := kb.Put(ctx, org, proj, "a.md", strings.NewReader("alpha")); err != nil {
		t.Fatal(err)
	}
	if err := kb.Put(ctx, org, proj, "stale.md", strings.NewReader("new-longer-content")); err != nil {
		t.Fatal(err)
	}

	kbDir := t.TempDir()
	// Disk has stale.md (short) and localonly.md (absent from store → removed).
	if err := os.WriteFile(filepath.Join(kbDir, "stale.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "localonly.md"), []byte("gone"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := materializeProjectKB(ctx, kb, org, proj, kbDir); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	if b, err := os.ReadFile(filepath.Join(kbDir, "a.md")); err != nil || string(b) != "alpha" {
		t.Fatalf("a.md = %q, %v; want alpha", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(kbDir, "stale.md")); err != nil || string(b) != "new-longer-content" {
		t.Fatalf("stale.md = %q, %v; want new-longer-content", b, err)
	}
	if _, err := os.Stat(filepath.Join(kbDir, "localonly.md")); !os.IsNotExist(err) {
		t.Fatalf("localonly.md still present; want removed (err=%v)", err)
	}
}

func TestReconcileProjectKB(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	kb := kbstore.New(storage.NewFS(t.TempDir()))
	ctx := context.Background()
	const org, proj = "o", "p"

	// Store has remoteonly.md (should be deleted) and stale.md (short).
	if err := kb.Put(ctx, org, proj, "remoteonly.md", strings.NewReader("R")); err != nil {
		t.Fatal(err)
	}
	if err := kb.Put(ctx, org, proj, "stale.md", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}

	kbDir := t.TempDir()
	// Disk has new.md (push) and stale.md (bigger → push).
	if err := os.WriteFile(filepath.Join(kbDir, "new.md"), []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "stale.md"), []byte("much-longer-now"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := reconcileProjectKB(ctx, kb, org, proj, kbDir); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	list, err := kb.List(ctx, org, proj)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]int64{}
	for _, f := range list {
		got[f.Name] = f.Size
	}
	if got["new.md"] != int64(len("fresh")) {
		t.Errorf("new.md size = %d; want %d", got["new.md"], len("fresh"))
	}
	if got["stale.md"] != int64(len("much-longer-now")) {
		t.Errorf("stale.md size = %d; want %d", got["stale.md"], len("much-longer-now"))
	}
	// Push-only: a store file with no disk counterpart is preserved (it may be
	// a panel upload not yet materialized here), never deleted by the backstop.
	if _, ok := got["remoteonly.md"]; !ok {
		t.Errorf("remoteonly.md removed by reconcile; push-only reconcile must preserve store-only files")
	}
}
