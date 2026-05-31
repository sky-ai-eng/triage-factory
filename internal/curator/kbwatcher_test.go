package curator

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// recordingHub captures every Broadcast call for assertion. Satisfies
// the package-local Broadcaster interface so we can swap in for a
// real *websocket.Hub without standing up an http test server.
type recordingHub struct {
	mu     sync.Mutex
	events []websocket.Event
}

func (r *recordingHub) Broadcast(e websocket.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingHub) snapshot() []websocket.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]websocket.Event, len(r.events))
	copy(out, r.events)
	return out
}

// waitForEvent polls the recorder until at least `n` events with the
// matching project_id appear, or the timeout fires. We give fsnotify
// generous wallclock — macOS FSEvents has occasional 50–200ms latency
// even on a quiet filesystem, plus the watcher's own debounce window.
func waitForEvent(t *testing.T, rec *recordingHub, projectID string, n int) []websocket.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var matches []websocket.Event
		for _, e := range rec.snapshot() {
			if e.Type == "project_knowledge_updated" && e.ProjectID == projectID {
				matches = append(matches, e)
			}
		}
		if len(matches) >= n {
			return matches
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d project_knowledge_updated event(s) for %q; got %+v",
		n, projectID, rec.snapshot())
	return nil
}

func TestKnowledgeWatcher_FiresOnFileWriteInExistingKB(t *testing.T) {
	root := t.TempDir()
	projectID := "abc123"
	kbDir := filepath.Join(root, projectID, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}

	rec := &recordingHub{}
	w, err := NewKnowledgeWatcher(rec, root, nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	defer w.Close()

	if err := os.WriteFile(filepath.Join(kbDir, "note.md"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	events := waitForEvent(t, rec, projectID, 1)
	if len(events) < 1 {
		t.Fatalf("no events captured")
	}
}

func TestKnowledgeWatcher_FiresOnNewProjectAndKBCreatedAtRuntime(t *testing.T) {
	// The interesting case: the project dir + knowledge-base dir
	// don't exist when the watcher starts. They're created mid-run
	// (which is what happens for a brand-new project's first turn).
	// The watcher must add inner watches lazily and still emit.
	root := t.TempDir()
	rec := &recordingHub{}
	w, err := NewKnowledgeWatcher(rec, root, nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	defer w.Close()

	projectID := "fresh-project"
	projectDir := filepath.Join(root, projectID)
	if err := os.Mkdir(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	// Give the root watcher a beat to register the new project dir
	// as a watched subdir before we create the knowledge-base inside.
	// fsnotify Add is synchronous from our handler's perspective, but
	// the handler runs on its own goroutine.
	time.Sleep(50 * time.Millisecond)

	kbDir := filepath.Join(projectDir, "knowledge-base")
	if err := os.Mkdir(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(kbDir, "first.md"), []byte("note"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Expect at least one event for this project. We may get two
	// (one from kb-dir creation, one from file write) — that's fine,
	// the assertion is "at least one." The frontend refetches on
	// each, idempotent.
	waitForEvent(t, rec, projectID, 1)
}

func TestKnowledgeWatcher_FiresOnDelete(t *testing.T) {
	root := t.TempDir()
	projectID := "delete-test"
	kbDir := filepath.Join(root, projectID, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	notePath := filepath.Join(kbDir, "doomed.md")
	if err := os.WriteFile(notePath, []byte("byebye"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	rec := &recordingHub{}
	w, err := NewKnowledgeWatcher(rec, root, nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	defer w.Close()

	if err := os.Remove(notePath); err != nil {
		t.Fatalf("rm: %v", err)
	}
	waitForEvent(t, rec, projectID, 1)
}

func TestKnowledgeWatcher_DebouncesBurstOfWrites(t *testing.T) {
	// A single agent Write often lands as Create + Write + Chmod in
	// quick succession; the per-project debounce must collapse that
	// burst into one broadcast, not one per fs event.
	//
	// We drive the debounce directly — fire() is the funnel every fs
	// event reaches — rather than writing files and waiting on fsnotify.
	// Going through the filesystem made this timing-sensitive: under a
	// busy scheduler (a full `go test ./...` run with -race) the gap
	// between two *delivered* fsnotify events could exceed
	// knowledgeDebounce, so the burst straddled the window and emitted
	// two events (SKY-384). That coupling — a wall-clock debounce vs.
	// OS-paced event delivery — is what made it flake, and it's
	// orthogonal to the coalescing this test exists to verify. The
	// fs -> handle -> fire path is already covered by
	// TestKnowledgeWatcher_FiresOnFileWriteInExistingKB; a tight
	// synchronous burst of fire() calls leaves exactly one live timer
	// (each call stops the prior), so it collapses to a single broadcast
	// regardless of machine load.
	root := t.TempDir()
	rec := &recordingHub{}
	w, err := NewKnowledgeWatcher(rec, root, nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	defer w.Close()

	projectID := "debounce"
	for range 4 {
		w.fire(projectID)
	}

	// Wait for the lone trailing timer to land, then confirm the burst
	// produced exactly that one broadcast.
	events := waitForEvent(t, rec, projectID, 1)
	if len(events) != 1 {
		t.Errorf("expected the burst to debounce into exactly 1 event, got %d: %+v", len(events), events)
	}
}

func TestKnowledgeWatcher_IgnoresChangesOutsideKnowledgeBase(t *testing.T) {
	// Files written to the project root (not under knowledge-base/)
	// must NOT fire. The current envelope steers the agent away from
	// the root, but if it slips up we don't want to spam the panel
	// with refetches that show nothing new.
	root := t.TempDir()
	projectID := "ignore-root"
	projectDir := filepath.Join(root, projectID)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}

	rec := &recordingHub{}
	w, err := NewKnowledgeWatcher(rec, root, nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	defer w.Close()

	if err := os.WriteFile(filepath.Join(projectDir, "stray.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	time.Sleep(knowledgeDebounce + 100*time.Millisecond)

	for _, e := range rec.snapshot() {
		if e.Type == "project_knowledge_updated" && e.ProjectID == projectID {
			t.Errorf("unexpected event for write outside knowledge-base: %+v", e)
		}
	}
}

func TestKnowledgeWatcher_FiresWhenProjectAndKBImportedAtomically(t *testing.T) {
	// Pin: project import tooling lays down `<root>/<id>/knowledge-base/`
	// + files in one syscall burst. The Create event for `<id>/` arrives
	// at our handler, but the inner Create event for `<id>/knowledge-base/`
	// was emitted before we'd installed a watch on `<id>/`, so it never
	// reaches us. Without this regression the watcher would never fire
	// for an imported project until the user manually edited a file —
	// the Knowledge panel would render empty even though the disk has
	// content.
	root := t.TempDir()
	rec := &recordingHub{}
	w, err := NewKnowledgeWatcher(rec, root, nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	defer w.Close()

	projectID := "imported"
	kbDir := filepath.Join(root, projectID, "knowledge-base")
	// MkdirAll creates both <id>/ and <id>/knowledge-base/ in one
	// shot. By the time our handler reacts to the <id>/ Create event,
	// the inner dir already exists and its own Create event has
	// already been (silently) dropped — there was no watch on <id>/
	// to receive it.
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir all: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "imported.md"), []byte("from import"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	// Without the atomic-detection branch in handle(), we never fire.
	// With it, handle() detects the pre-existing kb dir as it adds the
	// project-dir watch and fires immediately so the panel picks up
	// the imported content.
	waitForEvent(t, rec, projectID, 1)
}
