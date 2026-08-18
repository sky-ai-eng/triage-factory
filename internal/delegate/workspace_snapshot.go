// Durable blueprint workspace: snapshot a parked run's non-recoverable
// workspace to the blob store on dormancy, and rehydrate it on resume when the
// warm on-disk worktree is gone. The object store is the source of truth; the
// host worktree is a warm cache. A parked workspace surviving locally is the
// fast path (resume uses it directly, rehydrate is a no-op); a missing one
// rebuilds from the snapshot — never a brick.
//
// Two snapshot triggers are wired today: idle hibernation to `open`
// (markConversationOpen, live.go) and every non-failed terminal — which after the
// terminal vocabulary shrank to completed|failed is `completed`, whatever the
// outcome (processCompletion). A third — an executor-drain/scale-down trigger —
// is a forward seam for the execution-plane split: there are no executors to
// drain yet, so it is intentionally NOT wired. When it lands it calls
// snapshotWorkspace with the same key, identically to the two triggers above.
//
// The write policy and the retention sweep move together, always: the reaper
// enumerates exactly the states listed above (ListReapableSnapshotKeysSystem),
// so widening one without the other either leaks blobs forever or reaps a
// workspace something still wants.

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

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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

// restoreWorkspaceGit is the git half of a cold rehydrate. A package var, in the
// same spirit as worktreePushTargetBranch: the credential a rehydrate hands git
// is the thing that broke, and a test that only reads the rebuilt tree cannot
// see it. Swapping this lets a test assert the git config the rebuild would run
// under without standing up an authenticating remote.
var restoreWorkspaceGit = worktree.RestoreWorkspaceGit

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
// runtime is the conversation's engine (domain.ConversationRuntimeSDK |
// ConversationRuntimeNative), carried onto the span family because the blob's
// members are not runtime-agnostic: only a delegated SDK run snapshots a
// session transcript, so transcript sizes read without the attribute would
// look like a property of all snapshots. Empty is a caller that doesn't know
// (a fixture), and simply omits the attribute.
//
// Best-effort by contract: callers log and proceed on error, because the warm
// worktree (preserved on dormancy by the per-run guards) is the primary resume
// path and the snapshot is the durable backstop, only read when that cache is
// gone.
func (s *Spawner) snapshotWorkspace(ctx context.Context, orgID, conversationID, keyID, wtPath, sessionID, runtime string) (err error) {
	blobs := s.Storage()
	if blobs == nil {
		return nil // no store wired (tests / a configuration without the seam)
	}

	// Punctual and linked, not a child: this runs at a park or a terminal,
	// arbitrarily long after the engagement's setup span ended. It is also
	// the one piece of run teardown with an unbounded cost — a git bundle, a
	// tar of the whole scratch tree, and a blob PUT — so a park that took a
	// minute is answerable here rather than only in the log. The three phase
	// children below split that answer: whether the time went to the capture,
	// the compression, or the upload decides three different fixes.
	attrs := []attribute.KeyValue{telemetry.OrgID(orgID)}
	if runtime != "" {
		attrs = append(attrs, telemetry.Runtime(runtime))
	}
	ctx, span := s.startPunctual(ctx, conversationID, "workspace.snapshot", attrs...)
	defer func() {
		recordSpanError(span, err)
		span.End()
	}()

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
	// That child is one of the privileged operations that never trace
	// themselves, so this executor-side span IS its measurement; in local mode
	// the same span covers the in-process capture.
	capCtx, capSpan := snapshotPhase(ctx, "workspace.snapshot.capture", runtime)
	delta, transcript, err := captureWorkspaceGit(capCtx, wtPath, sessionID)
	if err != nil {
		recordSpanError(capSpan, err)
		capSpan.End()
		return fmt.Errorf("snapshot: capture: %w", err)
	}
	var bundleBytes, patchBytes int64
	if delta != nil {
		bundleBytes = int64(len(delta.Bundle))
		patchBytes = int64(len(delta.Patch))
	}
	capSpan.SetAttributes(
		telemetry.SnapshotBundleBytes(bundleBytes),
		telemetry.SnapshotPatchBytes(patchBytes),
		telemetry.SnapshotTranscriptBytes(int64(len(transcript))),
	)
	capSpan.End()

	_, archSpan := snapshotPhase(ctx, "workspace.snapshot.archive", runtime)
	f, rawBytes, gzBytes, err := stageSnapshotArchive(delta, wtPath, sessionID, transcript)
	if err != nil {
		recordSpanError(archSpan, err)
		archSpan.End()
		return fmt.Errorf("snapshot: archive: %w", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	// Raw bytes in against compressed bytes out: the pair is the codec's
	// report card — ratio from the two sizes, throughput from either against
	// the phase duration — so a compression change can prove itself from the
	// field rather than a benchmark.
	archSpan.SetAttributes(telemetry.SnapshotRawBytes(rawBytes), telemetry.SizeBytes(gzBytes))
	archSpan.End()

	putCtx, putSpan := snapshotPhase(ctx, "workspace.snapshot.put", runtime)
	putSpan.SetAttributes(telemetry.SizeBytes(gzBytes))
	putErr := blobs.Put(putCtx, snapshotKey(orgID, keyID), f)
	recordSpanError(putSpan, putErr)
	putSpan.End()
	if putErr != nil {
		return fmt.Errorf("snapshot: put: %w", putErr)
	}
	// Parked-window storage cost is a live sizing question; log every
	// snapshot's real compressed footprint so it's answerable from the field.
	// On the span too, where it explains the duration beside it.
	span.SetAttributes(telemetry.SizeBytes(gzBytes))
	delegateLog.Info("snapshot written", "key", snapshotKey(orgID, keyID), "bytes_gzipped", gzBytes)
	return nil
}

// snapshotPhase opens one phase child under the workspace.snapshot span in
// ctx. An ordinary child, unlike the punctual parent: the snapshot is bounded
// work inside one function frame, so nothing here risks the unbounded-trace
// problem the punctual/link pattern exists for. runtime repeats on every
// phase (not just the parent) so a phase queried on its own — which is how
// the dashboard reads the sizes — still says which engine's snapshot it was.
func snapshotPhase(ctx context.Context, name, runtime string) (context.Context, trace.Span) {
	ctx, span := tracer.Start(ctx, name)
	if runtime != "" {
		span.SetAttributes(telemetry.Runtime(runtime))
	}
	return ctx, span
}

// stageSnapshotArchive writes the snapshot members to a gzipped tar staged on
// disk, returning the open staging file positioned at the start — ready to
// stream into Put — with its pre-compression and compressed byte counts. On
// error the staging file is already cleaned up; on success it is the caller's
// to close and remove.
//
// Staged, not buffered: a large workspace (the _tfac tree especially) never
// sits whole in memory — scratch files are copied into the tar file by file,
// and Put reads the staged tar back incrementally rather than from a single
// in-RAM buffer. The stream is gzipped on its way to the staging file — the
// transcript and ci-logs members that dominate the blob are highly
// compressible text — without touching the member-by-member streaming inside
// writeSnapshotTar.
func stageSnapshotArchive(delta *worktree.GitDelta, wtPath, sessionID string, transcript []byte) (_ *os.File, rawBytes, gzippedBytes int64, err error) {
	f, err := os.CreateTemp("", "tf-snapshot-*.tar.gz")
	if err != nil {
		return nil, 0, 0, fmt.Errorf("tempfile: %w", err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	gzw := gzip.NewWriter(f)
	cw := countingWriter{w: gzw}
	if err = writeSnapshotTar(&cw, delta, wtPath, sessionID, transcript); err != nil {
		return nil, 0, 0, err
	}
	if err = gzw.Close(); err != nil {
		return nil, 0, 0, fmt.Errorf("close gzip: %w", err)
	}
	fi, statErr := f.Stat()
	if statErr != nil {
		err = fmt.Errorf("stat staged tar: %w", statErr)
		return nil, 0, 0, err
	}
	if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
		err = fmt.Errorf("rewind tar: %w", seekErr)
		return nil, 0, 0, err
	}
	return f, cw.n, fi.Size(), nil
}

// countingWriter counts what passes through it — the archive's raw
// (pre-compression) size, which nothing else can see: the tar stream goes
// straight into the gzip writer, and the staging file only ever holds the
// compressed result.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
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
		return fmt.Errorf("tar scratch: %w", err)
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
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := writeTarBytes(tw, snapManifest, manBytes); err != nil {
		return err
	}
	return tw.Close()
}

// gitSeed is everything a cold rehydrate's git rebuild needs about the repo it
// replays the delta onto: where the bare lives (owner/repo), the upstream URL
// that seeds one when this executor has none, and the credential that
// authenticates the network git the rebuild does. The zero value is the non-git
// run-root (a Jira/Slack lazy root), which has no bare and no delta.
//
// auth covers TWO network hops, not one — see RestoreWorkspaceGit. Seeding a
// missing bare is the obvious one; the load-bearing one is that the shared bare
// is a blobless partial clone, so the rebuild's `git worktree add` checkout
// triggers a lazy promisor fetch against origin even when the bare is already
// there. A bare that exists is not a bare that is self-sufficient.
type gitSeed struct {
	owner    string
	repo     string
	cloneURL string
	auth     worktree.CloneAuth
}

// gitSeedFor resolves the seed for a GitHub-backed run's rehydrate. The clone
// URL comes from the repository row (written in the org's configured protocol) —
// no PR is fetched on a later step or a resume, so there is no per-run URL to
// inherit.
//
// The auth is the run's own git-proxy routing whenever an executor sandbox
// exists, i.e. exactly the wiring setupGitHub does for the first clone: the
// orchestrator holds no GitHub token, and the sidecar attaches the real one on
// the upstream hop. Local mode gets the zero CloneAuth on purpose — the
// operator's ambient git credentials are its design, and there is no sidecar to
// route through.
//
// Degradations are deliberate and independent: with no profile URL the rebuild
// still authenticates (the insteadOf falls back to the org's git host base, the
// same upstream the sidecar's proxy relays to) but cannot seed a missing bare;
// with no sandbox it seeds and fetches anonymously, which is only ever local.
func (s *Spawner) gitSeedFor(ctx context.Context, orgID, owner, repo string, sidecar *runSidecar) gitSeed {
	seed := gitSeed{owner: owner, repo: repo}
	if owner == "" || repo == "" {
		return seed
	}
	if s.repos != nil {
		if profile, err := s.repos.GetByRefSystem(ctx, orgID, domain.RepoRef{Owner: owner, Repo: repo}); err != nil {
			delegateLog.Warn("load repository for workspace rehydrate failed; a missing bare cannot be seeded", "org", orgID, "repo", owner+"/"+repo, "error", err)
		} else if profile != nil {
			seed.cloneURL = profile.CloneURL
		}
	}
	upstream := seed.cloneURL
	if upstream == "" {
		upstream = s.gitHostBaseFor(ctx, orgID)
	}
	seed.auth = sidecar.GitCloneAuth(upstream)
	return seed
}

// gitHostBaseFor is the org's non-secret git host base (github.com, or a GHES
// host) — the insteadOf upstream the sidecar's git proxy relays to, and the
// fallback when no clone URL is on file. Empty when the resolver is unwired or
// the read fails, which leaves the caller's CloneAuth inert rather than pointed
// at a guessed host.
func (s *Spawner) gitHostBaseFor(ctx context.Context, orgID string) string {
	s.mu.Lock()
	resolver := s.ghResolver
	s.mu.Unlock()
	if resolver == nil {
		return ""
	}
	base, err := resolver.BaseURLFor(ctx, orgID)
	if err != nil {
		delegateLog.Warn("resolve org github base for workspace rehydrate failed; the rebuild's git will run unauthenticated", "org", orgID, "error", err)
		return ""
	}
	return base
}

// ensureWorkspace guarantees the run's worktree exists on disk before a resume
// re-invokes the agent, returning the cwd to resume in and how that tree came
// to be. Warm path: the parked worktree survived on disk (the dormancy guards
// kept it) → return it as-is, rehydrate is a no-op. Cold path: it's gone (host
// loss, /tmp wipe, or a startup sweep) → rebuild it from the durable snapshot
// and return the rebuilt path. seed locates, seeds and authenticates the bare
// the git delta replays onto; its zero value is the non-git run-root.
//
// The provenance is returned rather than inferred downstream because this is
// the only frame that knows it: past here a warm tree and a reconstruction of
// one are the same directory, and what the agent is told about its own prior
// work turns on the difference.
//
// conv.ClaimID is read, not just carried: a cold rebuild re-stamps
// worktree_path, and that write is this engagement's to make only while it
// still holds the conversation. Every caller is a claimed dispatch, so it is
// populated at both — including the config the step builder synthesizes, which
// copies it across for exactly this reason.
func (s *Spawner) ensureWorkspace(ctx context.Context, orgID string, conv *domain.Conversation, seed gitSeed) (_ string, prov domain.WorkspaceProvenance, err error) {
	// The provenance IS the interesting part of this span: a warm reuse is a
	// stat call and a cold rehydrate is a blob fetch plus a git rebuild, and
	// nothing downstream can tell them apart afterwards — past here they are
	// the same directory. Recorded from the named result so every exit below
	// carries it without restating the attribute.
	ctx, span := tracer.Start(ctx, "engagement.workspace.ensure")
	defer func() {
		if prov != "" {
			span.SetAttributes(telemetry.Workspace(string(prov)))
		}
		recordSpanError(span, err)
		span.End()
	}()

	if conv.WorktreePath != "" {
		if _, err := os.Stat(conv.WorktreePath); err == nil {
			return conv.WorktreePath, domain.WorkspaceProvenanceWarm, nil // warm: worktree still on disk
		}
	}

	blobs := s.Storage()
	if blobs == nil {
		return "", "", fmt.Errorf("worktree %q missing and no blob store to rehydrate from", conv.WorktreePath)
	}
	keyID := memoryNamespace(conv.BlueprintRunID)
	rc, err := blobs.Get(ctx, snapshotKey(orgID, keyID))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", "", fmt.Errorf("worktree %q missing and no snapshot for %s to rehydrate from", conv.WorktreePath, keyID)
		}
		return "", "", fmt.Errorf("rehydrate: get snapshot: %w", err)
	}
	defer func() { _ = rc.Close() }()

	// Rebuild at the deterministic, host-local run-root for this key (equal to
	// conv.WorktreePath on the same host; a fresh path after landing elsewhere).
	wtDir := worktree.RunRoot(keyID)
	if rErr := s.rehydrateFromSnapshot(ctx, wtDir, seed, rc); rErr != nil {
		return "", "", rErr
	}
	if wtDir != conv.WorktreePath {
		// Point the run (and the cleanup paths that key off it) at the rebuilt
		// worktree. System write — resume goroutines hold no JWT claims.
		// Non-fatal: the rebuilt path is returned and this resume proceeds. But
		// conv.WorktreePath stays stale, so the NEXT resume won't find the
		// warm copy and will cold-rehydrate again (correct, just slower) — log
		// it distinctly so unexpected repeat rehydrates are diagnosable.
		//
		// A fence refusal is excluded because that diagnosis would be wrong
		// twice over: the path is not stale, it is the successor's own, and
		// the next resume is not this engagement's to predict. setWorktreePath
		// has already logged the fact that actually happened — this executor
		// lost the conversation — and saying anything further here would file
		// a lost claim under slow rehydrates.
		if wErr := s.setWorktreePath(context.WithoutCancel(ctx), orgID, conv.ID, conv.ClaimID, wtDir); wErr != nil && !errors.Is(wErr, db.ErrClaimReleased) {
			delegateLog.Warn("rehydrate: persist new worktree_path failed; stale path will force a repeat cold rehydrate on the next resume", "worktree_path", wtDir, "conversation", conv.ID, "error", wErr)
		}
	}
	return wtDir, domain.WorkspaceProvenanceRehydrated, nil
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
func (s *Spawner) rehydrateFromSnapshot(ctx context.Context, wtDir string, seed gitSeed, r io.Reader) error {
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
		// No credential is minted here and none ever should be: the seed's auth
		// was resolved by the caller, which in multi mode routes it through the
		// run's credential sidecar (the git proxy) rather than reading a token
		// in-process. It authenticates both hops the rebuild can take — seeding
		// a bare this executor lacks, and the lazy promisor fetch the
		// worktree-add checkout triggers on the blobless bare even when the bare
		// is already here. The second is the one that bites: it needs the
		// network on every cold rehydrate of a private repo, not just a fresh
		// host, and local mode masks it because ambient git credentials answer
		// it there.
		if err := restoreWorkspaceGit(ctx, seed.owner, seed.repo, wtDir, delta, seed.cloneURL, seed.auth); err != nil {
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

// DiscardWorkspaceSnapshot is the exported seam onto the snapshot discard, for
// a caller outside the package that terminates a blueprint's work by a route of
// its own rather than through terminateBlueprint. Idempotent and nil-safe.
//
// It takes the blueprint run id, not a conversation id: the snapshot key is the
// memory namespace (see snapshotKey), so a conversation id names a blob that
// was never written and the discard silently does nothing.
func (s *Spawner) DiscardWorkspaceSnapshot(orgID, blueprintRunID string) {
	s.discardWorkspaceSnapshot(context.Background(), orgID, blueprintRunID)
}

// discardWorkspaceSnapshot deletes a parked workspace's snapshot blob once the
// run/blueprint it belonged to reaches a terminal state, so durable storage
// doesn't accumulate orphans. keyID is memoryNamespace(blueprintRunID).
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
