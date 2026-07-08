package curator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// KnowledgeWatcher emits `project_knowledge_updated` websocket events
// the moment a file under a project's knowledge-base/ directory is
// created, modified, removed, or renamed — so the frontend Knowledge
// panel refreshes mid-turn as the curator agent writes notes.
//
// The watcher is mode-aware because the curator's on-disk state moved
// under a per-org subtree in multi mode. Two layouts, one
// mechanism:
//
//	local:  <root>/<projectID>/knowledge-base/...
//	multi:  <root>/orgs/<orgID>/projects/<projectID>/knowledge-base/...
//
// The variable middle (orgs/<orgID>/projects) is the watcher's `prefix`
// — the literal directory segments between the watch root and the
// project-id dir, with a wildcard ("") element for the per-tenant org
// id. Local mode has an empty prefix (project dirs are direct children
// of the root), so its behavior is byte-for-byte what it was before.
// ProjectsWatchRoot picks the paired root for each mode.
//
// fsnotify watches are non-recursive on every supported platform, so we
// maintain the inner watches ourselves: starting at the root we descend
// only along the prefix path (never into siblings like repos/ or the
// bare-clone cache), watch each project dir, and watch its
// knowledge-base subdir. On a Create one level up we add the
// corresponding watch and (for the kb-dir-creation case) fire so the
// frontend renders whatever lands inside.
//
// Per-project debouncing collapses the burst of fs events a single
// agent Write tends to fire (Write often emits Create + Write + Chmod
// in quick succession) into a single broadcast.
const (
	kbDirName         = "knowledge-base"
	knowledgeDebounce = 100 * time.Millisecond
)

// ProjectsWatchRoot returns the directory the KnowledgeWatcher should
// root at so it observes every project's knowledge-base, in either mode.
// It is the producer-side companion to watchPrefix: this picks the root,
// watchPrefix picks the segments between that root and the project-id
// dir, and the two must agree, so they live together.
//
//   - local: the single projects dir (paths.ProjectsRoot for the
//     local-default org) — project dirs are its direct children, exactly
//     as before the per-org subtree move.
//   - multi: the state root itself — projects live under the per-org
//     subtree <root>/orgs/<orgID>/projects/<id>, so the watcher roots
//     above the org segment and descends through orgs/*/projects.
//
// It pre-flights paths.StateRootErr so a missing $HOME surfaces as an
// actionable error rather than a panic from the underlying resolver.
func ProjectsWatchRoot() (string, error) {
	if _, err := paths.StateRootErr(); err != nil {
		return "", fmt.Errorf("resolve state root: %w", err)
	}
	if runmode.Current() == runmode.ModeMulti {
		return paths.StateRoot(), nil
	}
	return paths.ProjectsRoot(runmode.LocalDefaultOrgID), nil
}

// watchPrefix returns the literal directory segments between the watch
// root and a project-id dir, for the current run mode. An empty-string
// element is a wildcard that matches any single segment (the per-tenant
// org id). See ProjectsWatchRoot for the paired root.
//
//   - local: nil — project dirs are direct children of the root.
//   - multi: {"orgs", "", "projects"} — <root>/orgs/<orgID>/projects/<id>.
func watchPrefix() []string {
	if runmode.Current() == runmode.ModeMulti {
		return []string{"orgs", "", "projects"}
	}
	return nil
}

// Broadcaster is the subset of *websocket.Hub behavior the watcher needs.
// Stating it as an interface lets tests substitute a recorder without
// wiring a real http server + ws client.
type Broadcaster interface {
	Broadcast(websocket.Event)
}

// ProjectOrgResolver maps a project id to its owning org id. The
// kbwatcher uses this to stamp the per-connection routing org on
// each project_knowledge_updated event so the hub's fanout filter
// keeps cross-tenant noise off.
//
// Return value semantics:
//
//   - non-empty orgID: normal case, broadcast with that scope.
//   - empty string: project couldn't be resolved (stale dir, lookup
//     error). The watcher DROPS the broadcast rather than falling
//     back to system-wide — an empty OrgID on the wire would reach
//     every connected tenant's clients, leaking the update across
//     the tenancy boundary. Resolver implementations should log on
//     their side and return ""; the watcher does not log on drop
//     (the resolver is closer to the failure).
//
// A nil ProjectOrgResolver is still tolerated for tests and pre-
// wiring; in that case the watcher broadcasts with OrgID="" and the
// hub delivers system-wide — that's fine for tests but not for
// production, which always wires a real resolver.
type ProjectOrgResolver func(projectID string) string

type KnowledgeWatcher struct {
	watcher       *fsnotify.Watcher
	hub           Broadcaster
	root          string
	prefix        []string
	resolveOrgFor ProjectOrgResolver

	mu       sync.Mutex
	debounce map[string]*time.Timer
}

// NewKnowledgeWatcher constructs a watcher rooted at root and starts the
// event loop. The root is created if missing — fresh installs haven't
// run a curator turn yet, so the dir genuinely may not exist. The
// per-mode prefix (see watchPrefix) is captured at construction; pass
// the matching root (see ProjectsWatchRoot).
//
// resolveOrgFor maps a project id (the directory name at the project
// level of the watched tree) to its owning org so the broadcast can be
// scoped to that tenant's clients. Nil resolver is tolerated (tests,
// pre-wiring) and falls back to empty-org broadcasts that the hub
// delivers to every client.
func NewKnowledgeWatcher(hub Broadcaster, root string, resolveOrgFor ProjectOrgResolver) (*KnowledgeWatcher, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("ensure watch root: %w", err)
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fsnotify: %w", err)
	}
	kw := &KnowledgeWatcher{
		watcher:       w,
		hub:           hub,
		root:          root,
		prefix:        watchPrefix(),
		resolveOrgFor: resolveOrgFor,
		debounce:      make(map[string]*time.Timer),
	}
	// Watching the root is required — without it the watcher can never
	// learn about the dirs below it, so a failure here is fatal.
	if err := w.Add(root); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("watch root %q: %w", root, err)
	}
	// Seed watches for everything already on disk. Failures on individual
	// subdirs log + continue inside descendChildren — a permission glitch
	// on one project shouldn't take the watcher down for all.
	kw.descendChildren(root, 0, false)
	go kw.run()
	return kw, nil
}

// projectDepth is the number of path segments between the watch root and
// a project-id dir: 0 in local mode (direct children), len(prefix)
// otherwise. The knowledge-base dir sits one level deeper.
func (kw *KnowledgeWatcher) projectDepth() int { return len(kw.prefix) }

// onPrefixPath reports whether parts' leading segments are consistent
// with the watch prefix — i.e. parts could be (or live under) a project
// dir rather than a sibling tree like repos/ or the bare-clone cache.
// Wildcard prefix elements ("") match any segment; segments beyond what
// parts provides are treated as not-yet-present (still on-path).
func (kw *KnowledgeWatcher) onPrefixPath(parts []string) bool {
	n := min(len(parts), len(kw.prefix))
	for i := 0; i < n; i++ {
		if seg := kw.prefix[i]; seg != "" && parts[i] != seg {
			return false
		}
	}
	return true
}

// knowledgeProjectID returns the project id for an event whose path is a
// project's knowledge-base dir or something inside it, and ok=false for
// anything else (a project dir on its own, a sibling like repos/, an off-
// prefix path). The project id is the segment immediately above
// knowledge-base.
func (kw *KnowledgeWatcher) knowledgeProjectID(parts []string) (string, bool) {
	pd := kw.projectDepth()
	kb := pd + 1 // knowledge-base segment index
	if len(parts) <= kb {
		return "", false
	}
	if parts[kb] != kbDirName {
		return "", false
	}
	if !kw.onPrefixPath(parts) {
		return "", false
	}
	return parts[pd], true
}

func (kw *KnowledgeWatcher) run() {
	for {
		select {
		case event, ok := <-kw.watcher.Events:
			if !ok {
				return
			}
			kw.handle(event)
		case err, ok := <-kw.watcher.Errors:
			if !ok {
				return
			}
			kbwatcherLog.Warn("watcher error", "error", err)
		}
	}
}

// handle dispatches a single fsnotify event in two passes:
//
//  1. Watch management — on a directory Create, descend the newly-created
//     dir if it's on the path to (or is) a project dir, so future events
//     inside it reach us. fsnotify is non-recursive, so we install these
//     inner watches ourselves.
//  2. Fire — on any change (create, write, remove, rename) under a
//     project's knowledge-base subtree, broadcast (debounced).
//
// fsnotify also delivers Remove events when watched dirs disappear; the
// watcher auto-removes its handle in that case, so we don't have to
// explicitly Unwatch on project deletion.
func (kw *KnowledgeWatcher) handle(event fsnotify.Event) {
	rel, err := filepath.Rel(kw.root, event.Name)
	if err != nil {
		return
	}
	parts := strings.Split(rel, string(filepath.Separator))

	if event.Op&fsnotify.Create != 0 {
		if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
			kw.handleDirCreated(parts, event.Name)
		}
	}

	if projectID, ok := kw.knowledgeProjectID(parts); ok {
		kw.fire(projectID)
	}
}

// handleDirCreated installs watches for a directory that just appeared,
// if it lies on the prefix path to a project's knowledge-base. The fire
// for knowledge-base activity is handled separately by handle's
// knowledgeProjectID branch (so a kb dir created on its own still emits);
// watchTree's fireExisting=true covers the atomic case where a project
// dir is laid down with its knowledge-base already inside (project import
// + a fresh project's first turn both MkdirAll the whole tree in one
// shot, so the inner Create events fire into watches we hadn't installed
// yet).
func (kw *KnowledgeWatcher) handleDirCreated(parts []string, full string) {
	if !kw.onPrefixPath(parts) {
		return
	}
	pd := kw.projectDepth()
	idx := len(parts) - 1 // index of the created dir
	switch {
	case idx <= pd:
		// An intermediate dir on the prefix path (orgs, orgs/<org>, …) or
		// a project dir. Watch it and descend; for an atomically-created
		// tree this reaches the knowledge-base and fires.
		kw.watchTree(full, idx+1, true)
	case idx == pd+1 && parts[idx] == kbDirName:
		// knowledge-base dir created on its own. Watch it so file changes
		// inside reach us; handle's fire branch emits for this create.
		if err := kw.watcher.Add(full); err != nil {
			kbwatcherLog.Warn("watch new kb dir failed", "dir", full, "error", err)
		}
	}
}

// watchTree adds a watch on dir and then descends. depth is the part-
// index that dir's immediate children occupy (root's children are
// depth 0). fireExisting, when true, emits for any knowledge-base dir
// found already populated during the descent — used for runtime
// atomic-create detection, left false during startup seeding so existing
// content doesn't spuriously fire on boot.
func (kw *KnowledgeWatcher) watchTree(dir string, depth int, fireExisting bool) {
	if err := kw.watcher.Add(dir); err != nil {
		kbwatcherLog.Warn("watch dir failed", "dir", dir, "error", err)
		return
	}
	kw.descendChildren(dir, depth, fireExisting)
}

// descendChildren walks the immediate children of an already-watched
// dir, installing inner watches along the prefix → project → kb path and
// nowhere else. depth is the index dir's children occupy. It stops at the
// knowledge-base dir (depth pd+1): the kb dir itself is watched so file
// changes fire, but we never descend into its subtree (parity with
// prior behavior, and it keeps repos/ + nested scratch off the
// watch set).
func (kw *KnowledgeWatcher) descendChildren(dir string, depth int, fireExisting bool) {
	pd := kw.projectDepth()
	if depth > pd+1 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		kbwatcherLog.Warn("read dir failed", "dir", dir, "error", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		child := filepath.Join(dir, e.Name())
		switch {
		case depth < pd:
			// Intermediate prefix level — descend only along the prefix
			// (so orgs/<org>/repos and other siblings stay unwatched).
			if seg := kw.prefix[depth]; seg == "" || seg == e.Name() {
				kw.watchTree(child, depth+1, fireExisting)
			}
		case depth == pd:
			// Children are project dirs.
			kw.watchTree(child, depth+1, fireExisting)
		case depth == pd+1:
			// Children of a project dir — only knowledge-base is watched.
			if e.Name() == kbDirName {
				if err := kw.watcher.Add(child); err != nil {
					kbwatcherLog.Warn("watch dir failed", "dir", child, "error", err)
					continue
				}
				if fireExisting {
					kw.fire(filepath.Base(dir)) // dir is the project dir
				}
			}
		}
	}
}

// fire schedules a debounced broadcast for projectID. Repeated calls
// within `knowledgeDebounce` collapse to a single emission, taking the
// trailing edge of the burst.
func (kw *KnowledgeWatcher) fire(projectID string) {
	kw.mu.Lock()
	defer kw.mu.Unlock()
	if t, ok := kw.debounce[projectID]; ok {
		t.Stop()
	}
	kw.debounce[projectID] = time.AfterFunc(knowledgeDebounce, func() {
		kw.mu.Lock()
		delete(kw.debounce, projectID)
		kw.mu.Unlock()
		if kw.hub == nil {
			return
		}
		var orgID string
		if kw.resolveOrgFor != nil {
			orgID = kw.resolveOrgFor(projectID)
			if orgID == "" {
				// Resolver couldn't map this project (stale dir,
				// lookup error). Drop rather than broadcast with
				// OrgID="" which the hub would fan out to every
				// tenant, leaking the update cross-tenancy.
				return
			}
		}
		kw.hub.Broadcast(websocket.Event{
			Type:      "project_knowledge_updated",
			OrgID:     orgID,
			ProjectID: projectID,
		})
	})
}

// Close stops the watcher. Pending debounce timers are allowed to
// fire — they broadcast onto the hub which is owned upstream and
// will outlive this watcher; broadcasting to a still-valid hub is
// harmless.
func (kw *KnowledgeWatcher) Close() error {
	return kw.watcher.Close()
}
