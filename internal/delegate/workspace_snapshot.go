// Durable blueprint workspace: snapshot a parked run's non-recoverable
// workspace to the blob store on dormancy, and rehydrate it on resume when the
// warm on-disk worktree is gone. The object store is the source of truth; the
// host worktree is a warm cache. A parked workspace surviving locally is the
// fast path (resume uses it directly, rehydrate is a no-op); a missing one
// rebuilds from the snapshot — never a brick.
//
// Two snapshot triggers are wired today: idle hibernation to `open`
// (hibernatePark, live.go) and a completed+abort terminal (processCompletion),
// both message-resumable. A third — an executor-drain/scale-down trigger — is a forward seam for the
// execution-plane split: there are no executors to drain yet, so it is
// intentionally NOT wired. When it lands it calls snapshotWorkspace with the
// same key, identically to the two triggers above.

package delegate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// Snapshot tar member names. The blob is one gzip-compressed tar holding the
// git delta, the ephemeral _tfac tree, the Claude session transcript, and a
// manifest; rehydrate demuxes by these names.
const (
	snapManifest = "manifest.json"
	snapBundle   = "repo.bundle"
	snapPatch    = "uncommitted.patch"
	snapSession  = "session.jsonl"
	// snapScratchPrefix names the scratch members INSIDE the blob, which is a
	// storage format rather than a path: it is deliberately not derived from the
	// on-disk directory name, so renaming that directory never strands the
	// snapshot of a parked run written by an earlier build.
	snapScratchPrefix = "scratch/"
)

// scratchExcludes are the top-level _tfac entries that already re-materialize on
// the next run and so never ride in the snapshot: entity-memory rebuilds from
// conversation_memory (and under a jail is not even a directory — it is the
// symlink standing in for the read-only mount, which the walk skips as
// non-regular regardless), project-knowledge is re-copied from
// the project KB. Everything else under _tfac (ci-logs, skill scratch,
// ad-hoc agent files, and the agent's own memory.md — which is not in the DB
// until termination ingests it) is non-recoverable and IS captured.
var scratchExcludes = map[string]bool{"entity-memory": true, "project-knowledge": true}

// snapshotManifest is the small header describing what a snapshot blob carries,
// read first on rehydrate to decide how to reconstruct.
type snapshotManifest struct {
	Branch    string `json:"branch"`
	Head      string `json:"head"`
	SessionID string `json:"session_id"`
	HasGit    bool   `json:"has_git"`
}

// snapshotKey is the storage key for a parked workspace's snapshot blob. keyID
// is the blueprint_run_id — every run is a blueprint step now, so every step of
// one blueprint shares the one workspace blob. It is exactly the value
// memoryNamespace yields and the value the on-disk worktree directory is named
// after, so the key, the namespace, and the dir name stay in lockstep.
//
// The "workspace.tar" leaf is the bare tar's name; the blob is gzipped inside
// it. Keeping the leaf avoids a dual-key dance at every discard/delete site for
// zero benefit — nothing reads the key's extension to decide the format.
func snapshotKey(orgID, keyID string) string {
	return orgID + "/" + keyID + "/workspace.tar"
}

// snapshotWorkspace writes a parked run's non-recoverable workspace state — the
// git delta, the ephemeral _tfac subdirs, and the Claude session transcript
// — to durable storage under the run's snapshot key, so a resume that lands
// without the warm on-disk worktree can rebuild it (see ensureWorkspace). It
// runs identically in both modes: local writes the same blob through fsStorage
// under the state-root, multi through the object store.
//
// Best-effort by contract: callers log and proceed on error, because the warm
// worktree (preserved on dormancy by the per-run guards) is the primary resume
// path and the snapshot is the durable backstop, only read when that cache is
// gone.
func (s *Spawner) snapshotWorkspace(ctx context.Context, orgID, keyID, wtPath, sessionID string) error {
	blobs := s.Storage()
	if blobs == nil {
		return nil // no store wired (tests / a configuration without the seam)
	}
	if keyID == "" {
		return fmt.Errorf("snapshot: empty key id")
	}
	if wtPath == "" {
		return fmt.Errorf("snapshot: empty worktree path")
	}

	// Non-recoverable state — the git delta (nil for a non-git run-root, e.g. a
	// Jira lazy run) AND the session transcript. In multi mode both are read
	// inside a dropped-privilege, network-isolated child running as the sandbox
	// uid: the git capture's filter-honoring commands never execute
	// agent-planted drivers as root, and the SDK's owner-only transcript is
	// readable there when it is not to the orchestrator (see captureWorkspaceGit).
	delta, transcript, err := captureWorkspaceGit(ctx, wtPath, sessionID)
	if err != nil {
		return fmt.Errorf("snapshot: capture: %w", err)
	}

	// Stage the tar on disk, then stream it into Put. A large workspace (the
	// _tfac tree especially) never buffers whole in memory: scratch files
	// are copied into the tar file by file, and Put reads the staged tar back
	// incrementally rather than from a single in-RAM buffer. The stream is
	// gzipped on its way to the staging file — the transcript and ci-logs
	// members that dominate the blob are highly compressible text — without
	// touching the member-by-member streaming inside writeSnapshotTar.
	f, err := os.CreateTemp("", "tf-snapshot-*.tar.gz")
	if err != nil {
		return fmt.Errorf("snapshot: tempfile: %w", err)
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	gzw := gzip.NewWriter(f)
	if err := writeSnapshotTar(gzw, delta, wtPath, sessionID, transcript); err != nil {
		_ = f.Close()
		return err
	}
	if err := gzw.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("snapshot: close gzip: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("snapshot: stat staged tar: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return fmt.Errorf("snapshot: rewind tar: %w", err)
	}
	putErr := blobs.Put(ctx, snapshotKey(orgID, keyID), f)
	_ = f.Close()
	if putErr != nil {
		return fmt.Errorf("snapshot: put: %w", putErr)
	}
	// Parked-window storage cost is a live sizing question; log every
	// snapshot's real compressed footprint so it's answerable from the field.
	delegateLog.Info("snapshot written", "key", snapshotKey(orgID, keyID), "bytes_gzipped", fi.Size())
	return nil
}

// writeSnapshotTar streams the snapshot members into w as one tar: the git
// bundle + uncommitted patch (bounded — they're the delta, not full history),
// the ephemeral _tfac tree (streamed file by file), the Claude session
// transcript, and the manifest.
func writeSnapshotTar(w io.Writer, delta *worktree.GitDelta, wtPath, sessionID string, transcript []byte) error {
	tw := tar.NewWriter(w)
	man := snapshotManifest{SessionID: sessionID}
	if delta != nil {
		man.HasGit = true
		man.Branch = delta.Branch
		man.Head = delta.Head
		if len(delta.Bundle) > 0 {
			if err := writeTarBytes(tw, snapBundle, delta.Bundle); err != nil {
				return err
			}
		}
		if len(delta.Patch) > 0 {
			if err := writeTarBytes(tw, snapPatch, delta.Patch); err != nil {
				return err
			}
		}
	}
	if err := tarScratch(tw, wtPath); err != nil {
		return fmt.Errorf("snapshot: tar scratch: %w", err)
	}
	if sessionID != "" {
		if len(transcript) > 0 {
			if err := writeTarBytes(tw, snapSession, transcript); err != nil {
				return err
			}
		} else {
			// The run has a session but the capture came back without its
			// transcript (absent on disk, or a capture that couldn't read it). The
			// blob is still written — worktree state matters on its own — but a
			// resume from it will hit the transcript-missing guard and fail.
			// Surface it: this is otherwise silent, and it's exactly the shape that
			// produced a resume-fails-with-no-reason report.
			delegateLog.Warn("snapshot omits session transcript; a resume of this run will not be able to continue the conversation", "session", sessionID, "worktree", wtPath)
		}
	}
	manBytes, err := json.Marshal(man)
	if err != nil {
		return fmt.Errorf("snapshot: marshal manifest: %w", err)
	}
	if err := writeTarBytes(tw, snapManifest, manBytes); err != nil {
		return err
	}
	return tw.Close()
}

// ensureWorkspace guarantees the run's worktree exists on disk before a resume
// re-invokes the agent, returning the cwd to resume in. Warm path: the parked
// worktree survived on disk (the dormancy guards kept it) → return it as-is,
// rehydrate is a no-op. Cold path: it's gone (host loss, /tmp wipe, or a
// startup sweep) → rebuild it from the durable snapshot and return the rebuilt
// path. owner/repo/cloneURL locate (and, on a fresh host only, seed) the bare
// the git delta replays onto; they're empty/unused for a non-git run-root.
func (s *Spawner) ensureWorkspace(ctx context.Context, orgID string, run *domain.Conversation, owner, repo, cloneURL string) (string, error) {
	if run.WorktreePath != "" {
		if _, err := os.Stat(run.WorktreePath); err == nil {
			return run.WorktreePath, nil // warm: worktree still on disk
		}
	}

	blobs := s.Storage()
	if blobs == nil {
		return "", fmt.Errorf("worktree %q missing and no blob store to rehydrate from", run.WorktreePath)
	}
	keyID := memoryNamespace(run.BlueprintRunID, run.ID)
	rc, err := blobs.Get(ctx, snapshotKey(orgID, keyID))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", fmt.Errorf("worktree %q missing and no snapshot for %s to rehydrate from", run.WorktreePath, keyID)
		}
		return "", fmt.Errorf("rehydrate: get snapshot: %w", err)
	}
	defer func() { _ = rc.Close() }()

	// Rebuild at the deterministic, host-local run-root for this key (equal to
	// run.WorktreePath on the same host; a fresh path after landing elsewhere).
	wtDir := worktree.RunRoot(keyID)
	if err := s.rehydrateFromSnapshot(ctx, orgID, owner, repo, cloneURL, wtDir, rc); err != nil {
		return "", err
	}
	if wtDir != run.WorktreePath {
		// Point the run (and the cleanup paths that key off it) at the rebuilt
		// worktree. System write — resume goroutines hold no JWT claims.
		// Non-fatal: the rebuilt path is returned and this resume proceeds. But
		// run.WorktreePath stays stale, so the NEXT resume won't find the
		// warm copy and will cold-rehydrate again (correct, just slower) — log
		// it distinctly so unexpected repeat rehydrates are diagnosable.
		if err := s.agentRuns.SetWorktreePathSystem(context.Background(), orgID, run.ID, wtDir); err != nil {
			delegateLog.Warn("rehydrate: persist new worktree_path failed; stale path will force a repeat cold rehydrate on the next resume", "worktree_path", wtDir, "run", run.ID, "error", err)
		}
	}
	return wtDir, nil
}

// rehydrateFromSnapshot unpacks a snapshot blob and reconstructs the worktree
// at wtDir: rebuild the git worktree from the bare + delta (or just the
// directory for a non-git run-root), restore the ephemeral _tfac tree, and
// drop the Claude session transcript at the new cwd's encoding so
// `claude --resume` reconnects.
//
// The bounded members (manifest, bundle, patch, session) are read into memory;
// the _tfac tree — which can run to GBs — is streamed to a staging dir on
// disk as it's read (the worktree it belongs in doesn't exist until
// RestoreWorkspaceGit runs below), then moved into place with one rename. This
// mirrors the snapshot side's temp-file staging so neither direction buffers a
// large workspace whole.
func (s *Spawner) rehydrateFromSnapshot(ctx context.Context, orgID, owner, repo, cloneURL, wtDir string, r io.Reader) error {
	var man snapshotManifest
	var bundle, patch, session []byte

	if err := os.MkdirAll(filepath.Dir(wtDir), 0o755); err != nil {
		return fmt.Errorf("rehydrate: mkdir runs parent: %w", err)
	}
	// Sibling of wtDir → the post-restore move is an intra-filesystem rename.
	scratchStaging, err := os.MkdirTemp(filepath.Dir(wtDir), ".scratch-rehydrate-*")
	if err != nil {
		return fmt.Errorf("rehydrate: scratch staging: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratchStaging) }() // no-op once renamed into place
	sawScratch := false

	// Snapshots are gzipped tars (the writer's gzip.Writer wrapper).
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("rehydrate: open gzip: %w", err)
	}
	defer func() { _ = gzr.Close() }()
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("rehydrate: read tar: %w", err)
		}
		switch {
		case hdr.Name == snapManifest:
			data, err := io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("rehydrate: read manifest: %w", err)
			}
			if err := json.Unmarshal(data, &man); err != nil {
				return fmt.Errorf("rehydrate: manifest: %w", err)
			}
		case hdr.Name == snapBundle:
			if bundle, err = io.ReadAll(tr); err != nil {
				return fmt.Errorf("rehydrate: read bundle: %w", err)
			}
		case hdr.Name == snapPatch:
			if patch, err = io.ReadAll(tr); err != nil {
				return fmt.Errorf("rehydrate: read patch: %w", err)
			}
		case hdr.Name == snapSession:
			if session, err = io.ReadAll(tr); err != nil {
				return fmt.Errorf("rehydrate: read session: %w", err)
			}
		case strings.HasPrefix(hdr.Name, snapScratchPrefix):
			if err := stageScratchMember(scratchStaging, strings.TrimPrefix(hdr.Name, snapScratchPrefix), tr); err != nil {
				return err
			}
			sawScratch = true
		}
	}

	// gzip validates the CRC-32 / ISIZE trailer only when the stream is read to
	// its footer, but the tar reader stops at the archive's end-of-archive
	// marker — which precedes that footer — so member reads alone never trigger
	// the check (and gzip.Reader.Close does not force it either). Drain the
	// remainder (a few trailing bytes for our own writes) to validate the whole
	// blob, and do it here, before any worktree mutation: a checksum mismatch
	// means the snapshot is corrupt, so fail the rehydrate rather than rebuild
	// onto untrustworthy state.
	if _, err := io.Copy(io.Discard, gzr); err != nil {
		return fmt.Errorf("rehydrate: gzip integrity: %w", err)
	}

	if man.HasGit {
		delta := &worktree.GitDelta{Branch: man.Branch, Head: man.Head, Bundle: bundle, Patch: patch}
		// No clone credential is minted here: every ensureWorkspace caller
		// passes an empty cloneURL (the rebuild replays the delta onto a bare
		// that already exists on this host), and CloneAuthFor returns zero
		// auth for an empty URL — so the former token mint never reached git.
		// If a fresh-host seed-from-cloneURL path lands later, its auth must
		// route through the run's credential sidecar (the git proxy), never an
		// in-process resolver read.
		auth := worktree.CloneAuthFor(cloneURL, "")
		if err := worktree.RestoreWorkspaceGit(ctx, owner, repo, wtDir, delta, cloneURL, auth); err != nil {
			return fmt.Errorf("rehydrate: restore git: %w", err)
		}
	} else {
		// Non-git run-root (Jira lazy): just recreate the parent directory; the
		// agent re-materializes per-repo worktrees via `workspace add`.
		if err := os.MkdirAll(wtDir, 0o700); err != nil {
			return fmt.Errorf("rehydrate: make run root: %w", err)
		}
		// The git path plants this inside RestoreWorkspaceGit; do the same here so
		// a rehydrated Jira run root carries the jail's skills symlink too. The
		// tree is orchestrator-owned at this instant and won't be again after the
		// launch chown.
		if err := worktree.EnsureSandboxSkillsLink(wtDir); err != nil {
			delegateLog.Warn("plant sandbox skills symlink on rehydrated run root failed", "dir", wtDir, "error", err)
		}
	}

	if sawScratch {
		// The fresh worktree has no _tfac (git-excluded), so move the staged
		// tree in wholesale.
		if err := os.Rename(scratchStaging, filepath.Join(wtDir, worktree.ScratchDir)); err != nil {
			return fmt.Errorf("rehydrate: install scratch: %w", err)
		}
	}
	// Plant the jail's memory symlink AFTER the scratch install, never before: the
	// install is a wholesale rename onto _tfac, which fails outright if the link's
	// parent already exists. This is the rehydrated tree's one orchestrator-owned
	// moment — the run that resumes into it may find the tree already handed off,
	// and the snapshot never carries the link (entity-memory is excluded from
	// capture, and the walk skips non-regular files anyway).
	if err := worktree.EnsureSandboxMemoryLink(ctx, wtDir); err != nil {
		delegateLog.Warn("plant sandbox memory symlink on rehydrated tree failed", "dir", wtDir, "error", err)
	}
	if len(session) > 0 && man.SessionID != "" {
		if err := restoreSessionTranscript(wtDir, man.SessionID, session); err != nil {
			return err
		}
	}
	return nil
}

// DiscardWorkspaceSnapshot deletes the durable workspace snapshot for a
// standalone (non-blueprint) run that has reached a terminal state via the
// approval path: the artifact-approve handler calls it for a standalone run
// whose approval is its terminal — the single-run mirror of terminateBlueprint's
// snapshot cleanup. Keyed by run_id (a standalone run's snapshot key).
// Idempotent and nil-safe.
func (s *Spawner) DiscardWorkspaceSnapshot(orgID, runID string) {
	s.discardWorkspaceSnapshot(context.Background(), orgID, runID)
}

// discardWorkspaceSnapshot deletes a parked workspace's snapshot blob once the
// run/blueprint it belonged to reaches a terminal state, so durable storage
// doesn't accumulate orphans. keyID is memoryNamespace(blueprintRunID, runID).
// Idempotent — Delete on a missing key is a no-op — so terminal paths call it
// unconditionally without first checking whether a snapshot was ever written.
func (s *Spawner) discardWorkspaceSnapshot(ctx context.Context, orgID, keyID string) {
	blobs := s.Storage()
	if blobs == nil || keyID == "" {
		return
	}
	if err := blobs.Delete(ctx, snapshotKey(orgID, keyID)); err != nil {
		delegateLog.Warn("discard workspace snapshot failed", "org", orgID, "key_id", keyID, "error", err)
	}
}

// tarScratch walks wtPath/_tfac and writes every regular file under the
// snapScratchPrefix, skipping the re-materializable entity-memory and
// project-knowledge subtrees. A missing _tfac is fine (nothing to capture).
func tarScratch(tw *tar.Writer, wtPath string) error {
	root := filepath.Join(wtPath, worktree.ScratchDir)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil
	}
	return filepath.Walk(root, func(path string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		top := rel
		if i := strings.IndexRune(rel, filepath.Separator); i >= 0 {
			top = rel[:i]
		}
		if scratchExcludes[top] {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil // directories implied by their files; skip symlinks/etc.
		}
		// Stream each file into the tar rather than reading it whole — _tfac
		// (ci-logs, etc.) is the part that can run to GBs.
		return writeTarFile(tw, snapScratchPrefix+filepath.ToSlash(rel), path, fi.Size())
	})
}

// stageScratchMember streams one _tfac tar member to relPath under
// stagingDir without buffering it whole. The cleaned destination is verified to
// stay under stagingDir so a crafted blob (multi-mode object store) can't escape
// via a "../" member name.
func stageScratchMember(stagingDir, relPath string, r io.Reader) error {
	dest := filepath.Join(stagingDir, filepath.FromSlash(relPath))
	if rel, err := filepath.Rel(stagingDir, dest); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("rehydrate: scratch member %q escapes staging dir", relPath)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("rehydrate: mkdir scratch: %w", err)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("rehydrate: create scratch %s: %w", relPath, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("rehydrate: write scratch %s: %w", relPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("rehydrate: flush scratch %s: %w", relPath, err)
	}
	return nil
}

// restoreSessionTranscript writes the carried transcript to the new cwd's
// encoded project dir so `claude --resume <sessionID>` finds it.
func restoreSessionTranscript(wtDir, sessionID string, data []byte) error {
	p, err := worktree.ClaudeSessionPath(worktree.ResolveClaudeProjectCwd(wtDir), sessionID)
	if err != nil {
		return fmt.Errorf("rehydrate: session path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("rehydrate: mkdir session dir: %w", err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return fmt.Errorf("rehydrate: write session: %w", err)
	}
	return nil
}

// writeTarBytes writes one regular-file member into the snapshot tar from an
// in-memory buffer. Used for the bounded members (bundle, patch, session,
// manifest); _tfac files stream through writeTarFile instead.
func writeTarBytes(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o600,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return fmt.Errorf("tar header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("tar write %s: %w", name, err)
	}
	return nil
}

// writeTarFile streams a file into the snapshot tar without buffering it whole.
// size is the header length; the run is parked (dormant) when a snapshot is
// taken, so _tfac isn't being written concurrently and the on-disk size is
// stable. If a short read still occurs, io.Copy's count mismatch surfaces as a
// tar error rather than silent corruption.
func writeTarFile(tw *tar.Writer, name, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("tar open %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o600,
		Size:     size,
		Typeflag: tar.TypeReg,
	}); err != nil {
		return fmt.Errorf("tar header %s: %w", name, err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("tar copy %s: %w", name, err)
	}
	return nil
}
