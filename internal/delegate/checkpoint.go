// Workspace checkpoints: a live native engagement snapshots its run tree at
// tool-batch boundaries, so an executor that dies without handing anything
// back — a SIGKILL, an OOM kill, a lost node — costs at most the checkpoint
// interval plus one tool call of workspace, rather than everything since the
// last park or step boundary.
//
// A checkpoint writes the same blob under the same key an ending writes, and
// through the same three phases (workspace_snapshot.go): capture, archive,
// upload. What differs is when each runs relative to the agent. The capture
// and the archive read the tree, so they run at a quiet point — after a batch
// has every result persisted, while the model thinks about the next call —
// and the next sandbox tool call waits for them. The upload reads only the
// staged file, so it runs in the background and nothing waits for it.
//
// Native runtime only, which is multi mode only. The SDK dispatches its own
// tools, so there is no point at which this process could hold one, and a
// local tree lives on the executor's own disk, where a process crash does not
// take it.
//
// The one ordering rule: an engagement's checkpoint and its ending's snapshot
// are the same writer to the key's record and blob. recordNativeResult stops
// the checkpointer and waits for it before any ending runs — otherwise an
// in-flight checkpoint's Finish would close the ending's pending record while
// the ending's blob is still uploading, or its Put would land after the
// ending's and overwrite the newer tree.

package delegate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// DefaultSnapshotInterval is how often a live engagement checkpoints its
// tree, at most: a checkpoint also needs a tool call to have run since the
// last one, and it waits for the next batch boundary.
const DefaultSnapshotInterval = 5 * time.Minute

const (
	// checkpointHostSlots bounds how many checkpoints capture and compress at
	// once in this process, so a burst of engagements reaching boundaries
	// together does not pin the executor's CPU. A checkpoint that finds no
	// slot is skipped, not queued: the next boundary tries again.
	checkpointHostSlots = 2

	// checkpointCaptureTimeout bounds the capture phase — the git capture and
	// the archive — which is the part the next tool call waits for. The same
	// five minutes the cap-broker gives the isolated capture child.
	checkpointCaptureTimeout = 5 * time.Minute

	// checkpointUploadTimeout bounds the background upload, so a PUT that
	// hangs cannot hold the engagement's one checkpoint slot forever.
	checkpointUploadTimeout = 5 * time.Minute
)

// checkpointStoppingNotice answers a tool call the engagement stopped while it
// was waiting on a checkpoint's capture. The call never ran.
const checkpointStoppingNotice = "This tool call was not run: the engagement stopped before it could start."

// Outcomes of a checkpoint that came due, on tf_workspace_checkpoints_total.
const (
	checkpointWritten          = "written"
	checkpointSkippedUnchanged = "skipped_unchanged"
	checkpointSkippedBusy      = "skipped_busy"
	checkpointFailed           = "failed"
)

// ParseSnapshotInterval interprets the TF_SNAPSHOT_INTERVAL_SEC env value.
// Empty → the default. 0 → checkpoints disabled. Non-numeric or negative → the
// default plus an error the caller logs; a bad value must not brick boot, the
// same rule TF_SNAPSHOT_WAIT_SEC follows.
func ParseSnapshotInterval(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultSnapshotInterval, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return DefaultSnapshotInterval, fmt.Errorf(
			"invalid TF_SNAPSHOT_INTERVAL_SEC %q (want a non-negative integer number of seconds, 0 to disable checkpoints); using default %s",
			raw, DefaultSnapshotInterval)
	}
	return time.Duration(n) * time.Second, nil
}

// SetSnapshotInterval sets how often live native engagements checkpoint their
// workspace. Zero disables checkpoints. Call once at startup (internal/app
// resolves TF_SNAPSHOT_INTERVAL_SEC); an engagement reads it when it starts.
func (s *Spawner) SetSnapshotInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshotInterval = d
}

func (s *Spawner) checkpointInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotInterval
}

// acquireCheckpointSlot takes one of the host's checkpoint slots without
// waiting, reporting whether it got one.
func (s *Spawner) acquireCheckpointSlot() bool {
	s.mu.Lock()
	if s.checkpointSlots == nil {
		s.checkpointSlots = make(chan struct{}, checkpointHostSlots)
	}
	slots := s.checkpointSlots
	s.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Spawner) releaseCheckpointSlot() {
	s.mu.Lock()
	slots := s.checkpointSlots
	s.mu.Unlock()
	<-slots
}

// checkpointer is one engagement's checkpoint schedule and its one checkpoint
// in flight. It lives for the agent loop and no longer. Every method is
// nil-safe, so an engagement that does not checkpoint wires a nil one.
type checkpointer struct {
	s        *Spawner
	w        snapshotWrite
	interval time.Duration
	activity *activityTracker

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	stopped bool
	// lastStarted is when the last checkpoint started, or the loop did before
	// the first. Monotonic.
	lastStarted time.Time
	// sandboxCalls counts tool calls dispatched into the jail since
	// lastStarted. Loop-side tools never reach the jail and are not counted:
	// they cannot have changed the tree.
	sandboxCalls int
	// inflight is true from a checkpoint's start to the end of its upload.
	inflight bool
	// captured is the in-flight checkpoint's barrier: closed once it has
	// stopped reading the tree. Nil before the first checkpoint.
	captured chan struct{}
	// lastFingerprint digests the tree the last written checkpoint stored.
	lastFingerprint string
	// coveredAt is the latest moment the stored blob is known to match the
	// tree: the capture time of the last checkpoint written or found
	// unchanged, or the loop's start before the first. Monotonic.
	coveredAt time.Time
}

// startCheckpointer arms checkpoints for one native engagement, or returns nil
// when it will take none: checkpoints are disabled, no blob store is wired, or
// the engagement has no claim or tree to snapshot. ctx is the engagement's, so
// a stop, a fence or a shutdown cancels a checkpoint in flight with it.
func (s *Spawner) startCheckpointer(ctx context.Context, orgID, conversationID, keyID, claimID, wtPath string) *checkpointer {
	interval := s.checkpointInterval()
	if interval <= 0 || s.Storage() == nil || claimID == "" || keyID == "" || wtPath == "" {
		return nil
	}
	now := time.Now()
	c := &checkpointer{
		s: s,
		w: snapshotWrite{
			orgID: orgID, conversationID: conversationID, keyID: keyID, claimID: claimID,
			wtPath: wtPath, runtime: domain.ConversationRuntimeNative, reason: snapshotReasonCheckpoint,
		},
		interval:    interval,
		activity:    s.activityFor(conversationID),
		lastStarted: now,
		coveredAt:   now,
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	s.mu.Lock()
	if s.checkpointers == nil {
		s.checkpointers = make(map[string]*checkpointer)
	}
	s.checkpointers[conversationID] = c
	s.mu.Unlock()
	return c
}

// checkpointerFor is the checkpointer of the engagement driving
// conversationID in this process, or nil.
func (s *Spawner) checkpointerFor(conversationID string) *checkpointer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpointers[conversationID]
}

// stop cancels a checkpoint in flight and waits for it to return, record
// writes included, and takes no more. Every ending calls it before its own
// snapshot; see the file comment for why waiting, not just cancelling, is the
// point. Cancelling rather than letting the upload finish keeps a shutdown's
// hand-back inside its grace period: the ending's snapshot supersedes the
// checkpoint anyway.
func (c *checkpointer) stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.cancel()
	c.wg.Wait()
	c.s.mu.Lock()
	if c.s.checkpointers[c.w.conversationID] == c {
		delete(c.s.checkpointers, c.w.conversationID)
	}
	c.s.mu.Unlock()
}

// age is how long the engagement's tree has gone without being covered by a
// stored blob, for the claim renewal to stamp. False when the engagement does
// not checkpoint, which the renewal stamps as no reading.
func (c *checkpointer) age() (time.Duration, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.coveredAt), true
}

// noteToolCall counts a call the jail ran. Wired into AfterToolCall, which
// only dispatched calls reach.
func (c *checkpointer) noteToolCall() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.sandboxCalls++
	c.mu.Unlock()
}

// afterToolBatch is the engine's AfterToolBatch hook. When a checkpoint is due
// it starts one and returns, so the capture overlaps the next model call.
//
// Due means both: the interval has passed since the last checkpoint started,
// and the jail has run a tool since. A checkpoint that comes due while the
// previous one is still uploading, or finds no host slot, is skipped rather
// than queued — the next boundary is due as well, and tries again.
func (c *checkpointer) afterToolBatch(_ context.Context, position float64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.stopped || c.sandboxCalls == 0 || time.Since(c.lastStarted) < c.interval {
		c.mu.Unlock()
		return
	}
	if c.inflight || !c.s.acquireCheckpointSlot() {
		c.mu.Unlock()
		recordWorkspaceCheckpoint(checkpointSkippedBusy)
		return
	}
	c.lastStarted = time.Now()
	c.sandboxCalls = 0
	c.inflight = true
	captured := make(chan struct{})
	c.captured = captured
	last := c.lastFingerprint
	c.wg.Add(1)
	c.mu.Unlock()

	go c.run(position, last, captured)
}

// run is one checkpoint, from capture to recorded outcome.
func (c *checkpointer) run(position float64, lastFingerprint string, captured chan struct{}) {
	defer c.wg.Done()
	var once sync.Once
	treeReleased := func() {
		once.Do(func() {
			close(captured)
			c.s.releaseCheckpointSlot()
		})
	}
	defer treeReleased()

	w := c.w
	w.position = &position
	res := c.s.writeCheckpoint(c.ctx, w, lastFingerprint, treeReleased)

	c.mu.Lock()
	c.inflight = false
	switch res.outcome {
	case checkpointWritten:
		c.lastFingerprint = res.fingerprint
		c.coveredAt = res.capturedAt
	case checkpointSkippedUnchanged:
		c.coveredAt = res.capturedAt
	}
	c.mu.Unlock()
	recordWorkspaceCheckpoint(res.outcome)
}

// awaitCapture is the barrier the BeforeToolCall hook runs: a sandbox tool
// call does not start while a checkpoint is still reading the tree, so the
// capture is exactly the tree after the batch it follows. It waits for the
// capture phase only, never the upload.
//
// The wait is an activity operation, so the stall watchdog stops an
// engagement whose capture hangs past its own timeout. A deny is returned
// only when the engagement stops during the wait, and the call is then not
// run.
func (c *checkpointer) awaitCapture(ctx context.Context) (deny string) {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	ch := c.captured
	c.mu.Unlock()
	if ch == nil {
		return ""
	}
	select {
	case <-ch:
		return ""
	default:
	}
	end := c.activity.begin("checkpoint", checkpointCaptureTimeout+backstopMargin)
	defer end()
	select {
	case <-ch:
		return ""
	case <-ctx.Done():
		return checkpointStoppingNotice
	}
}

// checkpointHooks composes the checkpointer into the engine's hooks around
// the ones the engagement already has. A nil checkpointer leaves them as they
// were, so an engagement that does not checkpoint runs exactly the hooks it
// always did.
func checkpointHooks(c *checkpointer, hooks agentloop.Hooks) agentloop.Hooks {
	if c == nil {
		return hooks
	}
	gate, after := hooks.BeforeToolCall, hooks.AfterToolCall
	hooks.BeforeToolCall = func(ctx context.Context, call domain.ToolCall) string {
		if gate != nil {
			if deny := gate(ctx, call); deny != "" {
				return deny
			}
		}
		return c.awaitCapture(ctx)
	}
	hooks.AfterToolCall = func(ctx context.Context, call domain.ToolCall, out agentloop.ToolOutcome) agentloop.ToolOutcome {
		c.noteToolCall()
		if after != nil {
			return after(ctx, call, out)
		}
		return out
	}
	hooks.AfterToolBatch = c.afterToolBatch
	return hooks
}

// checkpointCapture is the capture a checkpoint runs. A package var, in the
// same spirit as captureViaSandbox: a test holds a capture open to observe
// what waits for it and what does not.
var checkpointCapture = captureSnapshot

// checkpointResult is what one checkpoint came to.
type checkpointResult struct {
	outcome     string
	fingerprint string
	capturedAt  time.Time
}

// writeCheckpoint captures the live tree, skips the write when it matches the
// last one stored, and otherwise archives and uploads it. treeReleased is
// called the moment the tree has been read for the last time — before the
// upload — and releases the barrier the next tool call waits on.
//
// The record is opened only once there is something to write, after the
// fingerprint, rather than before the capture as an ending opens it. An
// ending's record has a reader waiting on it; a checkpoint's only reader is a
// successor after this executor died, and to that reader a checkpoint that
// died before it opened the record and one that died with the record pending
// under a dead writer read the same: restore the blob already there. Opening
// it late keeps an unchanged tree to one capture and no writes.
func (s *Spawner) writeCheckpoint(ctx context.Context, w snapshotWrite, lastFingerprint string, treeReleased func()) (res checkpointResult) {
	if s.Storage() == nil {
		return checkpointResult{outcome: checkpointFailed}
	}
	ctx, span := s.startSnapshotSpan(ctx, w)
	var err error
	defer func() {
		span.SetAttributes(telemetry.Outcome(res.outcome))
		recordSpanError(span, err)
		span.End()
		if err != nil {
			delegateLog.Warn("workspace checkpoint failed; the last one written stands",
				"conversation", w.conversationID, "key_id", w.keyID, "error", err)
		}
	}()
	res.outcome = checkpointFailed

	capCtx, cancelCapture := context.WithTimeout(ctx, checkpointCaptureTimeout)
	defer cancelCapture()
	captured, err := checkpointCapture(capCtx, w)
	if err != nil {
		return res
	}
	defer captured.release()
	res.capturedAt = captured.at
	res.fingerprint, err = snapshotFingerprint(capCtx, captured.state, w.wtPath)
	if err != nil {
		return res
	}
	if res.fingerprint == lastFingerprint {
		res.outcome = checkpointSkippedUnchanged
		return res
	}

	// A stopped checkpointer opens no record: the ending that stopped it is
	// about to open its own, and waits for this goroutine before it does.
	if err = ctx.Err(); err != nil {
		return res
	}
	stateCtx := context.WithoutCancel(ctx)
	owned := s.beginSnapshotState(stateCtx, w.orgID, w.keyID, w.claimID)
	settled := false
	defer func() {
		if owned && !settled {
			s.finishSnapshotState(stateCtx, w.orgID, w.keyID, w.claimID, false)
		}
	}()

	staged, err := archiveSnapshot(capCtx, w, captured)
	captured.release()
	treeReleased()
	if err != nil {
		return res
	}
	defer staged.discard()

	upCtx, cancelUpload := context.WithTimeout(ctx, checkpointUploadTimeout)
	defer cancelUpload()
	written, err := s.uploadSnapshot(upCtx, w, staged, owned)
	if err != nil {
		return res
	}
	if !written {
		// A newer engagement holds the key, and the record is its to close.
		settled = true
		return res
	}
	span.SetAttributes(telemetry.SizeBytes(staged.compressedBytes))
	if owned {
		s.finishSnapshotState(stateCtx, w.orgID, w.keyID, w.claimID, true)
		settled = true
	}
	res.outcome = checkpointWritten
	return res
}

// snapshotFingerprint digests what a snapshot of this capture would carry: the
// git delta's identity and bytes, the transcript, and a stat walk of the
// scratch the archive would walk. Two captures with the same fingerprint
// would archive to the same members, so a checkpoint that matches the last
// one written has nothing to store.
//
// The scratch half is path, size and modification time, not content. It is
// the cheap half on purpose: the scratch is the unbounded part of a workspace,
// and hashing it would cost what compressing it costs.
func snapshotFingerprint(ctx context.Context, captured worktree.CapturedState, wtPath string) (string, error) {
	h := sha256.New()
	field := func(parts ...string) {
		for _, p := range parts {
			fmt.Fprintf(h, "%d:%s;", len(p), p)
		}
	}
	if d := captured.Delta; d != nil {
		field("git", d.Branch, d.Head)
		if err := hashMember(h, d.Bundle, captured.BundlePath); err != nil {
			return "", fmt.Errorf("fingerprint bundle: %w", err)
		}
		if err := hashMember(h, d.Patch, captured.PatchPath); err != nil {
			return "", fmt.Errorf("fingerprint patch: %w", err)
		}
	} else {
		field("no-git")
	}
	field("session", captured.SessionID)
	if err := hashMember(h, captured.Transcript, captured.TranscriptPath); err != nil {
		return "", fmt.Errorf("fingerprint transcript: %w", err)
	}
	omittedCILogs, err := walkScratch(ctx, wtPath, func(rel, _ string, fi os.FileInfo) error {
		field("file", rel, strconv.FormatInt(fi.Size(), 10), strconv.FormatInt(fi.ModTime().UnixNano(), 10))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("fingerprint scratch: %w", err)
	}
	field("ci-logs", strconv.FormatBool(omittedCILogs))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashMember feeds one captured member into h, length first, from memory or
// from its staged file — the same two forms writeCapturedMember reads.
func hashMember(h hash.Hash, data []byte, path string) error {
	size, err := capturedBytesSize(data, path)
	if err != nil {
		return err
	}
	fmt.Fprintf(h, "%d;", size)
	if path == "" {
		_, _ = h.Write(data)
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.CopyN(h, f, size)
	return err
}

// workspaceCheckpoints owns this package's checkpoint counter. Package-level
// because a metric instrument is process-global by nature, and swapped
// atomically so a test can back it with a manual reader while a checkpoint
// goroutine may still be counting.
var workspaceCheckpoints atomic.Pointer[workspaceCheckpointStats]

func init() {
	workspaceCheckpoints.Store(newWorkspaceCheckpointStats(otel.GetMeterProvider()))
}

type workspaceCheckpointStats struct {
	checkpoints metric.Int64Counter
}

// newWorkspaceCheckpointStats builds the instrument against mp — production
// passes the global provider, which telemetry.Init installs. An
// instrument-creation error can only be a programmer error, and the API hands
// back a usable no-op alongside it, so it is logged rather than propagated.
func newWorkspaceCheckpointStats(mp metric.MeterProvider) *workspaceCheckpointStats {
	counter, err := mp.Meter("internal/delegate").Int64Counter("workspace.checkpoints",
		metric.WithDescription("Workspace checkpoints that came due in live native engagements, by outcome."))
	if err != nil {
		delegateLog.Warn("workspace checkpoint counter setup failed", "error", err)
	}
	return &workspaceCheckpointStats{checkpoints: counter}
}

func recordWorkspaceCheckpoint(outcome string) {
	stats := workspaceCheckpoints.Load()
	if stats == nil || stats.checkpoints == nil {
		return
	}
	stats.checkpoints.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}
