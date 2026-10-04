package delegate

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// newWorkspaceStallFixture is a checkpoint fixture whose engagement runs under
// the given watchdog timings: a claimed native conversation, a blob store, and
// a run tree at the task's run root.
func newWorkspaceStallFixture(t *testing.T, conversationID string, timings activityTimings) checkpointFixture {
	t.Helper()
	paths.SetForTest(t, t.TempDir())
	f := checkpointFixture{stallFixture: newStallFixture(t, conversationID, timings)}
	wireBlobStore(t, f.s)
	f.keyID = workspaceKey(f.task.ID)
	f.tree = worktree.RunRoot(f.keyID)
	writeFile(t, filepath.Join(f.tree, worktree.ScratchDir, "notes.txt"), "initial")
	if _, err := f.database.Exec(`UPDATE conversations SET worktree_path = ? WHERE id = ?`, f.tree, conversationID); err != nil {
		t.Fatalf("stamp worktree path: %v", err)
	}
	f.outcomes = checkpointCounter(t)
	return f
}

// stopIntent reads the conversation's stop intent: whether one is recorded,
// and the reason it would settle as.
func (f stallFixture) stopIntent(t *testing.T) (recorded bool, reason string) {
	t.Helper()
	if err := f.database.QueryRow(
		`SELECT stop_requested_at IS NOT NULL, COALESCE(stop_requested_reason, '') FROM conversations WHERE id = ?`, f.conversationID,
	).Scan(&recorded, &reason); err != nil {
		t.Fatalf("read stop intent: %v", err)
	}
	return recorded, reason
}

// slowPutStorage takes delay over every upload, yielding to the upload's own
// context the way a real store's request does.
type slowPutStorage struct {
	storage.Storage
	delay time.Duration
}

func (s *slowPutStorage) Put(ctx context.Context, key string, r io.Reader) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.Storage.Put(ctx, key, r)
}

// blockedPutStorage never finishes an upload: it returns only when the
// upload's context ends.
type blockedPutStorage struct{ storage.Storage }

func (blockedPutStorage) Put(ctx context.Context, _ string, _ io.Reader) error {
	<-ctx.Done()
	return ctx.Err()
}

// blockedGetStorage never answers a read: it returns only when the read's
// context ends, as a store that stopped answering does.
type blockedGetStorage struct{ storage.Storage }

func (blockedGetStorage) Get(ctx context.Context, _ string) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestWorkspaceOp_ItsOwnTimeoutFiresBeforeTheWatchdog: a workspace operation
// that outlives its bound fails with the timeout, and the watchdog behind it
// stays quiet. The two must not share a deadline, or the watchdog wins the
// race and the conversation waits for a person instead of being retried.
func TestWorkspaceOp_ItsOwnTimeoutFiresBeforeTheWatchdog(t *testing.T) {
	f := newStallFixture(t, "r-workspace-op", activityTimings{idle: time.Hour, workspaceOp: 80 * time.Millisecond})

	opCtx, end := f.s.beginWorkspaceOp(f.claimCtx, f.conversationID, "clone")
	<-opCtx.Done()
	end()
	if !errors.Is(opCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("operation context ended with %v, want its own deadline", opCtx.Err())
	}
	time.Sleep(200 * time.Millisecond)
	if cause := context.Cause(f.claimCtx); cause != nil {
		t.Fatalf("the watchdog stopped the engagement (%v) at the operation's own timeout", cause)
	}
	if recorded, reason := f.stopIntent(t); recorded {
		t.Fatalf("a stop intent (%q) was filed for an operation its own timeout had already failed", reason)
	}
}

// TestEnsureWorkspace_ARehydrateTimeoutIsAnError: a store that stops answering
// fails the rehydrate as an error the setup ladder retries, and the watchdog
// files no stall for it.
func TestEnsureWorkspace_ARehydrateTimeoutIsAnError(t *testing.T) {
	f := newWorkspaceStallFixture(t, "r-rehydrate-timeout", activityTimings{idle: time.Hour, workspaceOp: 80 * time.Millisecond})
	f.s.SetStorage(blockedGetStorage{Storage: f.s.Storage()})

	conv := &domain.Conversation{
		ID: f.conversationID, OrgID: runmode.LocalDefaultOrgID, TaskID: f.task.ID, ClaimID: f.claimID,
		WorktreePath: filepath.Join(t.TempDir(), "gone"),
	}
	_, _, _, err := f.s.ensureWorkspace(f.claimCtx, runmode.LocalDefaultOrgID, conv, gitSeed{}, failingFreshBuilder(t))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ensureWorkspace = %v, want the rehydrate's own timeout", err)
	}
	time.Sleep(200 * time.Millisecond)
	if cause := context.Cause(f.claimCtx); cause != nil {
		t.Fatalf("the watchdog stopped the engagement (%v) for a rehydrate its own timeout had failed", cause)
	}
	if recorded, reason := f.stopIntent(t); recorded {
		t.Fatalf("a stop intent (%q) was filed for a rehydrate timeout", reason)
	}
}

// TestStall_AnIdleParkWithASlowSnapshotFilesNoStop: an idle park releases the
// claim and then snapshots. However long that snapshot takes, the engagement
// no longer holds the conversation, so the watchdog must not file a stop: one
// would turn the idle park into a stalled one, or stop whatever holds the
// conversation next.
func TestStall_AnIdleParkWithASlowSnapshotFilesNoStop(t *testing.T) {
	const idle = 400 * time.Millisecond
	f := newWorkspaceStallFixture(t, "r-park-slow-snapshot", activityTimings{idle: idle})
	f.s.SetStorage(&slowPutStorage{Storage: f.s.Storage(), delay: 3 * idle})

	f.s.activityFor(f.conversationID).touch()
	disp := f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID},
		f.keyID, f.tree, "manual", runmode.LocalDefaultUserID, time.Now(),
		agentloop.Result{Kind: agentloop.ResultParked}, nil)
	if disp.fenced || disp.handedBack {
		t.Fatalf("disposition = %+v, want the idle park to land", disp)
	}
	if !f.blobExists(t) {
		t.Fatal("the park's snapshot was not written")
	}
	time.Sleep(2 * idle)

	if recorded, reason := f.stopIntent(t); recorded {
		t.Fatalf("a stop intent (%q) landed after the park released the claim", reason)
	}
	if errors.Is(context.Cause(f.claimCtx), errStalled) {
		t.Error("the watchdog stopped an engagement that had already parked")
	}
	var status, parkReason string
	if err := f.database.QueryRow(
		`SELECT COALESCE(status, ''), COALESCE(park_reason, '') FROM conversations WHERE id = ?`, f.conversationID,
	).Scan(&status, &parkReason); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != domain.StatusOpen || parkReason != string(domain.ParkReasonIdle) {
		t.Errorf("conversation = (%q, %q), want (open, idle)", status, parkReason)
	}
}

// TestStall_AShutdownHandBackWithASlowSnapshotFilesNoStop: the hand-back
// releases the claim before its snapshot too. A stop filed during that
// snapshot would take a conversation the queue is about to run again out of
// the queue.
func TestStall_AShutdownHandBackWithASlowSnapshotFilesNoStop(t *testing.T) {
	const idle = 400 * time.Millisecond
	f := newWorkspaceStallFixture(t, "r-handback-slow-snapshot", activityTimings{idle: idle})
	f.s.SetStorage(&slowPutStorage{Storage: f.s.Storage(), delay: 3 * idle})

	f.s.activityFor(f.conversationID).touch()
	if f.s.handBackOnShutdown(f.claimCtx, liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, namespace: f.keyID,
		claudeCwd: f.tree, claimID: f.claimID, runtime: domain.ConversationRuntimeNative,
	}, "") {
		t.Fatal("the hand-back was fenced")
	}
	time.Sleep(2 * idle)
	if recorded, reason := f.stopIntent(t); recorded {
		t.Fatalf("a stop intent (%q) landed after the hand-back released the claim", reason)
	}
}

// TestStall_ASlowPreTerminalSnapshotWithinItsBoundCompletes: the snapshot a
// conclusion takes before its terminal write is work the engagement does
// while it holds the claim, and it is tracked as an operation with its own
// bound. A snapshot slower than the idle limit but inside that bound is not
// a stall, and the blob it writes lands.
func TestStall_ASlowPreTerminalSnapshotWithinItsBoundCompletes(t *testing.T) {
	const idle = 300 * time.Millisecond
	f := newWorkspaceStallFixture(t, "r-conclude-slow-snapshot", activityTimings{idle: idle, workspaceOp: time.Minute})
	f.s.SetStorage(&slowPutStorage{Storage: f.s.Storage(), delay: 4 * idle})

	f.s.activityFor(f.conversationID).touch()
	disp := f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID},
		f.keyID, f.tree, "manual", runmode.LocalDefaultUserID, time.Now(),
		agentloop.Result{Kind: agentloop.ResultConcluded, Outcome: domain.ConversationOutcomeFinish, ResultSummary: "done"}, nil)
	if disp.fenced {
		t.Fatalf("disposition = %+v, want the conclusion recorded", disp)
	}
	if cause := context.Cause(f.claimCtx); cause != nil {
		t.Fatalf("the engagement was stopped (%v) during a snapshot inside its bound", cause)
	}
	if !f.blobExists(t) {
		t.Fatal("the conclusion's snapshot was cut off; no blob was written")
	}
	assertSnapshotState(t, f.s, f.keyID, domain.WorkspaceSnapshotWritten, f.claimID)
	if got := storedStatus(t, f.database, f.conversationID); got != "completed" {
		t.Errorf("status = %q, want completed", got)
	}
}

// TestStall_APreTerminalSnapshotPastItsBoundFailsWithoutAStall: the snapshot's
// bound is what ends a snapshot that never finishes. The snapshot fails, best
// effort as ever, the conclusion is still recorded, and the watchdog behind
// the bound stays quiet.
func TestStall_APreTerminalSnapshotPastItsBoundFailsWithoutAStall(t *testing.T) {
	f := newWorkspaceStallFixture(t, "r-conclude-hung-snapshot", activityTimings{idle: time.Hour, workspaceOp: 100 * time.Millisecond})
	f.s.SetStorage(blockedPutStorage{Storage: f.s.Storage()})

	done := make(chan engagementDisposition, 1)
	go func() {
		done <- f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
			runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID},
			f.keyID, f.tree, "manual", runmode.LocalDefaultUserID, time.Now(),
			agentloop.Result{Kind: agentloop.ResultConcluded, Outcome: domain.ConversationOutcomeFinish, ResultSummary: "done"}, nil)
	}()
	select {
	case disp := <-done:
		if disp.fenced {
			t.Fatalf("disposition = %+v, want the conclusion recorded", disp)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a snapshot that never finishes held the conclusion past its bound")
	}
	if cause := context.Cause(f.claimCtx); cause != nil {
		t.Fatalf("the engagement was stopped (%v); the snapshot's own bound should have ended it", cause)
	}
	if got := storedStatus(t, f.database, f.conversationID); got != "completed" {
		t.Errorf("status = %q, want completed", got)
	}
	assertSnapshotState(t, f.s, f.keyID, domain.WorkspaceSnapshotFailed, f.claimID)
}

// TestStall_AReleasedClaimFilesNoStop: a claim released from under the
// engagement — expiry handling after its lease lapsed, a takeover — leaves
// the conversation to whoever holds it next. A stall decided after that
// stands down rather than filing a stop against it.
func TestStall_AReleasedClaimFilesNoStop(t *testing.T) {
	f := newStallFixture(t, "r-stall-after-release", activityTimings{idle: time.Hour})
	if _, err := f.database.Exec(
		`UPDATE claims SET released_at = CURRENT_TIMESTAMP, outcome = 'reaped' WHERE id = ?`, f.claimID,
	); err != nil {
		t.Fatalf("release the claim: %v", err)
	}
	conv := &domain.Conversation{ID: f.conversationID, OrgID: runmode.LocalDefaultOrgID, ClaimID: f.claimID}

	f.s.stallEngagement(conv, f.fence, stallCause{elapsed: time.Minute})
	if recorded, reason := f.stopIntent(t); recorded {
		t.Fatalf("a stall decided after the claim was released filed a stop intent (%q)", reason)
	}
	if errors.Is(context.Cause(f.claimCtx), errStalled) {
		t.Error("a stall decided after the claim was released stopped the engagement as stalled")
	}

	// The control: the same stall on a live claim stops it.
	g := newStallFixture(t, "r-stall-live-claim", activityTimings{idle: time.Hour})
	g.s.stallEngagement(&domain.Conversation{ID: g.conversationID, OrgID: runmode.LocalDefaultOrgID, ClaimID: g.claimID}, g.fence, stallCause{elapsed: time.Minute})
	if recorded, reason := g.stopIntent(t); !recorded || reason != string(domain.ParkReasonStalled) {
		t.Errorf("stop intent on a live claim = (%v, %q), want (true, stalled)", recorded, reason)
	}
}

// TestReleaseActivity_LeavesASuccessorsWatchdogAlone: the release names the
// claim it lets go of, so an engagement that outlived a takeover in this
// process cannot end the watchdog of the one that took over.
func TestReleaseActivity_LeavesASuccessorsWatchdogAlone(t *testing.T) {
	s := &Spawner{activity: map[string]*activityTracker{}}
	rec := newStallRecorder()
	stop := s.startActivityTracker(&domain.Conversation{ID: "c-takeover", ClaimID: "claim-successor"}, func(error) {})
	defer stop()
	s.activityFor("c-takeover").stalled = rec.stalled
	s.activityFor("c-takeover").begin("tool:bash", 60*time.Millisecond)

	s.releaseActivity("c-takeover", "claim-predecessor")
	if cause := rec.waitFired(t, 5*time.Second); cause.op != "tool:bash" {
		t.Errorf("stall op = %q, want the successor's tool:bash", cause.op)
	}

	rec2 := newStallRecorder()
	stop2 := s.startActivityTracker(&domain.Conversation{ID: "c-own", ClaimID: "claim-own"}, func(error) {})
	defer stop2()
	s.activityFor("c-own").stalled = rec2.stalled
	s.activityFor("c-own").begin("tool:bash", 60*time.Millisecond)
	s.releaseActivity("c-own", "claim-own")
	rec2.assertQuietFor(t, 200*time.Millisecond, "after the engagement released its own claim")
}

// TestActivityTracker_StopWaitsForADecidedStall: a stall the watchdog decided
// just before the engagement let go is finished before stop returns, so the
// release that follows never races the stop the stall is filing.
func TestActivityTracker_StopWaitsForADecidedStall(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	a := newActivityTracker(time.Hour, func(stallCause) {
		close(entered)
		<-release
	})
	a.mu.Lock()
	a.lastActivity = time.Now().Add(-2 * time.Hour)
	a.mu.Unlock()
	go a.check()
	waitFor(t, entered, "the stall callback")

	stopped := make(chan struct{})
	go func() {
		a.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while the decided stall was still filing")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	waitFor(t, stopped, "stop after the stall finished")
}
