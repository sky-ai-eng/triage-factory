// Durable blueprint workspace: snapshot a parked run's non-recoverable
// workspace to the blob store on dormancy, and rehydrate it on resume when the
// warm on-disk worktree is gone. The object store is the source of truth; the
// host worktree is a warm cache. A parked workspace surviving locally is the
// fast path (resume uses it directly, rehydrate is a no-op); a missing one
// rebuilds from the snapshot — never a brick.
//
// Two snapshot triggers are wired today: a park to `open` — an idle
// hibernation, a turn that ended without a conclusion, or a stop
// (parkConversationOpen, live.go) — and every non-failed terminal, which after
// the terminal vocabulary shrank to completed|failed is `completed`, whatever
// the outcome (processCompletion). A third — an executor-drain/scale-down
// trigger — is a forward seam for the execution-plane split: there are no
// executors to drain yet, so it is intentionally NOT wired. When it lands it
// calls snapshotWorkspace with the same key, identically to the two triggers
// above.
//
// A park does NOT wait for its snapshot: it records that a persist is owed,
// flips the conversation, and captures afterwards. The record is what makes
// that safe — see parkConversationOpen for the ordering and workspace_wait.go
// for the resume that reads it.
//
// The write policy and the retention sweep move together, always: the reaper
// enumerates exactly the states listed above (ListReapableSnapshotKeysSystem),
// so widening one without the other either leaks blobs forever or reaps a
// workspace something still wants.

package delegate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Snapshot tar member names. The blob is one compressed tar holding the
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

// scratchExcludes are the top-level _tfac entries that come back by a route of
// their own and so never ride in the snapshot: entity-memory rebuilds from
// conversation_memory (and under a jail is not even a directory — it is the
// symlink standing in for the read-only mount, which the walk skips as
// non-regular regardless), knowledge is re-copied from the team knowledge
// store on every launch (carrying it would preserve a stale copy at the cost
// of the blob's size), and ci-logs is the extracted output of `exec gh
// actions download-logs`, which the agent re-runs to get byte-identical
// content back from GitHub. Everything else under _tfac (skill scratch,
// ad-hoc agent files, and the agent's own memory.md — which is not in the DB
// until termination ingests it) is non-recoverable and IS captured.
//
// ci-logs is the only one of the three the agent can notice missing:
// entity-memory and knowledge are both re-staged before it looks, while a full
// Actions log archive — routinely hundreds of MB to GBs of text, and the largest thing a
// park would ever compress — is re-fetched on demand rather than restored.
// So a cold rehydrate whose snapshot dropped a populated ci-logs plants
// ciLogsNotice where the logs were, rather than handing back a tree that
// quietly lost them.
var scratchExcludes = map[string]bool{
	"entity-memory":    true,
	knowledgeDirName:   true,
	worktree.CILogsDir: true,
}

// ciLogsNoticeFile is the notice a cold rehydrate leaves under ci-logs in place
// of the logs the blob did not carry, and ciLogsNotice is what it says. An
// agent resuming into a rebuilt tree may remember reading a log at that path;
// this is the difference between an explained absence and a mystery.
const ciLogsNoticeFile = "NOT-RESTORED.md"

const ciLogsNotice = `# CI logs were not restored

This workspace was rebuilt from a snapshot after its host copy was lost. A
snapshot carries only state that exists nowhere else, and extracted CI logs are
not that: they are a verbatim copy of a GitHub Actions log archive, so the same
bytes are still one download away.

Anything that was under _tfac/ci-logs/ before the rebuild is therefore gone —
the work that read it still happened. To get the identical content back:

    triagefactory exec gh actions download-logs <run_id>

That writes ./_tfac/ci-logs/<run_id>/ exactly as it was.
`

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
	// CILogsOmitted says the captured workspace held extracted CI logs that
	// this blob deliberately left out, which is what a rehydrate needs to know
	// to explain the absence in the tree it rebuilds. Absent on a snapshot
	// whose workspace never downloaded any — an empty ci-logs directory needs
	// no explanation — and absent on blobs written before the exclusion, which
	// carry their logs as ordinary scratch members and restore them normally.
	CILogsOmitted bool `json:"ci_logs_omitted,omitempty"`
}

// snapshotKey is the storage key for a parked workspace's snapshot blob. keyID
// is the blueprint_run_id — every run is a blueprint step now, so every step of
// one blueprint shares the one workspace blob. It is exactly the value
// memoryNamespace yields and the value the on-disk worktree directory is named
// after, so the key, the namespace, and the dir name stay in lockstep.
//
// The "workspace.tar" leaf is the bare tar's name; the blob is compressed inside
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
// members are not runtime-agnostic: only a delegated SDK-runtime conversation
// snapshots a session transcript, so transcript sizes read without the
// attribute would look like a property of all snapshots. Empty is a caller that doesn't know
// (a fixture), and simply omits the attribute.
//
// claimID is the engagement writing this snapshot, and it is what makes the
// blob's lifecycle knowable and its ordering safe. The write is bracketed by a
// durable state record (workspace_snapshots): 'pending' before the capture, so
// a resume that finds no blob can tell "a persist is in flight, here is who
// owes it" from "there is nothing"; then 'written' or 'failed' on the way out,
// CAS'd on this claim. The same claim id gates the upload — an engagement a
// successor has displaced skips its Put rather than overwriting the
// successor's newer blob. Empty claimID (a claimless caller, a fixture) writes
// no state and takes no guard: with no engagement to name, there is nothing a
// waiter could ask about and nothing a CAS could fence on, so the blob is
// written with its lifecycle unrecorded.
//
// Best-effort by contract: callers log and proceed on error, because the warm
// worktree (preserved on dormancy by the per-run guards) is the primary resume
// path and the snapshot is the durable backstop, only read when that cache is
// gone.
func (s *Spawner) snapshotWorkspace(ctx context.Context, orgID, conversationID, keyID, claimID, wtPath, sessionID, runtime string) error {
	return s.persistWorkspaceSnapshot(ctx, orgID, conversationID, keyID, claimID, wtPath, sessionID, runtime, false)
}

// persistWorkspaceSnapshot is snapshotWorkspace's body with the lifecycle
// bracket's opening move made optional. leaseHeld says the caller already
// opened the record and holds it, which a park does so the record exists
// before the status flip a waiter reads; false opens one here — including for
// a caller whose own open failed, since an untracked persist is precisely what
// a resume cannot read.
func (s *Spawner) persistWorkspaceSnapshot(ctx context.Context, orgID, conversationID, keyID, claimID, wtPath, sessionID, runtime string, leaseHeld bool) (err error) {
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

	// From here on a persist is owed, and that is recorded before any of the
	// work rather than after it: the record's whole purpose is to be readable
	// while the blob does not yet exist. The rejections above are deliberately
	// outside it — nothing was ever owed for a call that names no key.
	//
	// owned says this engagement holds the key's lifecycle. False for a
	// claimless caller or an unwired store, and then the branches below skip
	// the guard and the terminal write: the blob is still produced, untracked.
	//
	// The lifecycle writes run detached from cancellation — the same
	// WithoutCancel the park's own writes take, and for a sharper reason: a
	// cancelled ctx is precisely when the capture below fails, so a
	// cancellation that also swallowed the 'failed' write would leave the key
	// pending forever and a later resume waiting out its full bound on a
	// persist nobody is producing.
	stateCtx := context.WithoutCancel(ctx)
	owned := leaseHeld || s.beginSnapshotState(stateCtx, orgID, keyID, claimID)
	defer func() {
		// A durable 'failed' is what lets a waiting resume stop waiting and
		// fall back, so every error exit below lands here rather than leaving
		// the key pending forever. The two nil exits are covered elsewhere:
		// the success path writes 'written' itself, and the superseded path
		// writes nothing at all — the row is the successor's now, and its
		// outcome is the successor's to record.
		if err != nil && owned {
			s.finishSnapshotState(stateCtx, orgID, keyID, claimID, false)
		}
	}()

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
	captured, cleanupCapture, err := captureWorkspaceGit(capCtx, wtPath, sessionID)
	if err != nil {
		recordSpanError(capSpan, err)
		capSpan.End()
		return fmt.Errorf("snapshot: capture: %w", err)
	}
	if cleanupCapture != nil {
		defer func() {
			if cleanupCapture != nil {
				cleanupCapture()
			}
		}()
	}
	bundleBytes, err := capturedMemberSize(captured.Delta, captured.BundlePath, true)
	if err != nil {
		recordSpanError(capSpan, err)
		capSpan.End()
		return fmt.Errorf("snapshot: capture bundle size: %w", err)
	}
	patchBytes, err := capturedMemberSize(captured.Delta, captured.PatchPath, false)
	if err != nil {
		recordSpanError(capSpan, err)
		capSpan.End()
		return fmt.Errorf("snapshot: capture patch size: %w", err)
	}
	transcriptBytes, err := capturedBytesSize(captured.Transcript, captured.TranscriptPath)
	if err != nil {
		recordSpanError(capSpan, err)
		capSpan.End()
		return fmt.Errorf("snapshot: capture transcript size: %w", err)
	}
	capSpan.SetAttributes(
		telemetry.SnapshotBundleBytes(bundleBytes),
		telemetry.SnapshotPatchBytes(patchBytes),
		telemetry.SnapshotTranscriptBytes(transcriptBytes),
	)
	capSpan.End()

	_, archSpan := snapshotPhase(ctx, "workspace.snapshot.archive", runtime)
	f, rawBytes, compressedBytes, err := stageSnapshotArchive(captured, wtPath)
	if err != nil {
		recordSpanError(archSpan, err)
		archSpan.End()
		return fmt.Errorf("snapshot: archive: %w", err)
	}
	if cleanupCapture != nil {
		cleanupCapture()
		cleanupCapture = nil
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	// Raw bytes in against compressed bytes out: the pair is the codec's
	// report card — ratio from the two sizes, throughput from either against
	// the phase duration — so a compression change can prove itself from the
	// field rather than a benchmark.
	archSpan.SetAttributes(telemetry.SnapshotRawBytes(rawBytes), telemetry.SizeBytes(compressedBytes))
	archSpan.End()

	// Pre-Put guard: a newer engagement may have taken the key over while this
	// teardown was capturing — a cross-pod stop releases the claim the instant
	// the user asks, and the successor can be running, parking, and writing its
	// own snapshot before this one reaches the upload. Its blob is the truth,
	// so this one is not written at all.
	//
	// The guard keys on who owns the key, never on whether a successor claim
	// exists: a successor on a different executor may be waiting for exactly
	// this blob to rehydrate from, and skipping the Put there would strand it.
	//
	// Accepted race, stated so nobody "fixes" it: between this read and the Put
	// a successor could complete an entire park-and-snapshot cycle, landing the
	// older blob after the newer one. That needs a full claim -> run -> park ->
	// capture -> put inside a window measured in microseconds, and the
	// successor's next park overwrites it again. Closing it completely means
	// versioned blob keys, which changes key derivation everywhere.
	if owned && s.snapshotSuperseded(stateCtx, orgID, keyID, claimID) {
		delegateLog.Info("snapshot superseded by a newer engagement; not writing",
			"conversation", conversationID, "key", snapshotKey(orgID, keyID), "claim_id", claimID)
		return nil
	}

	putCtx, putSpan := snapshotPhase(ctx, "workspace.snapshot.put", runtime)
	putSpan.SetAttributes(telemetry.SizeBytes(compressedBytes))
	putErr := blobs.Put(putCtx, snapshotKey(orgID, keyID), f)
	recordSpanError(putSpan, putErr)
	putSpan.End()
	if putErr != nil {
		return fmt.Errorf("snapshot: put: %w", putErr)
	}
	// Parked-window storage cost is a live sizing question; log every
	// snapshot's real compressed footprint so it's answerable from the field.
	// On the span too, where it explains the duration beside it.
	span.SetAttributes(telemetry.SizeBytes(compressedBytes))
	if owned {
		s.finishSnapshotState(stateCtx, orgID, keyID, claimID, true)
	}
	delegateLog.Info("snapshot written", "key", snapshotKey(orgID, keyID), "bytes_compressed", compressedBytes)
	return nil
}

// beginSnapshotState records that claimID owes a snapshot for this key and
// reports whether this engagement now holds the key's lifecycle. False when
// there is nothing to record against — no store wired (a fixture), no claim to
// fence on (a claimless caller) — and the caller then neither guards its upload
// nor writes a terminal state.
//
// A failure to record is not a failure to snapshot: the blob is the thing the
// resume actually reads, so a store error logs and the snapshot proceeds
// untracked rather than being abandoned.
func (s *Spawner) beginSnapshotState(ctx context.Context, orgID, keyID, claimID string) bool {
	if s.workspaceSnapshots == nil || claimID == "" {
		return false
	}
	if err := s.workspaceSnapshots.BeginSnapshotSystem(ctx, orgID, keyID, claimID); err != nil {
		delegateLog.Warn("record workspace snapshot as pending failed; snapshotting untracked",
			"org", orgID, "key_id", keyID, "claim_id", claimID, "error", err)
		return false
	}
	return true
}

// finishSnapshotState closes out this engagement's write. An unmatched CAS is
// the ordinary outcome of losing the key to a successor mid-write, not an
// error: the successor owns the row and its own finish is the one that counts,
// so this says so at INFO and leaves the row alone.
func (s *Spawner) finishSnapshotState(ctx context.Context, orgID, keyID, claimID string, ok bool) {
	if s.workspaceSnapshots == nil {
		return
	}
	matched, err := s.workspaceSnapshots.FinishSnapshotSystem(ctx, orgID, keyID, claimID, ok)
	if err != nil {
		delegateLog.Warn("record workspace snapshot outcome failed",
			"org", orgID, "key_id", keyID, "claim_id", claimID, "written", ok, "error", err)
		return
	}
	if !matched {
		delegateLog.Info("workspace snapshot state was re-owned by a newer engagement; outcome not recorded",
			"org", orgID, "key_id", keyID, "claim_id", claimID, "written", ok)
	}
}

// snapshotStateFor reads one key's lifecycle row. (nil, nil) means no persist
// was ever owed — also what an unwired store reports. An error is passed up
// rather than folded into that: callers deciding whether to keep waiting have
// to tell "no record" from "cannot tell".
func (s *Spawner) snapshotStateFor(ctx context.Context, orgID, keyID string) (*domain.WorkspaceSnapshotState, error) {
	if s.workspaceSnapshots == nil {
		return nil, nil
	}
	return s.workspaceSnapshots.GetSnapshotStateSystem(ctx, orgID, keyID)
}

// snapshotSuperseded reports whether the key's lifecycle row has moved to
// another engagement since this one began. A read failure answers false — the
// guard exists to prevent an older blob overwriting a newer one, and a store
// that cannot answer is not evidence that happened; refusing the Put on it
// would strand a cross-executor resume waiting for this very blob.
func (s *Spawner) snapshotSuperseded(ctx context.Context, orgID, keyID, claimID string) bool {
	if s.workspaceSnapshots == nil {
		return false
	}
	state, err := s.workspaceSnapshots.GetSnapshotStateSystem(ctx, orgID, keyID)
	if err != nil {
		delegateLog.Warn("read workspace snapshot state before writing failed; writing anyway",
			"org", orgID, "key_id", keyID, "claim_id", claimID, "error", err)
		return false
	}
	// A vanished row is not a takeover either — a terminal cleanup or the
	// retention reaper may have dropped it, and this teardown's blob is still
	// the newest thing anyone has.
	return state != nil && state.WriterClaimID != claimID
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

// stageSnapshotArchive writes the snapshot members to a zstd-compressed tar staged on
// disk, returning the open staging file positioned at the start — ready to
// stream into Put — with its pre-compression and compressed byte counts. On
// error the staging file is already cleaned up; on success it is the caller's
// to close and remove.
//
// Staged, not buffered: a large workspace (the _tfac tree especially) never
// sits whole in memory — scratch files are copied into the tar file by file,
// and Put reads the staged tar back incrementally rather than from a single
// in-RAM buffer. The stream is compressed on its way to the staging file — the
// transcript and ci-logs members that dominate the blob are highly
// compressible text — without touching the member-by-member streaming inside
// writeSnapshotTar.
func stageSnapshotArchive(captured worktree.CapturedState, wtPath string) (_ *os.File, rawBytes, compressedBytes int64, err error) {
	f, err := os.CreateTemp("", "tf-snapshot-*.tar.zst")
	if err != nil {
		return nil, 0, 0, fmt.Errorf("tempfile: %w", err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	zw, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderCRC(true))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("open zstd: %w", err)
	}
	cw := countingWriter{w: zw}
	if err = writeSnapshotTar(&cw, captured, wtPath); err != nil {
		_ = zw.Close()
		return nil, 0, 0, err
	}
	if err = zw.Close(); err != nil {
		return nil, 0, 0, fmt.Errorf("close zstd: %w", err)
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
// straight into the compression writer, and the staging file only ever holds the
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

// writeSnapshotTar streams the snapshot members into w as one tar: the
// potentially unbounded git bundle + uncommitted patch,
// the ephemeral _tfac tree (streamed file by file), the Claude session
// transcript, and the manifest.
func writeSnapshotTar(w io.Writer, captured worktree.CapturedState, wtPath string) error {
	tw := tar.NewWriter(w)
	delta := captured.Delta
	man := snapshotManifest{SessionID: captured.SessionID}
	if delta != nil {
		man.HasGit = true
		man.Branch = delta.Branch
		man.Head = delta.Head
		if len(delta.Bundle) > 0 || captured.BundlePath != "" {
			if err := writeCapturedMember(tw, snapBundle, delta.Bundle, captured.BundlePath); err != nil {
				return err
			}
		}
		if len(delta.Patch) > 0 || captured.PatchPath != "" {
			if err := writeCapturedMember(tw, snapPatch, delta.Patch, captured.PatchPath); err != nil {
				return err
			}
		}
	}
	omittedCILogs, err := tarScratch(tw, wtPath)
	if err != nil {
		return fmt.Errorf("tar scratch: %w", err)
	}
	man.CILogsOmitted = omittedCILogs
	if captured.SessionID != "" {
		if len(captured.Transcript) > 0 || captured.TranscriptPath != "" {
			if err := writeCapturedMember(tw, snapSession, captured.Transcript, captured.TranscriptPath); err != nil {
				return err
			}
		} else {
			// The run has a session but the capture came back without its
			// transcript (absent on disk, or a capture that couldn't read it). The
			// blob is still written — worktree state matters on its own — but a
			// resume from it will hit the transcript-missing guard and fail.
			// Surface it: this is otherwise silent, and it's exactly the shape that
			// produced a resume-fails-with-no-reason report.
			delegateLog.Warn("snapshot omits session transcript; a resume of this conversation will not be able to continue where it left off", "session", captured.SessionID, "worktree", wtPath)
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

func capturedMemberSize(delta *worktree.GitDelta, path string, bundle bool) (int64, error) {
	var data []byte
	if delta != nil {
		if bundle {
			data = delta.Bundle
		} else {
			data = delta.Patch
		}
	}
	return capturedBytesSize(data, path)
}

func capturedBytesSize(data []byte, path string) (int64, error) {
	if path == "" {
		return int64(len(data)), nil
	}
	if len(data) != 0 {
		return 0, fmt.Errorf("member has both buffered bytes and a staged path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("staged member %s is not a regular file", path)
	}
	return info.Size(), nil
}

func writeCapturedMember(tw *tar.Writer, name string, data []byte, path string) error {
	if path == "" {
		return writeTarBytes(tw, name, data)
	}
	size, err := capturedBytesSize(data, path)
	if err != nil {
		return fmt.Errorf("tar staged %s: %w", name, err)
	}
	return writeTarFile(tw, name, path, size)
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
// The auth is the engagement's own git-proxy routing, matching setupGitHub's
// first clone. Multi resolves it from the credential sidecar; local resolves it
// from the loopback channel that holds the configured GitHub identity.
//
// Degradations are deliberate and independent: with no profile URL the rebuild
// still authenticates (the insteadOf falls back to the org's git host base, the
// same upstream the sidecar's proxy relays to) but cannot seed a missing bare;
// with no engagement proxy (an unwired fixture) it seeds and fetches without
// injected auth.
func (s *Spawner) gitSeedFor(ctx context.Context, orgID, owner, repo string, sidecar *runSidecar, localChannels ...*localGitChannel) gitSeed {
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
	if seed.auth == (worktree.CloneAuth{}) && len(localChannels) > 0 && localChannels[0] != nil {
		// The managed local channel speaks git-over-HTTPS even when the org's
		// stored clone preference is SSH. Rebuild from the canonical HTTPS form;
		// the agent's process-scoped rewrites cover any surviving SSH remotes.
		upstream = s.gitHostBaseFor(ctx, orgID)
		if upstream != "" {
			seed.cloneURL = strings.TrimRight(upstream, "/") + "/" + owner + "/" + repo + ".git"
		}
		seed.auth = localChannels[0].cloneAuth(upstream)
	}
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

// freshWorkspaceBuilder builds this conversation's run tree from nothing, the
// way its very first claim built it. The caller supplies it because only the
// caller knows the shape: this frame holds the snapshot seed, which can replay
// a delta onto a bare and cannot reconstruct a first launch. nil leaves the
// fallback arm unavailable.
type freshWorkspaceBuilder func(ctx context.Context) (string, error)

// ensureWorkspace guarantees the run's worktree exists on disk before a claim
// re-invokes the agent, returning the cwd to work in and how that tree came to
// be. A ladder, and each rung is a different amount of the agent's remembered
// work:
//
//   - warm — the parked worktree survived on disk (the dormancy guards kept
//     it). Returned as-is; nothing is rebuilt.
//   - rehydrated — it is gone (host loss, /tmp wipe, a startup sweep) but the
//     durable snapshot is there, so the tree is rebuilt from it. seed locates,
//     seeds and authenticates the bare the git delta replays onto; its zero
//     value is the non-git run-root.
//   - waited, then rehydrated — the snapshot is not there YET. A park flips the
//     conversation's status before writing the blob, so this is the ordinary
//     reading of a healthy run for as long as the capture takes; the lifecycle
//     record says a persist is in flight and awaitSnapshotBlob waits it out.
//   - fresh — no persist is coming (it failed, its writer died, or the wait
//     gave up). A native conversation is rebuilt from nothing and told so, its
//     continuity being the transcript rather than the tree. An SDK
//     conversation cannot: its continuity WAS the session file inside the
//     blob, so it gets the expired answer instead.
//
// The provenance is returned rather than inferred downstream because this is
// the only frame that knows it: past here a warm tree and a reconstruction of
// one are the same directory, and what the agent is told about its own prior
// work turns on the difference.
//
// conv.ClaimID is read, not just carried: a rebuild re-stamps worktree_path,
// and that write is this engagement's to make only while it still holds the
// conversation. Every caller is a claimed dispatch, so it is populated at both
// — including the config the step builder synthesizes, which copies it across
// for exactly this reason.
func (s *Spawner) ensureWorkspace(ctx context.Context, orgID string, conv *domain.Conversation, seed gitSeed, fresh freshWorkspaceBuilder) (_ string, prov domain.WorkspaceProvenance, err error) {
	// The provenance IS the interesting part of this span — nothing downstream
	// can tell the three rungs apart, since past here they are the same
	// directory. Recorded from the named result so every exit below carries it
	// without restating the attribute, with the wait beside it: a resume that
	// sat out most of a minute behind a hung writer otherwise reads identically
	// to one that walked straight through.
	ctx, span := tracer.Start(ctx, "engagement.workspace.ensure")
	defer func() {
		if prov != "" {
			span.SetAttributes(telemetry.Workspace(string(prov)))
		}
		recordSpanError(span, err)
		span.End()
	}()

	// Held across the whole resolution, warm stat included: the eviction sweep
	// runs in this same process and deletes exactly this tree, so a warm hit
	// taken outside the lock could be a directory that no longer exists by the
	// time the agent runs in it. Under the lock the sweep either goes first
	// (this resolution then cold-rehydrates, which is correct and merely
	// slower) or finds this engagement's claim on its re-check and declines.
	keyID := memoryNamespace(conv.BlueprintRunID)
	unlock := s.workspaceLocks.lock(workspaceLockKey(orgID, keyID))
	defer unlock()

	if conv.WorktreePath != "" {
		if _, err := os.Stat(conv.WorktreePath); err == nil {
			return conv.WorktreePath, domain.WorkspaceProvenanceWarm, nil // warm: worktree still on disk
		}
	}

	blobs := s.Storage()
	if blobs == nil {
		return "", "", fmt.Errorf("worktree %q missing and no blob store to rehydrate from", conv.WorktreePath)
	}
	rc, err := blobs.Get(ctx, snapshotKey(orgID, keyID))
	if errors.Is(err, storage.ErrNotFound) {
		// Not there yet, or not there at all — the lifecycle record is what
		// separates those, and the wait is where that question is asked.
		appeared, waited := s.awaitSnapshotBlob(ctx, orgID, keyID)
		span.SetAttributes(telemetry.SnapshotWaitedMs(waited.Milliseconds()))
		if !appeared {
			return s.workspaceFromNothing(ctx, orgID, conv, keyID, fresh)
		}
		rc, err = blobs.Get(ctx, snapshotKey(orgID, keyID))
		if err != nil {
			// It existed a moment ago and now does not read: a successor's
			// discard, or a store fault. Either way there is nothing to
			// rehydrate from, so this takes the same answer the wait's own
			// failure would have.
			delegateLog.Warn("rehydrate: the snapshot the wait saw could not be fetched; falling back",
				"conversation", conv.ID, "key_id", keyID, "error", err)
			return s.workspaceFromNothing(ctx, orgID, conv, keyID, fresh)
		}
	} else if err != nil {
		return "", "", fmt.Errorf("rehydrate: get snapshot: %w", err)
	}
	defer func() { _ = rc.Close() }()

	// Rebuild at the deterministic, host-local run-root for this key (equal to
	// conv.WorktreePath on the same host; a fresh path after landing elsewhere).
	wtDir := worktree.RunRoot(keyID)
	if rErr := s.rehydrateFromSnapshot(ctx, wtDir, seed, rc); rErr != nil {
		return "", "", rErr
	}
	s.restampWorktreePath(ctx, orgID, conv, wtDir)
	return wtDir, domain.WorkspaceProvenanceRehydrated, nil
}

// workspaceFromNothing is the ladder's last rung: there is no workspace to
// recover and none is coming. The runtime decides what happens next, and that
// is a fact about where each engine keeps the conversation rather than a
// preference. A native one is replayed from its `messages` rows into whatever
// tree it is given, so a workspace built from nothing is a real if lossier
// continuation — the fresh provenance is what tells the agent which. An SDK
// one is resumed by session id against a transcript that lived inside the
// blob, so without it there is nothing to reconnect to.
//
// A caller with no builder gets the refusal whatever the runtime; the SDK
// resume dispatch is the one such caller, and it could not use this arm anyway.
func (s *Spawner) workspaceFromNothing(ctx context.Context, orgID string, conv *domain.Conversation, keyID string, fresh freshWorkspaceBuilder) (string, domain.WorkspaceProvenance, error) {
	if resumeSourceFor(conv.Runtime) != resumeSourceMessages || fresh == nil {
		return "", "", fmt.Errorf("worktree %q missing and no snapshot for %s to rehydrate from: %w", conv.WorktreePath, keyID, ErrWorkspaceExpired)
	}
	delegateLog.Warn("no workspace to recover; building this conversation a fresh one — uncommitted work from the prior engagement is lost",
		"conversation", conv.ID, "key_id", keyID, "org", orgID)
	wtDir, err := fresh(ctx)
	if err != nil {
		return "", "", fmt.Errorf("build fresh workspace for %s: %w", keyID, err)
	}
	s.restampWorktreePath(ctx, orgID, conv, wtDir)
	return wtDir, domain.WorkspaceProvenanceFresh, nil
}

// restampWorktreePath points the conversation (and the cleanup paths that key
// off it) at a tree this engagement just rebuilt. System write — claim
// goroutines hold no JWT claims. Non-fatal: the rebuilt path is returned
// either way, but a stale conv.WorktreePath costs the NEXT claim a repeat
// rebuild, so the failure is logged distinctly to keep that diagnosable.
//
// A fence refusal is excluded because that diagnosis would be wrong twice
// over: the path is not stale, it is the successor's own, and the next claim
// is not this engagement's to predict. setWorktreePath has already logged the
// thing that actually happened — this executor lost the conversation.
func (s *Spawner) restampWorktreePath(ctx context.Context, orgID string, conv *domain.Conversation, wtDir string) {
	if wtDir == conv.WorktreePath {
		return
	}
	if err := s.setWorktreePath(context.WithoutCancel(ctx), orgID, conv.ID, conv.ClaimID, wtDir); err != nil && !errors.Is(err, db.ErrClaimReleased) {
		delegateLog.Warn("persist rebuilt worktree_path failed; stale path will force a repeat rebuild on the next claim", "worktree_path", wtDir, "conversation", conv.ID, "error", err)
	}
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

	// New snapshots use zstd, but existing gzip blobs remain durable state and
	// must stay readable across the format migration. Sniff the outer framing;
	// the manifest that describes the tar is inside the compressed stream.
	cr, codec, err := snapshotReader(r)
	if err != nil {
		return fmt.Errorf("rehydrate: open compressed snapshot: %w", err)
	}
	defer func() { _ = cr.Close() }()
	tr := tar.NewReader(cr)
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

	// The codecs validate their checksum only when the stream is read to
	// its footer, but the tar reader stops at the archive's end-of-archive
	// marker — which precedes that footer — so member reads alone never trigger
	// the check (and gzip.Reader.Close does not force it either). Drain the
	// remainder (a few trailing bytes for our own writes) to validate the whole
	// blob, and do it here, before any worktree mutation: a checksum mismatch
	// means the snapshot is corrupt, so fail the rehydrate rather than rebuild
	// onto untrustworthy state.
	if _, err := io.Copy(io.Discard, cr); err != nil {
		return fmt.Errorf("rehydrate: %s integrity: %w", codec, err)
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
		// host. Each mode therefore supplies its engagement's proxy route.
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
	if man.CILogsOmitted {
		// AFTER the scratch install, for the same reason the memory symlink is:
		// the install is a wholesale rename onto _tfac, which fails outright if
		// this notice's parent already exists. Best-effort — the notice
		// explains an absence, and failing the resume over it would trade a
		// missing explanation for a missing workspace.
		if err := plantCILogsNotice(wtDir); err != nil {
			delegateLog.Warn("plant ci-logs notice on rehydrated tree failed; an agent that read a log under _tfac/ci-logs before the rebuild will find no explanation for its absence", "dir", wtDir, "error", err)
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

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// snapshotReader chooses a decoder from the blob's outer magic bytes. Anything
// other than zstd is handed to gzip so old snapshots retain gzip's useful,
// specific header errors rather than acquiring an ambiguous format error.
func snapshotReader(r io.Reader) (io.ReadCloser, string, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(len(zstdMagic))
	if err != nil {
		return nil, "", err
	}
	if bytes.Equal(magic, zstdMagic) {
		zr, err := zstd.NewReader(br)
		if err != nil {
			return nil, "", err
		}
		return zr.IOReadCloser(), "zstd", nil
	}
	gzr, err := gzip.NewReader(br)
	if err != nil {
		return nil, "", err
	}
	return gzr, "gzip", nil
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
//
// The lifecycle record goes with the blob. A discard that dropped only the blob
// would leave a 'written' row pointing at nothing — manufacturing the one state
// a resume has no honest reading of — where dropping both says the truth: this
// key has no snapshot lifecycle at all.
func (s *Spawner) discardWorkspaceSnapshot(ctx context.Context, orgID, keyID string) {
	if keyID == "" {
		return
	}
	if blobs := s.Storage(); blobs != nil {
		if err := blobs.Delete(ctx, snapshotKey(orgID, keyID)); err != nil {
			delegateLog.Warn("discard workspace snapshot failed", "org", orgID, "key_id", keyID, "error", err)
		}
	}
	if s.workspaceSnapshots != nil {
		if err := s.workspaceSnapshots.DeleteSnapshotStateSystem(ctx, orgID, keyID); err != nil {
			delegateLog.Warn("discard workspace snapshot state failed", "org", orgID, "key_id", keyID, "error", err)
		}
	}
}

// tarScratch walks wtPath/_tfac and writes every regular file under the
// snapScratchPrefix, skipping the scratchExcludes subtrees. A missing _tfac is
// fine (nothing to capture).
//
// It reports whether a ci-logs subtree with something in it was skipped, which
// is the manifest's CILogsOmitted and the gate on the rehydrate's notice. An
// empty ci-logs directory reports false: nothing was dropped, so there is
// nothing to explain.
func tarScratch(tw *tar.Writer, wtPath string) (omittedCILogs bool, err error) {
	root := filepath.Join(wtPath, worktree.ScratchDir)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return false, nil
	}
	err = filepath.Walk(root, func(path string, fi os.FileInfo, walkErr error) error {
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
				if rel == worktree.CILogsDir && dirHasEntry(path) {
					omittedCILogs = true
				}
				return filepath.SkipDir
			}
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil // directories implied by their files; skip symlinks/etc.
		}
		// Stream each file into the tar rather than reading it whole — the
		// agent's own intermediates are unbounded even with the log archives
		// excluded.
		return writeTarFile(tw, snapScratchPrefix+filepath.ToSlash(rel), path, fi.Size())
	})
	return omittedCILogs, err
}

// dirHasEntry reports whether path holds at least one entry, reading only far
// enough to answer rather than listing a directory the walk is about to skip.
// A read failure answers false: this decides whether to explain an absence, and
// a directory that cannot be read is not evidence that anything was dropped.
func dirHasEntry(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(1)
	return err == nil && len(names) > 0
}

// plantCILogsNotice writes ciLogsNotice into the rehydrated tree's ci-logs
// directory, recreating that directory since the snapshot carried none of it.
func plantCILogsNotice(wtDir string) error {
	dir := filepath.Join(wtDir, worktree.ScratchDir, worktree.CILogsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir ci-logs: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, ciLogsNoticeFile), []byte(ciLogsNotice), 0o600)
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
// in-memory buffer. Local capture members and the manifest use this path;
// multi capture members and _tfac files stream through writeTarFile.
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
