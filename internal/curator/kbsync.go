package curator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// KBSyncer is the multi-mode executor's knowledge-base synchronizer. It
// replaces the pure-broadcaster KnowledgeWatcher on a pod whose disk actually
// hosts KB files (the home executor): when the curator agent writes, renames,
// or deletes a file under a project's knowledge-base/ directory, the syncer
// mirrors that change into the blob store — the multi-mode KB source of truth
// — and broadcasts project_knowledge_updated so the Knowledge panel refreshes
// live from wherever the browser is connected.
//
// It is built from the same watch mechanics as KnowledgeWatcher (a
// non-recursive fsnotify watch descended along the orgs/<org>/projects/<id>
// prefix), but three properties make it binary-safe for the images and
// videos agents capture from PR testing, not just small markdown:
//
//   - Per-file quiesce: a file whose (size, mtime) is still changing since the
//     last observation re-arms its debounce instead of uploading a partial —
//     a video capture in progress is never streamed half-written.
//   - Tiered change detection: an in-memory per-project manifest (size, mtime,
//     and — only for files under a hash threshold — a content hash) suppresses
//     re-uploading a file the agent rewrote with identical bytes.
//   - Streamed, bounded, capped uploads: each changed file streams through the
//     store's multipart uploader; per-file and per-project size caps skip
//     oversize content with a warning rather than blocking the batch.
//
// The manifest is a cache, never durable state — it is seeded lazily by
// listing the store and rebuilt on demand, so a syncer restart re-derives it.
type KBSyncer struct {
	watcher       *fsnotify.Watcher
	kb            *kbstore.Store
	hub           Broadcaster
	root          string
	prefix        []string
	resolveOrgFor ProjectOrgResolver

	mu       sync.Mutex
	projects map[string]*projectSync
}

// projectSync is one project's in-flight sync state: the trailing-edge
// debounce timer, the accumulated set of changed relative filenames (each
// mapped to the stat observed when it was last seen, for the quiesce check),
// an overflow flag that falls the batch back to a full directory reconcile,
// and the cache manifest.
type projectSync struct {
	timer    *time.Timer
	pending  map[string]statSnap
	overflow bool
	manifest map[string]fileMeta
	seeded   bool
}

// statSnap is a file's (size, mtime) at one observation, plus whether it
// existed — the quiesce discriminator.
type statSnap struct {
	size   int64
	mtime  time.Time
	exists bool
}

// fileMeta is one manifest entry: the last-synced (size, mtime) and, for
// files under the hash threshold, the content hash. A zero mtime with an
// empty hash marks an entry seeded from a store listing (size known, content
// assumed to match because a turn-start materialize just made disk == store).
type fileMeta struct {
	size  int64
	mtime time.Time
	hash  string
}

const (
	// kbPendingCap bounds the per-project changed-path set before the batch
	// falls back to a full-directory reconcile — a burst wider than this
	// (a bulk import, a `git checkout` inside the tree) is cheaper to
	// reconcile whole than to track path-by-path.
	kbPendingCap = 256
	// kbHashThreshold is the size at or above which content hashing is
	// skipped and (size, mtime) alone decides change — hashing a 200 MB
	// video every batch would cost more than the upload it might save.
	kbHashThreshold = 4 << 20 // 4 MiB
	// kbUploadConcurrency caps concurrent uploads within one project's batch
	// so a project with many changed files doesn't saturate the executor's
	// link to the blob store.
	kbUploadConcurrency = 2
	// Env knobs; see docs/self-hosting/storage.md.
	envKBSyncMaxFileMB = "TF_KB_SYNC_MAX_FILE_MB"
	envKBQuotaMB       = "TF_KB_QUOTA_MB"
	defaultKBMaxFileMB = 512
	defaultKBQuotaMB   = 2048
)

// NewKBSyncer constructs an executor KB syncer rooted at root (the multi-mode
// state root, above the orgs/<org>/projects subtree) and starts its event
// loop. kb is the shared KB blob store; hub broadcasts the panel refresh
// events; resolveOrgFor stamps each broadcast with the project's owning org so
// the hub's per-tenant filter delivers it. The root is created if missing.
func NewKBSyncer(kb *kbstore.Store, hub Broadcaster, root string, resolveOrgFor ProjectOrgResolver) (*KBSyncer, error) {
	if kb == nil {
		return nil, errors.New("kbsync: nil kb store")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("ensure watch root: %w", err)
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fsnotify: %w", err)
	}
	ks := &KBSyncer{
		watcher:       w,
		kb:            kb,
		hub:           hub,
		root:          root,
		prefix:        watchPrefix(),
		resolveOrgFor: resolveOrgFor,
		projects:      make(map[string]*projectSync),
	}
	if err := w.Add(root); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("watch root %q: %w", root, err)
	}
	ks.descendChildren(root, 0, false)
	go ks.run()
	return ks, nil
}

// Close stops the syncer's event loop. In-flight debounce timers are left to
// fire — they process against the shared store and hub, which outlive this
// syncer, so a trailing upload/broadcast is harmless.
func (ks *KBSyncer) Close() error { return ks.watcher.Close() }

func (ks *KBSyncer) projectDepth() int { return len(ks.prefix) }

// onPrefixPath / knowledgeProjectID / handleDirCreated / watchTree /
// descendChildren mirror KnowledgeWatcher's non-recursive watch-installation
// mechanics — kept as a deliberate copy so the local-mode watcher stays
// byte-identical and untouched (the executor syncer is a distinct topology).
func (ks *KBSyncer) onPrefixPath(parts []string) bool {
	n := min(len(parts), len(ks.prefix))
	for i := 0; i < n; i++ {
		if seg := ks.prefix[i]; seg != "" && parts[i] != seg {
			return false
		}
	}
	return true
}

// knowledgeProjectID returns (projectID, relPath, ok): relPath is the file's
// path relative to the project's knowledge-base dir (the flat KB filename), ok
// is false for anything that isn't inside a knowledge-base subtree.
func (ks *KBSyncer) knowledgeProjectID(parts []string) (projectID, relPath string, ok bool) {
	pd := ks.projectDepth()
	kb := pd + 1 // knowledge-base segment index
	if len(parts) <= kb {
		return "", "", false
	}
	if parts[kb] != kbDirName {
		return "", "", false
	}
	if !ks.onPrefixPath(parts) {
		return "", "", false
	}
	rel := strings.Join(parts[kb+1:], "/")
	return parts[pd], rel, true
}

func (ks *KBSyncer) run() {
	for {
		select {
		case event, ok := <-ks.watcher.Events:
			if !ok {
				return
			}
			ks.handle(event)
		case err, ok := <-ks.watcher.Errors:
			if !ok {
				return
			}
			kbsyncLog.Warn("watcher error", "error", err)
		}
	}
}

func (ks *KBSyncer) handle(event fsnotify.Event) {
	rel, err := filepath.Rel(ks.root, event.Name)
	if err != nil {
		return
	}
	parts := strings.Split(rel, string(filepath.Separator))

	if event.Op&fsnotify.Create != 0 {
		if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
			ks.handleDirCreated(parts, event.Name)
		}
	}

	if projectID, relPath, ok := ks.knowledgeProjectID(parts); ok && relPath != "" {
		ks.observe(projectID, relPath, event.Name)
	}
}

func (ks *KBSyncer) handleDirCreated(parts []string, full string) {
	if !ks.onPrefixPath(parts) {
		return
	}
	pd := ks.projectDepth()
	idx := len(parts) - 1
	switch {
	case idx <= pd:
		ks.watchTree(full, idx+1)
	case idx == pd+1 && parts[idx] == kbDirName:
		if err := ks.watcher.Add(full); err != nil {
			kbsyncLog.Warn("watch new kb dir failed", "dir", full, "error", err)
		}
	}
}

func (ks *KBSyncer) watchTree(dir string, depth int) {
	if err := ks.watcher.Add(dir); err != nil {
		kbsyncLog.Warn("watch dir failed", "dir", dir, "error", err)
		return
	}
	ks.descendChildren(dir, depth, true)
}

// descendChildren installs inner watches along the prefix → project → kb
// path. When seedExisting is true (runtime atomic-create of a project tree),
// any knowledge-base files found already on disk are seeded into the pending
// set so a project laid down whole (import, first turn) still syncs.
func (ks *KBSyncer) descendChildren(dir string, depth int, seedExisting bool) {
	pd := ks.projectDepth()
	if depth > pd+1 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		kbsyncLog.Warn("read dir failed", "dir", dir, "error", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		child := filepath.Join(dir, e.Name())
		switch {
		case depth < pd:
			if seg := ks.prefix[depth]; seg == "" || seg == e.Name() {
				ks.watchTree(child, depth+1)
			}
		case depth == pd:
			ks.watchTree(child, depth+1)
		case depth == pd+1:
			if e.Name() == kbDirName {
				if err := ks.watcher.Add(child); err != nil {
					kbsyncLog.Warn("watch dir failed", "dir", child, "error", err)
					continue
				}
				if seedExisting {
					ks.seedExistingFiles(filepath.Base(dir), child)
				}
			}
		}
	}
}

// seedExistingFiles queues every flat file already present in a
// just-watched knowledge-base dir so an atomically-created project tree
// (import, first turn) is synced even though its inner Create events fired
// before the watch existed.
func (ks *KBSyncer) seedExistingFiles(projectID, kbDir string) {
	entries, err := os.ReadDir(kbDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ks.observe(projectID, e.Name(), filepath.Join(kbDir, e.Name()))
	}
}

// observe records one changed KB file and (re-)arms the project's debounce.
func (ks *KBSyncer) observe(projectID, relPath, fullPath string) {
	snap := statOf(fullPath)
	ks.mu.Lock()
	ps := ks.projects[projectID]
	if ps == nil {
		ps = &projectSync{pending: map[string]statSnap{}, manifest: map[string]fileMeta{}}
		ks.projects[projectID] = ps
	}
	if _, already := ps.pending[relPath]; !already && len(ps.pending) >= kbPendingCap {
		ps.overflow = true
	}
	ps.pending[relPath] = snap
	if ps.timer != nil {
		ps.timer.Stop()
	}
	ps.timer = time.AfterFunc(knowledgeDebounce, func() { ks.flush(projectID) })
	ks.mu.Unlock()
}

// flush drains one project's pending set: it uploads/deletes the quiesced,
// genuinely-changed files (or reconciles the whole dir on overflow) and
// broadcasts a pending event at the start of the batch and a completion event
// at the end. Runs on the debounce timer's goroutine.
func (ks *KBSyncer) flush(projectID string) {
	ks.mu.Lock()
	ps := ks.projects[projectID]
	if ps == nil {
		ks.mu.Unlock()
		return
	}
	pending := ps.pending
	overflow := ps.overflow
	ps.pending = map[string]statSnap{}
	ps.overflow = false
	ks.mu.Unlock()

	orgID := ks.resolveOrg(projectID)
	if orgID == "" {
		return // cannot scope the broadcast or build a key — drop, turn-end reconcile backstops
	}

	ctx := context.Background()
	kbDir := ks.knowledgeBaseDir(orgID, projectID)

	if overflow {
		kbsyncLog.Info("pending set overflow; full reconcile", "project", projectID)
		ks.broadcast(orgID, projectID, nil) // batch start, unknown set
		if err := ks.reconcile(ctx, orgID, projectID, kbDir); err != nil {
			kbsyncLog.Warn("full reconcile failed", "project", projectID, "error", err)
		}
		ks.broadcast(orgID, projectID, nil) // batch complete
		return
	}

	// Quiesce pass: a file whose stat moved since we recorded it is still
	// being written — re-arm it instead of uploading a partial.
	ready := make(map[string]statSnap, len(pending))
	var stillGrowing []string
	for rel, seen := range pending {
		now := statOf(ks.fullPath(orgID, projectID, rel))
		if now.exists && (now.size != seen.size || !now.mtime.Equal(seen.mtime)) {
			stillGrowing = append(stillGrowing, rel)
			continue
		}
		ready[rel] = now
	}
	for _, rel := range stillGrowing {
		ks.reobserve(projectID, rel, ks.fullPath(orgID, projectID, rel))
	}

	if len(ready) == 0 {
		return
	}

	names := make([]string, 0, len(ready))
	for rel := range ready {
		names = append(names, rel)
	}
	ks.broadcast(orgID, projectID, names) // batch start with pending names

	ks.ensureManifestSeeded(ctx, ps, orgID, projectID)
	ks.syncBatch(ctx, ps, orgID, projectID, kbDir, ready)

	ks.broadcast(orgID, projectID, nil) // batch complete
}

// syncBatch uploads/deletes the quiesced files, bounded to
// kbUploadConcurrency in flight, applying the tiered change detection and the
// size/quota caps. A failed upload leaves the file dirty (re-armed) so the
// next batch or the turn-end reconcile retries it.
func (ks *KBSyncer) syncBatch(ctx context.Context, ps *projectSync, orgID, projectID, kbDir string, ready map[string]statSnap) {
	maxFileBytes := int64(envInt(envKBSyncMaxFileMB, defaultKBMaxFileMB)) << 20
	quotaBytes := int64(envInt(envKBQuotaMB, defaultKBQuotaMB)) << 20

	sem := make(chan struct{}, kbUploadConcurrency)
	var wg sync.WaitGroup
	for rel, snap := range ready {
		if kbstore.ValidateName(rel) != nil {
			// Nested or dot-prefixed — never a flat, user-visible KB file.
			continue
		}
		if !snap.exists {
			ks.deleteFile(ctx, ps, orgID, projectID, rel)
			continue
		}
		if snap.size > maxFileBytes {
			kbsyncLog.Warn("kb file over per-file cap; skipped", "project", projectID, "file", rel, "size", snap.size, "cap", maxFileBytes)
			continue
		}
		decision := ks.classify(ps, orgID, projectID, rel, snap, quotaBytes)
		if decision == decisionSkip {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(rel string, snap statSnap) {
			defer wg.Done()
			defer func() { <-sem }()
			ks.uploadFile(ctx, ps, orgID, projectID, kbDir, rel, snap)
		}(rel, snap)
	}
	wg.Wait()
}

type syncDecision int

const (
	decisionSkip syncDecision = iota
	decisionUpload
)

// classify applies tiered change detection against the manifest and the
// per-project quota. Under the lock so the manifest read is race-free with a
// concurrent upload's write.
func (ks *KBSyncer) classify(ps *projectSync, orgID, projectID, rel string, snap statSnap, quotaBytes int64) syncDecision {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	m, ok := ps.manifest[rel]
	if ok {
		// Exact (size, mtime) match — we have synced this exact file.
		if m.size == snap.size && m.mtime.Equal(snap.mtime) {
			return decisionSkip
		}
		// A seed-from-store entry (zero mtime, empty hash) whose size matches:
		// a turn-start materialize just made disk == store, so treat as synced.
		if m.hash == "" && m.mtime.IsZero() && m.size == snap.size {
			return decisionSkip
		}
	}
	if snap.size < kbHashThreshold {
		if h, err := hashFile(ks.fullPath(orgID, projectID, rel)); err == nil {
			if ok && m.hash != "" && m.hash == h {
				// Identical content re-written — record the new mtime, skip upload.
				m.mtime = snap.mtime
				ps.manifest[rel] = m
				return decisionSkip
			}
		}
	}
	// Quota check against the manifest total, counting this file's new size.
	if quotaBytes > 0 {
		var total int64
		for name, mm := range ps.manifest {
			if name == rel {
				continue
			}
			total += mm.size
		}
		if total+snap.size > quotaBytes {
			kbsyncLog.Warn("kb project over quota; upload skipped", "project", projectID, "file", rel, "quota", quotaBytes)
			return decisionSkip
		}
	}
	return decisionUpload
}

// uploadFile streams one file to the store and records it in the manifest. A
// failure re-arms the file so a later batch / the turn-end reconcile retries.
func (ks *KBSyncer) uploadFile(ctx context.Context, ps *projectSync, orgID, projectID, kbDir, rel string, snap statSnap) {
	full := ks.fullPath(orgID, projectID, rel)
	f, err := os.Open(full)
	if err != nil {
		if !os.IsNotExist(err) {
			kbsyncLog.Warn("open kb file for upload failed", "project", projectID, "file", rel, "error", err)
			ks.reobserve(projectID, rel, full)
		}
		return
	}
	h := sha256.New()
	var reader io.Reader = f
	if snap.size < kbHashThreshold {
		// Hash while uploading so a small file's manifest entry can suppress
		// the next identical rewrite without a second read.
		reader = io.TeeReader(f, h)
	}
	putErr := ks.kb.Put(ctx, orgID, projectID, rel, reader)
	f.Close()
	if putErr != nil {
		kbsyncLog.Warn("kb upload failed; will retry", "project", projectID, "file", rel, "error", putErr)
		ks.reobserve(projectID, rel, full)
		return
	}
	hash := ""
	if snap.size < kbHashThreshold {
		hash = hex.EncodeToString(h.Sum(nil))
	}
	ks.mu.Lock()
	ps.manifest[rel] = fileMeta{size: snap.size, mtime: snap.mtime, hash: hash}
	ks.mu.Unlock()
}

// deleteFile removes a file from the store and drops its manifest entry.
func (ks *KBSyncer) deleteFile(ctx context.Context, ps *projectSync, orgID, projectID, rel string) {
	if kbstore.ValidateName(rel) != nil {
		return
	}
	if err := ks.kb.Delete(ctx, orgID, projectID, rel); err != nil {
		kbsyncLog.Warn("kb delete failed; will retry", "project", projectID, "file", rel, "error", err)
		ks.reobserve(projectID, rel, ks.fullPath(orgID, projectID, rel))
		return
	}
	ks.mu.Lock()
	delete(ps.manifest, rel)
	ks.mu.Unlock()
}

// reobserve re-queues a file (a still-growing file, or a failed upload) so the
// debounce fires again — the retry/quiesce mechanism.
func (ks *KBSyncer) reobserve(projectID, rel, fullPath string) {
	ks.observe(projectID, rel, fullPath)
}

// reconcile is the whole-directory disk→store push used on pending-set
// overflow: it uploads disk files the store lacks or that differ by size
// (push-only — see reconcileProjectKB for why the backstop never deletes
// store files), then rebuilds the manifest to match disk so subsequent
// incremental batches start from a correct cache. Deletes still propagate
// through the per-file Remove path, not this whole-directory diff.
func (ks *KBSyncer) reconcile(ctx context.Context, orgID, projectID, kbDir string) error {
	if err := reconcileProjectKB(ctx, ks.kb, orgID, projectID, kbDir); err != nil {
		return err
	}
	ks.rebuildManifest(orgID, projectID, kbDir)
	return nil
}

// ensureManifestSeeded lazily seeds a project's manifest from the store's
// listing on first use, so files already present in the store (a warm
// executor after a turn-start materialize) aren't re-uploaded.
func (ks *KBSyncer) ensureManifestSeeded(ctx context.Context, ps *projectSync, orgID, projectID string) {
	ks.mu.Lock()
	seeded := ps.seeded
	ks.mu.Unlock()
	if seeded {
		return
	}
	list, err := ks.kb.List(ctx, orgID, projectID)
	if err != nil {
		kbsyncLog.Warn("seed manifest list failed", "project", projectID, "error", err)
		return
	}
	ks.mu.Lock()
	for _, f := range list {
		if _, ok := ps.manifest[f.Name]; !ok {
			ps.manifest[f.Name] = fileMeta{size: f.Size} // zero mtime + empty hash = seed marker
		}
	}
	ps.seeded = true
	ks.mu.Unlock()
}

// rebuildManifest replaces a project's manifest with the current on-disk flat
// contents (size + mtime, no hash) after a full reconcile.
func (ks *KBSyncer) rebuildManifest(orgID, projectID, kbDir string) {
	entries, err := os.ReadDir(kbDir)
	if err != nil {
		return
	}
	fresh := map[string]fileMeta{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if kbstore.ValidateName(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fresh[e.Name()] = fileMeta{size: info.Size(), mtime: info.ModTime()}
	}
	ks.mu.Lock()
	ps := ks.projects[projectID]
	if ps != nil {
		ps.manifest = fresh
		ps.seeded = true
	}
	ks.mu.Unlock()
}

func (ks *KBSyncer) knowledgeBaseDir(orgID, projectID string) string {
	dir, err := KnowledgeDir(orgID, projectID)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, kbDirName)
}

func (ks *KBSyncer) fullPath(orgID, projectID, rel string) string {
	return filepath.Join(ks.knowledgeBaseDir(orgID, projectID), filepath.FromSlash(rel))
}

func (ks *KBSyncer) resolveOrg(projectID string) string {
	if ks.resolveOrgFor == nil {
		return ""
	}
	return ks.resolveOrgFor(projectID)
}

// broadcast emits project_knowledge_updated. It ALWAYS carries a "pending"
// field — the batch-start names, or an empty list on completion. That empty
// list (not a null Data) is what distinguishes an executor sync signal from the
// control pod's own upload/delete event, which broadcasts Data:null: the panel
// clears its ghost rows only on a sync signal, so a concurrent panel mutation
// of a different file can't prematurely clear a ghost for an in-flight batch.
func (ks *KBSyncer) broadcast(orgID, projectID string, names []string) {
	if ks.hub == nil || orgID == "" {
		return
	}
	if names == nil {
		names = []string{}
	}
	ks.hub.Broadcast(websocket.Event{
		Type:      "project_knowledge_updated",
		OrgID:     orgID,
		ProjectID: projectID,
		Data:      map[string]any{"pending": names},
	})
}

// MaterializeKB pulls the store→disk KB diff for a project on this executor —
// the kb_changed doorbell entry point. The dispatcher self-filters
// to a live session before calling, so this reflects a panel upload/delete
// into the dir the running agent reads. A missing KB store (local / unwired)
// or resolve failure is a no-op — the next turn's start-materialize backstops.
func (c *Curator) MaterializeKB(ctx context.Context, orgID, projectID string) {
	kb := c.getKB()
	if kb == nil {
		return
	}
	dir, err := KnowledgeDir(orgID, projectID)
	if err != nil {
		curatorLog.Warn("kb doorbell materialize: resolve dir failed", "project", projectID, "error", err)
		return
	}
	kbDir := filepath.Join(dir, kbDirName)
	if err := materializeProjectKB(ctx, kb, orgID, projectID, kbDir); err != nil {
		curatorLog.Warn("kb doorbell materialize failed", "project", projectID, "error", err)
	}
}

// DropMaterializedKBIfIdle best-effort removes this executor's materialized
// project dir on a kb_changed op="project_deleted", but only when it holds no
// live session for the project — a live session is mid-turn and owns the dir,
// so its own teardown (CancelProject → session shutdown) handles cleanup. The
// blob store is the source of truth and was already cleared by the control
// pod's DeletePrefix, so this only reclaims the executor's local cache.
func (c *Curator) DropMaterializedKBIfIdle(orgID, projectID string) {
	if c.getKB() == nil {
		return
	}
	if c.HasLiveSession(projectID) {
		return
	}
	dir, err := KnowledgeDir(orgID, projectID)
	if err != nil {
		return
	}
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		curatorLog.Warn("kb doorbell: drop materialized project dir failed", "project", projectID, "dir", dir, "error", err)
	}
}

// --- shared materialize / reconcile helpers (used by the session brackets) ---

// materializeProjectKB pulls the store→disk diff for one project: it downloads
// files the store has that are missing or a different size on disk, and
// removes local flat files the store no longer has. The turn-start bracket
// runs this so a homed turn's agent sees panel uploads and other executors'
// writes; it is usually a near no-op on a sticky-homed warm executor.
func materializeProjectKB(ctx context.Context, kb *kbstore.Store, orgID, projectID, kbDir string) error {
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		return fmt.Errorf("ensure kb dir: %w", err)
	}
	diff, err := kb.DiffManifest(ctx, orgID, projectID, kbDir)
	if err != nil {
		return err
	}
	for _, f := range diff.Pull {
		if err := downloadOne(ctx, kb, orgID, projectID, kbDir, f.Name); err != nil {
			kbsyncLog.Warn("materialize download failed", "project", projectID, "file", f.Name, "error", err)
		}
	}
	// RemoveLocal is intentionally asymmetric with reconcileProjectKB's
	// push-only stance, and the asymmetry is correct under the "disk is cache,
	// the store is the truth" doctrine. A disk file the store lacks is either a
	// panel delete to propagate (the common case — the store is authoritative,
	// so the cache must drop it) OR a local write that never reached the store
	// before this pod restarted (crashed between the fsnotify write and the
	// debounce/turn-end reconcile). We remove both: an un-synced write was never
	// durable, so treating it as absent from the truth loses nothing the system
	// had ever committed to keep — unlike a store-only file, which IS durable
	// user data and is why reconcileProjectKB never deletes remotely.
	for _, name := range diff.RemoveLocal {
		if err := os.Remove(filepath.Join(kbDir, name)); err != nil && !os.IsNotExist(err) {
			kbsyncLog.Warn("materialize local remove failed", "project", projectID, "file", name, "error", err)
		}
	}
	return nil
}

// reconcileProjectKB pushes the disk→store diff for one project: it uploads
// disk files the store lacks or that differ by size. It is the turn-end
// backstop for any write the watcher dropped mid-turn.
//
// It is deliberately push-ONLY — it never deletes store files. Agent deletes
// are already propagated by the watcher as they happen (a real fs Remove event
// keyed to the exact file, which is reliable), so by turn end a store file with
// no disk counterpart is a PANEL upload that landed in the store but hasn't
// materialized onto this executor's disk (e.g. the kb_changed doorbell was
// dropped). Deleting it here would silently lose the user's uploaded image or
// video — exactly the data this ticket is built to keep safe. The next
// turn-start materialize pulls it onto disk instead.
func reconcileProjectKB(ctx context.Context, kb *kbstore.Store, orgID, projectID, kbDir string) error {
	diff, err := kb.DiffManifest(ctx, orgID, projectID, kbDir)
	if err != nil {
		return err
	}
	for _, name := range diff.Push {
		if err := uploadOne(ctx, kb, orgID, projectID, kbDir, name); err != nil {
			kbsyncLog.Warn("reconcile upload failed", "project", projectID, "file", name, "error", err)
		}
	}
	return nil
}

func downloadOne(ctx context.Context, kb *kbstore.Store, orgID, projectID, kbDir, name string) error {
	rc, err := kb.Get(ctx, orgID, projectID, name)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(kbDir, ".materialize-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(kbDir, name))
}

func uploadOne(ctx context.Context, kb *kbstore.Store, orgID, projectID, kbDir, name string) error {
	f, err := os.Open(filepath.Join(kbDir, name))
	if err != nil {
		return err
	}
	defer f.Close()
	return kb.Put(ctx, orgID, projectID, name, f)
}

// --- small file helpers ---

func statOf(full string) statSnap {
	info, err := os.Lstat(full)
	if err != nil || !info.Mode().IsRegular() {
		return statSnap{exists: false}
	}
	return statSnap{size: info.Size(), mtime: info.ModTime(), exists: true}
}

func hashFile(full string) (string, error) {
	f, err := os.Open(full)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
