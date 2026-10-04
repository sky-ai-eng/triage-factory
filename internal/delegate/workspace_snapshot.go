// Durable blueprint workspace: snapshot an engagement's non-recoverable
// workspace to the blob store, and rehydrate it on resume when the warm
// on-disk worktree is gone. The object store is the source of truth; the host
// worktree is a warm cache. A workspace surviving locally is the fast path
// (resume uses it directly, rehydrate is a no-op); a missing one rebuilds from
// the snapshot — never a brick.
//
// Three kinds of moment write a snapshot, all under the task's key:
//
//   - an engagement letting go of a conversation it has not concluded
//     (leaveConversation, live.go): a park to `open` — a turn that ended
//     without a conclusion, or a stop by a person or the stall watchdog — and
//     the hand-backs that leave the conversation mid-flight, when the
//     dispatcher is shutting down or the model provider stayed unavailable;
//   - every non-failed terminal, `completed` whatever the outcome, before the
//     terminal write (processCompletion, recordNativeResult);
//   - a checkpoint of a live native engagement at a tool-batch boundary
//     (checkpoint.go).
//
// A park or a hand-back does NOT wait for its snapshot: it records that a
// persist is owed, releases the claim, and captures afterwards. The record is
// what makes that safe — see leaveConversation for the ordering and
// workspace_wait.go for the resume that reads it.
//
// The write policy and the retention sweep move together, always — and the
// sweep is the wider of the two on purpose: it enumerates every top-level
// conversation on the key (ListReapableSnapshotKeysSystem) rather than the
// states listed above, so a blob can never end up in a state the only thing
// that collects it does not look at. It is also the ONLY thing that drops a
// blob: no terminal discards one, because the key is the task and a blueprint
// reaching its end is not the task reaching its own. What the two must keep
// agreeing on is the key itself: both address the task, and a write under any
// other key would put blobs where retention never looks.

package delegate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Snapshot tar member names. The blob is one compressed tar holding a
// manifest — always the first member — each checkout's git delta, the
// ephemeral _tfac tree, and the Claude session transcript; rehydrate demuxes by
// these names.
const (
	snapManifest = "manifest.json"
	snapSession  = "session.jsonl"
	// snapScratchPrefix names the scratch members INSIDE the blob, which is a
	// storage format rather than a path: it is deliberately not derived from the
	// on-disk directory name, so renaming that directory never strands the
	// snapshot of a parked run written by an earlier build.
	snapScratchPrefix = "scratch/"
	// snapCheckoutsPrefix names a checkout's members:
	// checkouts/<path relative to the root>/bundle and .../patch.
	snapCheckoutsPrefix = "checkouts/"
)

// snapshotLayoutVersion is the run-tree layout this binary writes and the only
// one it restores: a plain run root with every checkout beneath it. A blob
// without it was written for a tree whose PR checkout sat at the root, and is
// treated as no snapshot at all rather than converted.
const snapshotLayoutVersion = 2

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

That writes <run_id>/ back into this directory exactly as it was, from
whichever directory you run it in.
`

// restoreCheckout is the git half of a cold rehydrate, once per checkout. A
// package var, in the same spirit as worktreePushTargetBranch: the credential a
// rehydrate hands git is the thing that broke, and a test that only reads the
// rebuilt tree cannot see it. Swapping this lets a test assert what each
// rebuild would run under without standing up an authenticating remote.
var restoreCheckout = worktree.RestoreCheckout

// snapshotManifest is the small header describing what a snapshot blob carries,
// read first on rehydrate to decide how to reconstruct.
type snapshotManifest struct {
	// LayoutVersion is snapshotLayoutVersion; a blob without it is refused.
	LayoutVersion int `json:"layout_version,omitempty"`
	// Checkouts are the checkouts under the root the blob carries, in path
	// order.
	Checkouts []manifestCheckout `json:"checkouts,omitempty"`
	SessionID string             `json:"session_id"`
	// CILogsOmitted says the captured workspace held extracted CI logs that
	// this blob deliberately left out, which is what a rehydrate needs to know
	// to explain the absence in the tree it rebuilds. Absent on a snapshot
	// whose workspace never downloaded any — an empty ci-logs directory needs
	// no explanation — and absent on blobs written before the exclusion, which
	// carry their logs as ordinary scratch members and restore them normally.
	CILogsOmitted bool `json:"ci_logs_omitted,omitempty"`
	// CapturedAt is when the tree was read. Absent on blobs written before it
	// was recorded.
	CapturedAt time.Time `json:"captured_at,omitzero"`
	// TranscriptPosition is the transcript ordering key (COALESCE(seq, id)) of
	// the last row whose effects the captured tree carries, and ConversationID
	// is the conversation whose transcript it is a position in. A checkpoint
	// taken mid-engagement records both. A park or a conclusion covers the
	// whole transcript and records neither, and so does every blob written
	// before checkpoints existed.
	//
	// The pair travels together because the blob is the task's: a later
	// step's conversation can restore it, and a position in another
	// conversation's transcript says nothing about its own.
	TranscriptPosition *float64 `json:"transcript_position,omitempty"`
	ConversationID     string   `json:"conversation_id,omitempty"`
	// Fingerprint (snapshotFingerprint) and WriterClaimID identify the tree a
	// checkpoint stored and the engagement that stored it, so a later position
	// the key's lifecycle row records for that tree can be told apart from one
	// recorded for any other. A checkpoint records both; nothing else does.
	Fingerprint   string `json:"fingerprint,omitempty"`
	WriterClaimID string `json:"writer_claim_id,omitempty"`
}

// manifestCheckout is one checkout in a snapshot: which repo and slug it is,
// where under the root it sits, the HEAD and branch it was on, and the names of
// its delta's members ("" when the capture carried none).
type manifestCheckout struct {
	RepoID string `json:"repo_id"`
	Slug   string `json:"slug"`
	Path   string `json:"path"`
	Head   string `json:"head"`
	Branch string `json:"branch,omitempty"`
	Bundle string `json:"bundle,omitempty"`
	Patch  string `json:"patch,omitempty"`
}

// positionFor is the transcript position this manifest's tree reflects for
// conversationID, or nil when it reflects the whole transcript — a snapshot
// taken at an ending, or a checkpoint of some other conversation on the task.
//
// st is the key's lifecycle row, or nil. A checkpoint that found the tree
// unchanged records a later position there instead of rewriting the blob
// (CoverSnapshotSystem), and it is the answer when it names this tree and the
// engagement that wrote it: the tree has not changed since, so it reflects the
// transcript that far. Any other row describes a different blob and is
// ignored, which leaves the manifest's own position, never a later one than
// the tree carries.
func (m snapshotManifest) positionFor(conversationID string, st *domain.WorkspaceSnapshotState) *float64 {
	if m.TranscriptPosition == nil || m.ConversationID != conversationID {
		return nil
	}
	pos := *m.TranscriptPosition
	if st != nil && st.CoveredPosition != nil && m.Fingerprint != "" &&
		st.CoveredFingerprint == m.Fingerprint && st.WriterClaimID == m.WriterClaimID &&
		*st.CoveredPosition > pos {
		pos = *st.CoveredPosition
	}
	return &pos
}

// snapshotKey is the storage key for a parked workspace's snapshot blob. keyID
// is the task id — every conversation on a task works in the one tree and
// shares the one workspace blob. It is exactly the value workspaceKey yields
// and the value the on-disk worktree directory is named after, so the key, the
// workspace key, and the dir name stay in lockstep.
//
// The snapshotBlobLeaf is the bare tar's name; the blob is compressed inside
// it. Keeping the leaf avoids a dual-key dance at every discard/delete site for
// zero benefit — nothing reads the key's extension to decide the format.
func snapshotKey(orgID, keyID string) string {
	return orgID + "/" + keyID + "/" + snapshotBlobLeaf
}

// snapshotBlobLeaf is the last segment of every snapshot key. Named because
// the boot re-key reads a key back apart (snapshotKeyID) and the two spellings
// have to agree or it recognizes none of the blobs it is there to move.
const snapshotBlobLeaf = "workspace.tar"

// Why a snapshot is written, on the workspace.snapshot span. An ending covers
// the whole transcript; a checkpoint covers it up to a position.
const (
	snapshotReasonPark       = "park"
	snapshotReasonConclusion = "conclusion"
	snapshotReasonShutdown   = "shutdown"
	snapshotReasonUpstream   = "upstream"
	snapshotReasonCheckpoint = "checkpoint"
)

// snapshotWrite names one persist: whose tree, under which key, for which
// engagement, and why.
type snapshotWrite struct {
	orgID, conversationID, keyID, claimID string
	wtPath, sessionID                     string
	// runtime is the conversation's engine (domain.ConversationRuntimeSDK |
	// ConversationRuntimeNative), carried onto the span family because the
	// blob's members are not runtime-agnostic: only a delegated SDK-runtime
	// conversation snapshots a session transcript, so transcript sizes read
	// without the attribute would look like a property of all snapshots. Empty
	// is a caller that doesn't know (a fixture), and simply omits the
	// attribute.
	runtime string
	// reason is one of the snapshotReason values.
	reason string
	// position is the transcript position a checkpoint's capture covers, and
	// nil for an ending, which covers all of it.
	position *float64
	// fingerprint is the checkpoint's snapshotFingerprint of the capture, ""
	// for an ending.
	fingerprint string
	// checkouts are the checkouts under wtPath this persist captures, resolved
	// from the task's conversation_worktrees rows right before the capture
	// (snapshotCheckouts).
	checkouts []snapshotCheckout
}

// snapshotCheckout is one checkout a snapshot captures.
type snapshotCheckout struct {
	repoID, slug string
	// rel is the checkout's path relative to the run root, slash-separated:
	// <owner>/<repo>/<slug>.
	rel string
	// path is the checkout's absolute host path.
	path string
}

// snapshotCheckouts resolves the checkouts a snapshot of root carries: the
// conversation_worktrees rows of every conversation on the task, since a
// blueprint's steps share one root. Deduplicated by path, keeping only those
// that exist and sit at <root>/<owner>/<repo>/<slug> as their row names them,
// in path order.
//
// A failed read is an error, not an empty set: a blob that silently lost every
// checkout would overwrite one that had them.
func (s *Spawner) snapshotCheckouts(ctx context.Context, orgID, taskID, root string) ([]snapshotCheckout, error) {
	if s.conversationWorktrees == nil || taskID == "" || root == "" {
		return nil, nil
	}
	rows, err := s.conversationWorktrees.ListForTaskSystem(ctx, orgID, taskID)
	if err != nil {
		return nil, fmt.Errorf("list the task's checkouts: %w", err)
	}
	seen := map[string]bool{}
	var out []snapshotCheckout
	for _, w := range rows {
		owner, repo := parseOwnerRepo(w.RepoID)
		rel := path.Join(owner, repo, w.Ref)
		if owner == "" || repo == "" || !validCheckoutRel(rel, w.RepoID, w.Ref) || filepath.Join(root, filepath.FromSlash(rel)) != filepath.Clean(w.Path) {
			// Another root's row (a step that ran on another host) or one
			// this layout never writes.
			continue
		}
		if seen[rel] {
			continue
		}
		fi, err := os.Lstat(w.Path)
		if err != nil || !fi.IsDir() {
			continue
		}
		seen[rel] = true
		out = append(out, snapshotCheckout{repoID: w.RepoID, slug: w.Ref, rel: rel, path: w.Path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

// validCheckoutRel reports whether rel is exactly <owner>/<repo>/<slug> for
// repoID and slug, every segment a plain name — the one place a checkout may
// sit under a root, and so the one shape a manifest entry may name.
func validCheckoutRel(rel, repoID, slug string) bool {
	owner, repo, ok := strings.Cut(repoID, "/")
	if !ok {
		return false
	}
	for _, seg := range []string{owner, repo, slug} {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "/\\\x00") || strings.HasPrefix(seg, "-") {
			return false
		}
	}
	if _, _, ok := worktree.ParseCheckoutSlug(slug); !ok {
		return false
	}
	return rel == owner+"/"+repo+"/"+slug
}

// snapshotWorkspace writes a finished engagement's non-recoverable workspace
// state — the git delta, the ephemeral _tfac subdirs, and the Claude session
// transcript — to durable storage under the run's snapshot key, so a resume
// that lands without the warm on-disk worktree can rebuild it (see
// ensureWorkspace). It runs identically in both modes: local writes the same
// blob through fsStorage under the state-root, multi through the object store.
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
	return s.persistWorkspaceSnapshot(ctx, snapshotWrite{
		orgID: orgID, conversationID: conversationID, keyID: keyID, claimID: claimID,
		wtPath: wtPath, sessionID: sessionID, runtime: runtime, reason: snapshotReasonConclusion,
	}, false)
}

// persistWorkspaceSnapshot is an ending's whole persist: open the record,
// capture, archive, upload, close the record. leaseHeld says the caller
// already opened the record and holds it, which a park does so the record
// exists before the status flip a waiter reads; false opens one here —
// including for a caller whose own open failed, since an untracked persist is
// precisely what a resume cannot read.
//
// A checkpoint runs the same three phases in a different order around the
// record (checkpoint.go), which is why they are separate functions.
func (s *Spawner) persistWorkspaceSnapshot(ctx context.Context, w snapshotWrite, leaseHeld bool) (err error) {
	blobs := s.Storage()
	if blobs == nil {
		return nil // no store wired (tests / a configuration without the seam)
	}

	ctx, span := s.startSnapshotSpan(ctx, w)
	defer func() {
		recordSpanError(span, err)
		span.End()
	}()

	if w.keyID == "" {
		return fmt.Errorf("snapshot: empty key id")
	}
	if w.wtPath == "" {
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
	// A newer engagement already holding the key ends the persist here, with
	// nothing written.
	//
	// The lifecycle writes run detached from cancellation — the same
	// WithoutCancel the park's own writes take, and for a sharper reason: a
	// cancelled ctx is precisely when the capture below fails, so a
	// cancellation that also swallowed the 'failed' write would leave the key
	// pending forever and a later resume waiting out its full bound on a
	// persist nobody is producing.
	stateCtx := context.WithoutCancel(ctx)
	owned := leaseHeld
	if !leaseHeld {
		record := s.beginSnapshotState(stateCtx, w.orgID, w.keyID, w.claimID)
		if record == snapshotSuperseded {
			return nil
		}
		owned = record.owned()
	}
	defer func() {
		// A durable 'failed' is what lets a waiting resume stop waiting and
		// fall back, so every error exit below lands here rather than leaving
		// the key pending forever. The two nil exits are covered elsewhere:
		// the success path writes 'written' itself, and the superseded path
		// writes nothing at all — the row is the successor's now, and its
		// outcome is the successor's to record.
		if err != nil && owned {
			s.finishSnapshotState(stateCtx, w.orgID, w.keyID, w.claimID, false)
		}
	}()

	if w.checkouts, err = s.snapshotCheckouts(stateCtx, w.orgID, w.keyID, w.wtPath); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	captured, err := captureSnapshot(ctx, w)
	if err != nil {
		return err
	}
	defer captured.release()
	staged, err := archiveSnapshot(ctx, w, captured)
	// The capture's staging is spent once the archive has read it, so it is
	// released before the upload rather than held across it.
	captured.release()
	if err != nil {
		return err
	}
	defer staged.discard()

	written, err := s.uploadSnapshot(ctx, w, staged, owned)
	if err != nil || !written {
		return err
	}
	span.SetAttributes(telemetry.SizeBytes(staged.compressedBytes))
	if owned {
		s.finishSnapshotState(stateCtx, w.orgID, w.keyID, w.claimID, true)
	}
	return nil
}

// startSnapshotSpan opens the workspace.snapshot span for one persist.
//
// Punctual and linked, not a child: this runs at a park, a terminal or a
// checkpoint, arbitrarily long after the engagement's setup span ended. It is
// also the one piece of run teardown with an unbounded cost — a git bundle, a
// tar of the whole scratch tree, and a blob PUT — so a park that took a minute
// is answerable here rather than only in the log. The three phase children
// split that answer: whether the time went to the capture, the compression,
// or the upload decides three different fixes. The reason separates the
// checkpoints, which run beside a live agent, from the endings.
func (s *Spawner) startSnapshotSpan(ctx context.Context, w snapshotWrite) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{telemetry.OrgID(w.orgID)}
	if w.runtime != "" {
		attrs = append(attrs, telemetry.Runtime(w.runtime))
	}
	if w.reason != "" {
		attrs = append(attrs, telemetry.Reason(w.reason))
	}
	return s.startPunctual(ctx, w.conversationID, "workspace.snapshot", attrs...)
}

// capturedSnapshot is a tree read and not yet archived: the run root's
// transcript, each checkout's git delta, the members staged on disk in multi
// mode, and when the tree was read.
type capturedSnapshot struct {
	// state is the run root's capture: the session and its transcript. The
	// root has no git delta of its own.
	state     worktree.CapturedState
	checkouts []capturedCheckout
	at        time.Time
	cleanups  []func()
}

// capturedCheckout is one checkout's capture: its delta, with the members
// either buffered on it or staged at its paths.
type capturedCheckout struct {
	snapshotCheckout
	state worktree.CapturedState
}

// release removes the capture's staging. Idempotent, and safe on a capture
// that staged nothing.
func (c *capturedSnapshot) release() {
	if c == nil {
		return
	}
	for _, cleanup := range c.cleanups {
		if cleanup != nil {
			cleanup()
		}
	}
	c.cleanups = nil
}

// captureSnapshot reads the tree's non-recoverable state — the session
// transcript at the root, and the git delta of every checkout w names. In multi
// mode each read runs inside a dropped-privilege, network-isolated child
// running as the sandbox uid: the git capture's filter-honoring commands never
// execute agent-planted drivers as root, and the SDK's owner-only transcript
// is readable there when it is not to the orchestrator (see
// captureWorkspaceGit). That child is one of the privileged operations that
// never trace themselves, so this executor-side span IS its measurement; in
// local mode the same span covers the in-process capture.
//
// The capture stages into a throwaway index (captureUncommittedTo) and reads
// status without optional locks, so it takes nothing a live agent's git
// holds: a checkpoint can run it beside the agent.
func captureSnapshot(ctx context.Context, w snapshotWrite) (_ *capturedSnapshot, err error) {
	capCtx, capSpan := snapshotPhase(ctx, "workspace.snapshot.capture", w.runtime)
	defer func() {
		recordSpanError(capSpan, err)
		capSpan.End()
	}()
	captured := &capturedSnapshot{at: time.Now()}
	defer func() {
		if err != nil {
			captured.release()
		}
	}()

	state, cleanup, err := captureWorkspaceGit(capCtx, w.wtPath, w.sessionID)
	captured.cleanups = append(captured.cleanups, cleanup)
	if err != nil {
		return nil, fmt.Errorf("snapshot: capture: %w", err)
	}
	// Only the transcript is the root's: it is a plain folder, and a delta
	// read off one an older binary laid out is not this layout's to carry.
	state.Delta, state.BundlePath, state.PatchPath = nil, "", ""
	captured.state = state

	var bundleBytes, patchBytes int64
	for _, co := range w.checkouts {
		coState, coCleanup, err := captureWorkspaceGit(capCtx, co.path, "")
		captured.cleanups = append(captured.cleanups, coCleanup)
		if err != nil {
			return nil, fmt.Errorf("snapshot: capture checkout %s: %w", co.rel, err)
		}
		if coState.Delta == nil {
			// A checkout whose .git is gone has no HEAD a restore could take
			// it back to; the directory is all that is left of it.
			delegateLog.Warn("snapshot: checkout is no longer a git worktree; it is not carried", "checkout", co.rel, "path", co.path)
			continue
		}
		b, err := capturedMemberSize(coState.Delta, coState.BundlePath, true)
		if err != nil {
			return nil, fmt.Errorf("snapshot: capture %s bundle size: %w", co.rel, err)
		}
		p, err := capturedMemberSize(coState.Delta, coState.PatchPath, false)
		if err != nil {
			return nil, fmt.Errorf("snapshot: capture %s patch size: %w", co.rel, err)
		}
		bundleBytes += b
		patchBytes += p
		captured.checkouts = append(captured.checkouts, capturedCheckout{snapshotCheckout: co, state: coState})
	}
	transcriptBytes, err := capturedBytesSize(state.Transcript, state.TranscriptPath)
	if err != nil {
		return nil, fmt.Errorf("snapshot: capture transcript size: %w", err)
	}
	capSpan.SetAttributes(
		telemetry.SnapshotBundleBytes(bundleBytes),
		telemetry.SnapshotPatchBytes(patchBytes),
		telemetry.SnapshotTranscriptBytes(transcriptBytes),
		telemetry.Count(len(captured.checkouts)),
	)
	return captured, nil
}

// stagedSnapshot is an archived blob on local disk, ready to upload.
type stagedSnapshot struct {
	f                         *os.File
	rawBytes, compressedBytes int64
}

// discard closes and removes the staged file. Idempotent.
func (st *stagedSnapshot) discard() {
	if st == nil || st.f == nil {
		return
	}
	_ = st.f.Close()
	_ = os.Remove(st.f.Name())
	st.f = nil
}

// archiveSnapshot compresses the capture and the tree's _tfac scratch into one
// staged blob. It still reads the tree — the scratch is walked here, not in
// the capture — so a checkpoint counts it as part of its quiet-point work.
func archiveSnapshot(ctx context.Context, w snapshotWrite, captured *capturedSnapshot) (_ *stagedSnapshot, err error) {
	_, archSpan := snapshotPhase(ctx, "workspace.snapshot.archive", w.runtime)
	defer func() {
		recordSpanError(archSpan, err)
		archSpan.End()
	}()
	man := snapshotManifest{CapturedAt: captured.at.UTC()}
	if w.position != nil {
		pos := *w.position
		man.TranscriptPosition = &pos
		man.ConversationID = w.conversationID
		man.Fingerprint = w.fingerprint
		man.WriterClaimID = w.claimID
	}
	f, rawBytes, compressedBytes, err := stageSnapshotArchive(ctx, captured, w.wtPath, man)
	if err != nil {
		return nil, fmt.Errorf("snapshot: archive: %w", err)
	}
	// Raw bytes in against compressed bytes out: the pair is the codec's
	// report card — ratio from the two sizes, throughput from either against
	// the phase duration — so a compression change can prove itself from the
	// field rather than a benchmark.
	archSpan.SetAttributes(telemetry.SnapshotRawBytes(rawBytes), telemetry.SizeBytes(compressedBytes))
	return &stagedSnapshot{f: f, rawBytes: rawBytes, compressedBytes: compressedBytes}, nil
}

// uploadSnapshot puts a staged blob under the key, reporting false with no
// error when a newer engagement has taken the key over and the blob is not
// written at all. It reads only the staged file, never the tree.
//
// owned says this engagement opened the key's record and so may ask whether
// it still holds it.
func (s *Spawner) uploadSnapshot(ctx context.Context, w snapshotWrite, staged *stagedSnapshot, owned bool) (written bool, err error) {
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
	if owned && s.snapshotSuperseded(context.WithoutCancel(ctx), w.orgID, w.keyID, w.claimID) {
		delegateLog.Info("snapshot superseded by a newer engagement; not writing",
			"conversation", w.conversationID, "key", snapshotKey(w.orgID, w.keyID), "claim_id", w.claimID, "reason", w.reason)
		return false, nil
	}

	putCtx, putSpan := snapshotPhase(ctx, "workspace.snapshot.put", w.runtime)
	putSpan.SetAttributes(telemetry.SizeBytes(staged.compressedBytes))
	putErr := s.Storage().Put(putCtx, snapshotKey(w.orgID, w.keyID), staged.f)
	recordSpanError(putSpan, putErr)
	putSpan.End()
	if putErr != nil {
		return false, fmt.Errorf("snapshot: put: %w", putErr)
	}
	// Parked-window storage cost is a live sizing question; log every
	// snapshot's real compressed footprint so it's answerable from the field.
	delegateLog.Info("snapshot written", "key", snapshotKey(w.orgID, w.keyID), "reason", w.reason, "bytes_compressed", staged.compressedBytes)
	return true, nil
}

// snapshotRecord is what opening a key's lifecycle record came to, and so
// what the write that follows may do.
type snapshotRecord int

const (
	// snapshotUntracked: nothing was recorded. The write goes ahead with
	// neither the pre-upload guard nor an outcome.
	snapshotUntracked snapshotRecord = iota
	// snapshotOwned: this engagement holds the key's lifecycle, guards its
	// upload on still holding it, and records the outcome.
	snapshotOwned
	// snapshotSuperseded: a newer engagement holds the key. This one writes
	// nothing at all, since both the blob and the outcome are the newer
	// engagement's.
	snapshotSuperseded
)

func (r snapshotRecord) owned() bool { return r == snapshotOwned }

// snapshotRecordTimeout bounds each write to a key's lifecycle record. The
// writes run detached from the engagement's cancellation so a stopped
// engagement still records its outcome, which would otherwise leave them
// with no bound at all.
const snapshotRecordTimeout = detachedWriteDeadline

// beginSnapshotState records that claimID owes a snapshot for this key and
// reports what that came to. Untracked when there is nothing to record
// against — no store wired (a fixture), no claim to fence on (a claimless
// caller) — and the caller then neither guards its upload nor writes a
// terminal state. Superseded when a newer engagement already holds the key,
// and the caller then writes nothing.
//
// A failure to record is not a failure to snapshot: the blob is the thing the
// resume actually reads, so a store error logs and the snapshot proceeds
// untracked rather than being abandoned.
func (s *Spawner) beginSnapshotState(ctx context.Context, orgID, keyID, claimID string) snapshotRecord {
	if s.workspaceSnapshots == nil || claimID == "" {
		return snapshotUntracked
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotRecordTimeout)
	defer cancel()
	err := s.workspaceSnapshots.BeginSnapshotSystem(ctx, orgID, keyID, claimID)
	if errors.Is(err, db.ErrSnapshotSuperseded) {
		delegateLog.Info("a newer engagement holds the workspace snapshot key; not writing",
			"org", orgID, "key_id", keyID, "claim_id", claimID)
		return snapshotSuperseded
	}
	if err != nil {
		delegateLog.Warn("record workspace snapshot as pending failed; snapshotting untracked",
			"org", orgID, "key_id", keyID, "claim_id", claimID, "error", err)
		return snapshotUntracked
	}
	return snapshotOwned
}

// finishSnapshotState closes out this engagement's write. An unmatched CAS is
// the ordinary outcome of losing the key to a successor mid-write, not an
// error: the successor owns the row and its own finish is the one that counts,
// so this says so at INFO and leaves the row alone.
func (s *Spawner) finishSnapshotState(ctx context.Context, orgID, keyID, claimID string, ok bool) {
	if s.workspaceSnapshots == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotRecordTimeout)
	defer cancel()
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

// coverSnapshotState records that the blob this engagement last wrote also
// reflects the transcript up to w.position, for a checkpoint that found the
// tree unchanged and uploaded nothing (CoverSnapshotSystem). Best-effort: a
// restore without it reads the older position in the blob's manifest, which
// overstates what was lost and never understates it.
//
// Detached from the checkpoint's context, as the record writes are: a stop
// that lands after the capture does not make the position untrue, and the
// ending that follows clears it with its own begin.
func (s *Spawner) coverSnapshotState(ctx context.Context, w snapshotWrite, fingerprint string) {
	if s.workspaceSnapshots == nil || w.claimID == "" || w.position == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotRecordTimeout)
	defer cancel()
	matched, err := s.workspaceSnapshots.CoverSnapshotSystem(ctx, w.orgID, w.keyID, w.claimID, fingerprint, *w.position)
	if err != nil {
		delegateLog.Warn("record an unchanged checkpoint's position failed; a restore reads the blob's older one",
			"org", w.orgID, "key_id", w.keyID, "claim_id", w.claimID, "error", err)
		return
	}
	if !matched {
		delegateLog.Info("the key no longer holds this engagement's written checkpoint; position not recorded",
			"org", w.orgID, "key_id", w.keyID, "claim_id", w.claimID)
	}
}

// coveredStateFor reads the key's lifecycle row for positionFor, when the
// restored blob is a checkpoint of this conversation and so has a position a
// row could improve on; nil otherwise. A read that fails is nil as well: the
// manifest's own position is still right, only less exact.
func (s *Spawner) coveredStateFor(ctx context.Context, orgID, keyID, conversationID string, man snapshotManifest) *domain.WorkspaceSnapshotState {
	if man.positionFor(conversationID, nil) == nil || man.Fingerprint == "" {
		return nil
	}
	st, err := s.snapshotStateFor(ctx, orgID, keyID)
	if err != nil {
		delegateLog.Warn("rehydrate: reading the snapshot record for a later checkpoint position failed; using the blob's",
			"conversation", conversationID, "key_id", keyID, "error", err)
		return nil
	}
	return st
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
//
// man is the manifest's identity half — when the tree was read and, for a
// checkpoint, the position it covers; the rest is filled in from the capture.
func stageSnapshotArchive(ctx context.Context, captured *capturedSnapshot, wtPath string, man snapshotManifest) (_ *os.File, rawBytes, compressedBytes int64, err error) {
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
	if err = writeSnapshotTar(ctx, &cw, captured, wtPath, man); err != nil {
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
// manifest, built on man, first — so whether a blob is one this binary can
// restore is answered by its first member (snapshotLayoutAt) — then every
// checkout's potentially unbounded bundle + uncommitted patch, the ephemeral
// _tfac tree (streamed file by file), and the Claude session transcript.
func writeSnapshotTar(ctx context.Context, w io.Writer, captured *capturedSnapshot, wtPath string, man snapshotManifest) error {
	tw := tar.NewWriter(w)
	man.LayoutVersion = snapshotLayoutVersion
	man.SessionID = captured.state.SessionID
	// The scratch walk reports the same, and runs after the manifest is out;
	// this is that walk's answer for the one directory it asks about.
	man.CILogsOmitted = dirHasEntry(filepath.Join(wtPath, worktree.ScratchDir, worktree.CILogsDir))
	type member struct {
		name, path string
		data       []byte
	}
	var members []member
	for _, co := range captured.checkouts {
		d := co.state.Delta
		mc := manifestCheckout{RepoID: co.repoID, Slug: co.slug, Path: co.rel, Head: d.Head, Branch: d.Branch}
		if len(d.Bundle) > 0 || co.state.BundlePath != "" {
			mc.Bundle = snapCheckoutsPrefix + co.rel + "/bundle"
			members = append(members, member{mc.Bundle, co.state.BundlePath, d.Bundle})
		}
		if len(d.Patch) > 0 || co.state.PatchPath != "" {
			mc.Patch = snapCheckoutsPrefix + co.rel + "/patch"
			members = append(members, member{mc.Patch, co.state.PatchPath, d.Patch})
		}
		man.Checkouts = append(man.Checkouts, mc)
	}
	manBytes, err := json.Marshal(man)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := writeTarBytes(tw, snapManifest, manBytes); err != nil {
		return err
	}
	for _, m := range members {
		if err := writeCapturedMember(tw, m.name, m.data, m.path); err != nil {
			return err
		}
	}
	if _, err := tarScratch(ctx, tw, wtPath); err != nil {
		return fmt.Errorf("tar scratch: %w", err)
	}
	if captured.state.SessionID != "" {
		if len(captured.state.Transcript) > 0 || captured.state.TranscriptPath != "" {
			if err := writeCapturedMember(tw, snapSession, captured.state.Transcript, captured.state.TranscriptPath); err != nil {
				return err
			}
		} else {
			// The run has a session but the capture came back without its
			// transcript (absent on disk, or a capture that couldn't read it). The
			// blob is still written — worktree state matters on its own — but a
			// resume from it will hit the transcript-missing guard and fail.
			// Surface it: this is otherwise silent, and it's exactly the shape that
			// produced a resume-fails-with-no-reason report.
			delegateLog.Warn("snapshot omits session transcript; a resume of this conversation will not be able to continue where it left off", "session", captured.state.SessionID, "worktree", wtPath)
		}
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

// gitSeed is everything a cold rehydrate's git rebuild needs about one repo it
// replays a checkout's delta onto: where the bare lives (owner/repo), the
// upstream URL that seeds one when this executor has none, and the credential
// that authenticates the network git the rebuild does.
//
// auth covers more than one network hop — see worktree.RestoreCheckout. Seeding
// a missing bare is the obvious one; the load-bearing one is that the shared
// bare is a blobless partial clone, so the rebuild's checkout triggers a lazy
// promisor fetch against origin even when the bare is already there. A bare
// that exists is not a bare that is self-sufficient.
type gitSeed struct {
	owner    string
	repo     string
	cloneURL string
	auth     worktree.CloneAuth
}

// gitSeedFor resolves the seed for one repo a rehydrate rebuilds a checkout
// of. The clone URL comes from the repository row (written in the org's
// configured protocol).
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
		// A repository row can still carry an SSH clone URL after an org moves to
		// HTTPS: rows are rewritten only when profiling next runs, and that is
		// TTL-gated. The managed channel is HTTPS end to end, so rebuild from the
		// canonical form rather than hand it a URL it cannot route.
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

// checkoutRestorer is what a cold rehydrate needs to rebuild the checkouts a
// snapshot carries, resolved per repo: the bare seed, and for a pr-<N>
// checkout the pull request, read fresh through this engagement's GitHub
// client so its push settings are re-derived the way a fresh --pr checkout
// derives them. The zero value rebuilds no checkout at all.
type checkoutRestorer struct {
	seed func(ctx context.Context, owner, repo string) gitSeed
	pr   func(ctx context.Context, owner, repo string, number int) (*ghclient.PRView, error)
}

// checkoutRestorerFor builds this engagement's restorer. Every network hop
// rides the engagement's own credential path: the sidecar's git and REST
// proxies on an executor, the loopback git channel and the resolver-built
// client locally.
func (s *Spawner) checkoutRestorerFor(orgID string, sidecar *runSidecar, localGit *localGitChannel) checkoutRestorer {
	return checkoutRestorer{
		seed: func(ctx context.Context, owner, repo string) gitSeed {
			return s.gitSeedFor(ctx, orgID, owner, repo, sidecar, localGit)
		},
		pr: func(ctx context.Context, owner, repo string, number int) (*ghclient.PRView, error) {
			client := prReadClient(orgID, nil, sidecar)
			if client == nil {
				var err error
				if client, err = s.resolveGHClient(ctx, orgID, owner, repo); err != nil {
					return nil, fmt.Errorf("resolve the GitHub client: %w", err)
				}
			}
			if client == nil {
				return nil, errNoGitHubClient
			}
			return client.GetPR(ctx, owner, repo, number, false)
		},
	}
}

// prHeadCloneURL is the PR's head repository URL in the protocol of the bare's
// origin, so a push remote never mixes SSH and HTTPS; "" for a deleted head
// repository.
func prHeadCloneURL(originURL string, pr *ghclient.PRView) string {
	if strings.HasPrefix(originURL, "https://") || originURL == "" {
		return pr.CloneURL
	}
	return pr.SSHURL
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
//     durable snapshot is there, so the tree is rebuilt from it, every checkout
//     it carries through restorer. A blob of an older layout is no snapshot:
//     the ladder falls through to the last rung exactly as with none.
//   - waited, then rehydrated — the snapshot is not there YET. A park flips the
//     conversation's status (a shutdown hand-back releases its claim) before
//     writing the blob, so this is the ordinary reading of a healthy run for as
//     long as the capture takes; the lifecycle record says a persist is in
//     flight and awaitSnapshotBlob waits it out. That holds when an earlier
//     blob is already under the key too: the key is the task's, so the blob
//     there is an earlier persist's, and the one in flight outranks it.
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
// asOf is the other half of that answer for a rehydrated tree: the transcript
// position the blob's capture covers, when a checkpoint of this conversation
// wrote it, or the later one an unchanged checkpoint recorded for that same
// tree. Nil on every other rung, and for a blob an ending wrote, which covers
// the whole transcript.
//
// A warm tree an older binary laid out — its root a git checkout — is not
// warm: it is removed and the ladder continues as though it were gone.
//
// conv.ClaimID is read, not just carried: a rebuild re-stamps worktree_path and
// records the rebuilt checkouts' rows, and those writes are this engagement's
// to make only while it still holds the conversation. Every caller is a claimed
// dispatch, so it is populated at both — including the config the step builder
// synthesizes, which copies it across for exactly this reason.
func (s *Spawner) ensureWorkspace(ctx context.Context, orgID string, conv *domain.Conversation, restorer checkoutRestorer, fresh freshWorkspaceBuilder) (_ string, prov domain.WorkspaceProvenance, asOf *float64, err error) {
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
	keyID := workspaceKey(conv.TaskID)
	unlock := s.workspaceLocks.lock(workspaceLockKey(orgID, keyID))
	defer unlock()

	if conv.WorktreePath != "" {
		if _, err := os.Stat(conv.WorktreePath); err == nil {
			if !worktree.IsGitWorktree(conv.WorktreePath) {
				return conv.WorktreePath, domain.WorkspaceProvenanceWarm, nil, nil // warm: worktree still on disk
			}
			delegateLog.Warn("the warm run tree was laid out by an older binary; removing it and treating the workspace as gone",
				"conversation", conv.ID, "key_id", keyID, "path", conv.WorktreePath)
			if worktree.IsRunTreeFor(conv.WorktreePath, keyID) {
				if err := worktree.RemoveAt(conv.WorktreePath, keyID); err != nil {
					return "", "", nil, fmt.Errorf("remove a run tree of an older layout: %w", err)
				}
			}
		}
	}

	blobs := s.Storage()
	if blobs == nil {
		return "", "", nil, fmt.Errorf("worktree %q missing and no blob store to rehydrate from", conv.WorktreePath)
	}

	// Past the warm check the tree is rebuilt from the store: one workspace
	// operation, bounded so a store or a git replay that stops answering
	// fails the rebuild as the error it is, and tracked so the watchdog backs
	// that bound up. The wait for an in-flight persist is not part of it — it
	// has a bound of its own, which an operator sets — and the fresh-build
	// rung reports its own operations.
	activity := s.activityFor(conv.ID)
	beginRehydrate := func() (context.Context, func()) {
		return s.beginWorkspaceOp(ctx, conv.ID, "rehydrate")
	}
	opCtx, endOp := beginRehydrate()
	defer func() { endOp() }()

	rc, err := blobs.Get(opCtx, snapshotKey(orgID, keyID))
	if err == nil && s.snapshotPersistPending(ctx, orgID, keyID) {
		// A blob is here and a newer one is being written: the key is the
		// task's, so this one is an earlier persist's — the last step boundary
		// or park — and the one in flight is the engagement that just let go
		// of this conversation, a shutdown hand-back or a park moments ago. A
		// claim that lands inside that window must wait for it, or it restores
		// the earlier tree and the work since is gone.
		_ = rc.Close()
		endOp()
		endWait := activity.begin("snapshot_wait", s.snapshotWait()+backstopMargin)
		_, waited := s.awaitSnapshotBlob(ctx, orgID, keyID, true)
		endWait()
		span.SetAttributes(telemetry.SnapshotWaitedMs(waited.Milliseconds()))
		opCtx, endOp = beginRehydrate()
		rc, err = blobs.Get(opCtx, snapshotKey(orgID, keyID))
	}
	if errors.Is(err, storage.ErrNotFound) {
		// Not there yet, or not there at all — the lifecycle record is what
		// separates those, and the wait is where that question is asked.
		endOp()
		endWait := activity.begin("snapshot_wait", s.snapshotWait()+backstopMargin)
		appeared, waited := s.awaitSnapshotBlob(ctx, orgID, keyID, false)
		endWait()
		span.SetAttributes(telemetry.SnapshotWaitedMs(waited.Milliseconds()))
		if !appeared {
			wt, prov, err := s.workspaceFromNothing(ctx, orgID, conv, keyID, fresh)
			return wt, prov, nil, err
		}
		opCtx, endOp = beginRehydrate()
		rc, err = blobs.Get(opCtx, snapshotKey(orgID, keyID))
		if err != nil {
			// It existed a moment ago and now does not read: a successor's
			// discard, or a store fault. Either way there is nothing to
			// rehydrate from, so this takes the same answer the wait's own
			// failure would have.
			delegateLog.Warn("rehydrate: the snapshot the wait saw could not be fetched; falling back",
				"conversation", conv.ID, "key_id", keyID, "error", err)
			wt, prov, err := s.workspaceFromNothing(ctx, orgID, conv, keyID, fresh)
			return wt, prov, nil, err
		}
	} else if err != nil {
		return "", "", nil, fmt.Errorf("rehydrate: get snapshot: %w", err)
	}
	defer func() { _ = rc.Close() }()

	// Rebuild at the deterministic, host-local run-root for this key (equal to
	// conv.WorktreePath on the same host; a fresh path after landing elsewhere).
	man, restored, rErr := s.rehydrateFromSnapshot(opCtx, keyID, conv.ClaimID, restorer, rc)
	if errors.Is(rErr, errSnapshotLayout) {
		delegateLog.Warn("rehydrate: the snapshot was written for an older run-tree layout; treating it as no snapshot",
			"conversation", conv.ID, "key_id", keyID)
		endOp()
		wt, prov, err := s.workspaceFromNothing(ctx, orgID, conv, keyID, fresh)
		return wt, prov, nil, err
	}
	if rErr != nil {
		return "", "", nil, rErr
	}
	wtDir := worktree.RunRoot(keyID)
	s.restampWorktreePath(ctx, orgID, conv, wtDir)
	s.recordRestoredCheckouts(ctx, orgID, conv, restored)
	asOf = man.positionFor(conv.ID, s.coveredStateFor(ctx, orgID, keyID, conv.ID, man))
	delegateLog.Info("workspace rehydrated from snapshot", "conversation", conv.ID, "key_id", keyID,
		"captured_at", man.CapturedAt, "checkpoint", asOf != nil)
	return wtDir, domain.WorkspaceProvenanceRehydrated, asOf, nil
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

// errSnapshotLayout is a blob written for a run-tree layout this binary does
// not restore. Never converted: the ladder treats it as no snapshot.
var errSnapshotLayout = errors.New("rehydrate: snapshot is of an older run-tree layout")

// restoredCheckout is a checkout a rehydrate rebuilt, with the row the
// restoring conversation records for it.
type restoredCheckout struct {
	worktree.RestoredCheckout
	repoID, slug string
}

// rehydrateFromSnapshot unpacks a snapshot blob and reconstructs the run tree
// at keyID's run root: a fresh root, every checkout the manifest names rebuilt
// beneath it the way `workspace add` builds one (worktree.RestoreCheckout), the
// ephemeral _tfac tree restored, and the Claude session transcript dropped at
// the root's encoding so `claude --resume` reconnects.
//
// The bounded members (manifest, session) are read into memory; the _tfac tree
// and the checkouts' bundles and patches — any of which can run large — are
// streamed to staging dirs on disk as they're read, the scratch one moved into
// place with one rename. This mirrors the snapshot side's temp-file staging so
// neither direction buffers a large workspace whole.
//
// All or nothing: checkouts of different repos rebuild in parallel (the
// per-repo lock serializes two of one repo), and if any fails every checkout
// already rebuilt is removed along with the root, so nothing is left for a
// later claim to mistake for a warm tree. The error returned is the one that
// says why — an unreachable upstream in preference to the rest, so the
// hand-back spends the budget the cause belongs to.
//
// It returns the blob's manifest, which is what says how much of the
// transcript the rebuilt tree reflects, and the checkouts it rebuilt.
func (s *Spawner) rehydrateFromSnapshot(ctx context.Context, keyID, claimID string, restorer checkoutRestorer, r io.Reader) (_ snapshotManifest, _ []restoredCheckout, err error) {
	var man snapshotManifest
	var session []byte
	sawManifest := false
	root := worktree.RunRoot(keyID)

	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return snapshotManifest{}, nil, fmt.Errorf("rehydrate: mkdir runs parent: %w", err)
	}
	// Siblings of the root → the scratch move is an intra-filesystem rename.
	scratchStaging, err := os.MkdirTemp(filepath.Dir(root), ".scratch-rehydrate-*")
	if err != nil {
		return snapshotManifest{}, nil, fmt.Errorf("rehydrate: scratch staging: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratchStaging) }() // no-op once renamed into place
	memberStaging, err := os.MkdirTemp(filepath.Dir(root), ".checkouts-rehydrate-*")
	if err != nil {
		return snapshotManifest{}, nil, fmt.Errorf("rehydrate: checkout staging: %w", err)
	}
	defer func() { _ = os.RemoveAll(memberStaging) }()
	staged := map[string]string{}
	sawScratch := false

	cr, codec, err := snapshotReader(r)
	if errors.Is(err, errSnapshotLayout) {
		return snapshotManifest{}, nil, err
	}
	if err != nil {
		return snapshotManifest{}, nil, fmt.Errorf("rehydrate: open compressed snapshot: %w", err)
	}
	defer func() { _ = cr.Close() }()
	tr := tar.NewReader(cr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return snapshotManifest{}, nil, fmt.Errorf("rehydrate: read tar: %w", err)
		}
		switch {
		case hdr.Name == snapManifest:
			data, err := io.ReadAll(tr)
			if err != nil {
				return snapshotManifest{}, nil, fmt.Errorf("rehydrate: read manifest: %w", err)
			}
			if err := json.Unmarshal(data, &man); err != nil {
				return snapshotManifest{}, nil, fmt.Errorf("rehydrate: manifest: %w", err)
			}
			sawManifest = true
		case hdr.Name == snapSession:
			if session, err = io.ReadAll(tr); err != nil {
				return snapshotManifest{}, nil, fmt.Errorf("rehydrate: read session: %w", err)
			}
		case strings.HasPrefix(hdr.Name, snapCheckoutsPrefix):
			// Staged under a name of our own choosing, never the member's: the
			// manifest is what says which members belong to which checkout.
			dest := filepath.Join(memberStaging, strconv.Itoa(len(staged)))
			if err := stageScratchMember(memberStaging, filepath.Base(dest), tr); err != nil {
				return snapshotManifest{}, nil, err
			}
			staged[hdr.Name] = dest
		case strings.HasPrefix(hdr.Name, snapScratchPrefix):
			if err := stageScratchMember(scratchStaging, strings.TrimPrefix(hdr.Name, snapScratchPrefix), tr); err != nil {
				return snapshotManifest{}, nil, err
			}
			sawScratch = true
		}
	}

	// zstd validates its checksum only when the stream is read to its footer,
	// but the tar reader stops at the archive's end-of-archive marker — which
	// precedes that footer — so member reads alone never trigger the check. Drain the
	// remainder (a few trailing bytes for our own writes) to validate the whole
	// blob, and do it here, before any worktree mutation: a checksum mismatch
	// means the snapshot is corrupt, so fail the rehydrate rather than rebuild
	// onto untrustworthy state.
	if _, err := io.Copy(io.Discard, cr); err != nil {
		return snapshotManifest{}, nil, fmt.Errorf("rehydrate: %s integrity: %w", codec, err)
	}
	if !sawManifest || man.LayoutVersion != snapshotLayoutVersion {
		return snapshotManifest{}, nil, errSnapshotLayout
	}
	for _, mc := range man.Checkouts {
		if !validCheckoutRel(mc.Path, mc.RepoID, mc.Slug) {
			return snapshotManifest{}, nil, fmt.Errorf("rehydrate: manifest names checkout %q of %s at %q, which this layout never writes", mc.Slug, mc.RepoID, mc.Path)
		}
		for _, name := range []string{mc.Bundle, mc.Patch} {
			if name != "" && staged[name] == "" {
				return snapshotManifest{}, nil, fmt.Errorf("rehydrate: checkout %s names member %q, which the blob does not carry", mc.Path, name)
			}
		}
	}

	// A fresh root. Whatever is at the path is a tree this engagement is about
	// to replace — a stale copy, or one an older binary laid out.
	if err := worktree.RemoveAt(root, keyID); err != nil {
		return snapshotManifest{}, nil, fmt.Errorf("rehydrate: clear the run root: %w", err)
	}
	if _, err := worktree.MakeRunRoot(keyID); err != nil {
		return snapshotManifest{}, nil, fmt.Errorf("rehydrate: make run root: %w", err)
	}
	defer func() {
		if err != nil {
			worktree.RemoveRunRoot(keyID)
		}
	}()

	restored, err := s.restoreCheckouts(ctx, root, keyID, claimID, restorer, man.Checkouts, staged)
	if err != nil {
		return snapshotManifest{}, nil, err
	}

	if sawScratch {
		// The fresh root has no _tfac, so move the staged tree in wholesale.
		if err := os.Rename(scratchStaging, filepath.Join(root, worktree.ScratchDir)); err != nil {
			return snapshotManifest{}, nil, fmt.Errorf("rehydrate: install scratch: %w", err)
		}
	}
	if man.CILogsOmitted {
		// AFTER the scratch install, for the same reason the memory symlink is:
		// the install is a wholesale rename onto _tfac, which fails outright if
		// this notice's parent already exists. Best-effort — the notice
		// explains an absence, and failing the resume over it would trade a
		// missing explanation for a missing workspace.
		if err := plantCILogsNotice(root); err != nil {
			delegateLog.Warn("plant ci-logs notice on rehydrated tree failed; an agent that read a log under _tfac/ci-logs before the rebuild will find no explanation for its absence", "dir", root, "error", err)
		}
	}
	// Plant the jail's memory symlink AFTER the scratch install, never before: the
	// install is a wholesale rename onto _tfac, which fails outright if the link's
	// parent already exists. This is the rehydrated tree's one orchestrator-owned
	// moment — the run that resumes into it may find the tree already handed off,
	// and the snapshot never carries the link (entity-memory is excluded from
	// capture, and the walk skips non-regular files anyway).
	if err := worktree.EnsureSandboxMemoryLink(root); err != nil {
		delegateLog.Warn("plant sandbox memory symlink on rehydrated tree failed", "dir", root, "error", err)
	}
	if len(session) > 0 && man.SessionID != "" {
		if err := restoreSessionTranscript(root, man.SessionID, session); err != nil {
			for _, c := range restored {
				c.Discard()
			}
			return snapshotManifest{}, nil, err
		}
	}
	return man, restored, nil
}

// restoreCheckouts rebuilds every checkout under root, all or nothing. Each
// runs on its own goroutine; two of one repo serialize on the per-repo lock
// inside worktree.RestoreCheckout. On any failure every checkout that came back
// is discarded.
func (s *Spawner) restoreCheckouts(ctx context.Context, root, keyID, claimID string, restorer checkoutRestorer, checkouts []manifestCheckout, staged map[string]string) ([]restoredCheckout, error) {
	type result struct {
		restored restoredCheckout
		err      error
	}
	results := make([]result, len(checkouts))
	var wg sync.WaitGroup
	for i, mc := range checkouts {
		wg.Add(1)
		go func(i int, mc manifestCheckout) {
			defer wg.Done()
			c, err := s.restoreOneCheckout(ctx, root, keyID, claimID, restorer, mc, staged)
			results[i] = result{restored: c, err: err}
		}(i, mc)
	}
	wg.Wait()

	var restored []restoredCheckout
	var firstErr, upstreamErr error
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			if upstreamErr == nil && upstreamSetupFailure(r.err) {
				upstreamErr = r.err
			}
			continue
		}
		restored = append(restored, r.restored)
	}
	if firstErr == nil {
		return restored, nil
	}
	for _, c := range restored {
		c.Discard()
	}
	if upstreamErr != nil {
		return nil, fmt.Errorf("rehydrate: %w", upstreamErr)
	}
	return nil, fmt.Errorf("rehydrate: %w", firstErr)
}

// restoreOneCheckout rebuilds one manifest checkout through restoreCheckout,
// resolving its repo's seed and, for a pr-<N> checkout, its pull request.
func (s *Spawner) restoreOneCheckout(ctx context.Context, root, keyID, claimID string, restorer checkoutRestorer, mc manifestCheckout, staged map[string]string) (restoredCheckout, error) {
	owner, repo := parseOwnerRepo(mc.RepoID)
	var seed gitSeed
	if restorer.seed != nil {
		seed = restorer.seed(ctx, owner, repo)
	}
	r := worktree.CheckoutRestore{
		Owner: owner, Repo: repo, CloneURL: seed.cloneURL, Auth: seed.auth,
		Root: root, Slug: mc.Slug, RootKey: keyID,
		Head: mc.Head, Branch: mc.Branch,
		BundlePath: staged[mc.Bundle], PatchPath: staged[mc.Patch],
	}
	if _, prNumber, _ := worktree.ParseCheckoutSlug(mc.Slug); prNumber > 0 {
		if restorer.pr == nil {
			return restoredCheckout{}, fmt.Errorf("restore %s: no GitHub client to read PR #%d with", mc.Path, prNumber)
		}
		pr, err := restorer.pr(ctx, owner, repo, prNumber)
		if err != nil {
			return restoredCheckout{}, fmt.Errorf("restore %s: read PR #%d: %w", mc.Path, prNumber, err)
		}
		if pr == nil {
			return restoredCheckout{}, fmt.Errorf("restore %s: PR #%d not found", mc.Path, prNumber)
		}
		r.PR = &worktree.PRCheckout{HeadRef: pr.HeadRef, HeadCloneURL: prHeadCloneURL(seed.cloneURL, pr), BaseRef: pr.BaseRef}
	}
	c, err := restoreCheckout(ctx, r)
	if err != nil {
		return restoredCheckout{}, err
	}
	return restoredCheckout{RestoredCheckout: c, repoID: mc.RepoID, slug: mc.Slug}, nil
}

// recordRestoredCheckouts records every rebuilt checkout as the restoring
// conversation's, at the path it has now, through the claim fence: the push
// gate and the next snapshot read these rows, and a conversation restoring a
// tree it did not build — a later step, or a run whose rows a terminal left
// pointing at a root that is gone — holds no row for them otherwise.
//
// Best-effort per row, like the PR checkout's own record at setup: a missing
// row costs this conversation its push to that repo, never its start. A fence
// refusal is the ownership loss setWorktreePath already reports.
func (s *Spawner) recordRestoredCheckouts(ctx context.Context, orgID string, conv *domain.Conversation, restored []restoredCheckout) {
	if s.conversationWorktrees == nil || conv.ClaimID == "" {
		return
	}
	for _, c := range restored {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ledgerWriteTimeout)
		_, err := s.conversationWorktrees.RecordForClaimSystem(recordCtx, orgID, conv.ClaimID, domain.ConversationWorktree{
			ConversationID: conv.ID, RepoID: c.repoID, Path: c.Path, Ref: c.slug,
		})
		cancel()
		if errors.Is(err, db.ErrClaimReleased) {
			return
		}
		if err != nil {
			delegateLog.Warn("record a restored checkout in conversation_worktrees failed; pushes to this repo will be denied for this conversation",
				"conversation", conv.ID, "repo", c.repoID, "path", c.Path, "error", err)
		}
	}
}

// snapshotPresence is what a key's blob is to this binary.
type snapshotPresence int

const (
	snapshotAbsent snapshotPresence = iota
	// snapshotRestorable: a blob of the layout this binary restores.
	snapshotRestorable
	// snapshotUnsupported: a blob of an older layout, which is no snapshot.
	snapshotUnsupported
)

// snapshotLayoutAt reads only as far as the key's blob's first member. A blob
// this binary wrote opens with its manifest, so the answer costs one short read
// rather than the download a restore makes; an older blob opens with something
// else, which is the answer too. An error is a store or decode failure, which
// the caller must not read as either.
func (s *Spawner) snapshotLayoutAt(ctx context.Context, orgID, keyID string) (snapshotPresence, error) {
	blobs := s.Storage()
	if blobs == nil {
		return snapshotAbsent, nil
	}
	rc, err := blobs.Get(ctx, snapshotKey(orgID, keyID))
	if errors.Is(err, storage.ErrNotFound) {
		return snapshotAbsent, nil
	}
	if err != nil {
		return snapshotAbsent, err
	}
	defer func() { _ = rc.Close() }()
	cr, _, err := snapshotReader(rc)
	if errors.Is(err, errSnapshotLayout) {
		return snapshotUnsupported, nil
	}
	if err != nil {
		return snapshotAbsent, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = cr.Close() }()
	tr := tar.NewReader(cr)
	hdr, err := tr.Next()
	if err != nil {
		return snapshotAbsent, fmt.Errorf("read snapshot: %w", err)
	}
	if hdr.Name != snapManifest {
		return snapshotUnsupported, nil
	}
	data, err := io.ReadAll(io.LimitReader(tr, manifestReadLimit))
	if err != nil {
		return snapshotAbsent, fmt.Errorf("read snapshot manifest: %w", err)
	}
	var man snapshotManifest
	if err := json.Unmarshal(data, &man); err != nil {
		return snapshotAbsent, fmt.Errorf("decode snapshot manifest: %w", err)
	}
	if man.LayoutVersion != snapshotLayoutVersion {
		return snapshotUnsupported, nil
	}
	return snapshotRestorable, nil
}

// manifestReadLimit bounds the layout probe's read of a manifest: a small JSON
// header, whatever the size of the blob behind it.
const manifestReadLimit = 1 << 20

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// snapshotReader opens a blob's zstd stream. Every blob this binary restores is
// zstd; anything else (the gzip blobs an older binary wrote) is a blob of an
// older layout, which is no snapshot.
func snapshotReader(r io.Reader) (io.ReadCloser, string, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(len(zstdMagic))
	if err != nil {
		return nil, "", err
	}
	if !bytes.Equal(magic, zstdMagic) {
		return nil, "", errSnapshotLayout
	}
	zr, err := zstd.NewReader(br)
	if err != nil {
		return nil, "", err
	}
	return zr.IOReadCloser(), "zstd", nil
}

// discardWorkspaceSnapshot deletes a parked workspace's snapshot blob once the
// work it belonged to reaches a terminal state, so durable storage
// doesn't accumulate orphans. keyID is workspaceKey(taskID).
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

// tarScratch writes every scratch file a snapshot carries (walkScratch) under
// the snapScratchPrefix, reporting whether a populated ci-logs subtree was
// left out — the manifest's CILogsOmitted and the gate on the rehydrate's
// notice.
func tarScratch(ctx context.Context, tw *tar.Writer, wtPath string) (omittedCILogs bool, err error) {
	return walkScratch(ctx, wtPath, func(rel, path string, fi os.FileInfo) error {
		// Stream each file into the tar rather than reading it whole — the
		// agent's own intermediates are unbounded even with the log archives
		// excluded.
		err := writeTarFile(tw, snapScratchPrefix+rel, path, fi.Size())
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	})
}

// walkScratch visits every regular file under wtPath/_tfac that a snapshot
// carries, in lexical order, skipping the scratchExcludes subtrees — the one
// definition of "the scratch a snapshot carries" that both the archive and the
// fingerprint read. A missing _tfac visits nothing.
//
// It reports whether a ci-logs subtree with something in it was skipped. An
// entry that vanishes between the listing and the visit is skipped: a
// checkpoint walks a tree a background process may still be changing, and an
// entry removed a moment later is the same torn capture as one removed a
// moment earlier. ctx is checked per entry.
func walkScratch(ctx context.Context, wtPath string, visit func(rel, path string, fi os.FileInfo) error) (omittedCILogs bool, err error) {
	root := filepath.Join(wtPath, worktree.ScratchDir)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return false, nil
	}
	err = filepath.Walk(root, func(path string, fi os.FileInfo, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
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
		return visit(filepath.ToSlash(rel), path, fi)
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
// size is the header length, from the walk's stat. An ending's snapshot reads
// a dormant tree, where that size is stable; a checkpoint reads a live one, where
// a background writer may still be appending. So exactly size bytes are copied:
// a file that grew since the stat is captured as it was at the stat, and one
// that shrank is a short read that fails the archive rather than a member
// padded with bytes the file never held. A file that vanished before the open
// fails with an error matching fs.ErrNotExist, which the walk skips.
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
	if _, err := io.CopyN(tw, f, size); err != nil {
		return fmt.Errorf("tar copy %s: %w", name, err)
	}
	return nil
}
