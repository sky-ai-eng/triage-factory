package delegate

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

func TestParseSnapshotInterval(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"", DefaultSnapshotInterval, false},
		{"0", 0, false},
		{"60", time.Minute, false},
		{" 30 ", 30 * time.Second, false},
		{"-1", DefaultSnapshotInterval, true},
		{"5m", DefaultSnapshotInterval, true},
		{"soon", DefaultSnapshotInterval, true},
	}
	for _, tc := range cases {
		got, err := ParseSnapshotInterval(tc.raw)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("ParseSnapshotInterval(%q) = (%s, %v), want (%s, error=%v)", tc.raw, got, err, tc.want, tc.wantErr)
		}
	}
}

// TestCheckpoint_ZeroIntervalTakesNone: TF_SNAPSHOT_INTERVAL_SEC=0 disables
// checkpoints outright — no checkpointer, and the engagement runs the hooks it
// always ran, with no barrier and no batch hook.
func TestCheckpoint_ZeroIntervalTakesNone(t *testing.T) {
	f := newCheckpointFixture(t, "r-ckpt-disabled")
	f.s.SetSnapshotInterval(0)
	if c := f.start(); c != nil {
		t.Fatal("a zero interval must take no checkpoints")
	}
	hooks := checkpointHooks(nil, agentloop.Hooks{})
	if hooks.BeforeToolCall != nil || hooks.AfterToolCall != nil || hooks.AfterToolBatch != nil {
		t.Error("with no checkpointer the engagement's hooks must be left exactly as they were")
	}
	if f.s.checkpointerFor(f.conversationID) != nil {
		t.Error("nothing may be registered for the renewal to report")
	}
}

// TestCheckpoint_DueOnlyAfterTheIntervalAndASandboxCall: a batch boundary is
// not enough. The interval must have passed since the last checkpoint started
// and the jail must have run a tool since; loop-side calls never reach the
// jail and do not count.
func TestCheckpoint_DueOnlyAfterTheIntervalAndASandboxCall(t *testing.T) {
	f := newCheckpointFixture(t, "r-ckpt-due")
	c := f.start()
	defer c.stop()

	// A tool ran, but the interval has not passed.
	c.noteToolCall()
	c.afterToolBatch(context.Background(), 5)
	c.wg.Wait()
	if f.blobExists(t) || f.outcomes(checkpointWritten) != 0 {
		t.Fatal("a checkpoint was taken before the interval passed")
	}

	// The interval passed, but no tool ran since the last checkpoint started:
	// a batch of only loop-side calls looks exactly like this.
	c.mu.Lock()
	c.sandboxCalls = 0
	c.mu.Unlock()
	f.pastInterval(c)
	c.afterToolBatch(context.Background(), 6)
	c.wg.Wait()
	if f.blobExists(t) {
		t.Fatal("a checkpoint was taken with no tool run since the last one")
	}

	// Both hold.
	c.noteToolCall()
	c.afterToolBatch(context.Background(), 7)
	c.wg.Wait()
	if got := f.outcomes(checkpointWritten); got != 1 {
		t.Fatalf("written checkpoints = %d, want 1", got)
	}
	man, files := f.blob(t)
	if man.TranscriptPosition == nil || *man.TranscriptPosition != 7 || man.ConversationID != f.conversationID {
		t.Errorf("manifest position = (%v, %q), want (7, %q)", man.TranscriptPosition, man.ConversationID, f.conversationID)
	}
	if man.CapturedAt.IsZero() {
		t.Error("a checkpoint's manifest must say when the tree was read")
	}
	if files["notes.txt"] != "initial" {
		t.Errorf("the blob must carry the tree: scratch = %v", files)
	}
	assertSnapshotState(t, f.s, f.keyID, domain.WorkspaceSnapshotWritten, f.claimID)

	// And the renewal now reports how recently the tree was covered.
	if age, ok := f.s.checkpointerFor(f.conversationID).age(); !ok || age > time.Minute {
		t.Errorf("checkpoint age = (%s, %v), want a fresh reading", age, ok)
	}
}

// TestCheckpoint_ThroughTheEngine wires the checkpointer into the real loop: a
// batch that ran a tool is followed by a checkpoint at the position of its last
// result, and a batch of only loop-side calls is followed by none.
func TestCheckpoint_ThroughTheEngine(t *testing.T) {
	f := newCheckpointFixture(t, "r-ckpt-engine")
	c := f.start()
	f.pastInterval(c)
	tools := &treeToolHost{root: f.tree}
	provider := &scriptedGateProvider{turns: []gateTurn{
		{calls: []domain.ToolCall{bashCall("c1", "write one.txt")}},
		// A stop with no reason is corrected in the loop and ends nothing:
		// a batch that dispatched nothing into the jail.
		{calls: []domain.ToolCall{{ID: "c2", Name: agentloop.ToolStopBlueprint, Input: map[string]any{"type": "abort"}}}},
		{text: "done"},
	}}

	result := f.runEngine(t, context.Background(), c, provider, tools, agentloop.Params{})
	if result.Kind != agentloop.ResultConcluded {
		t.Fatalf("result = %v (err %v), want concluded", result.Kind, result.Err)
	}
	// The upload runs beside the loop and can outlast it; stopping now would
	// cancel it, which is the ending's behavior and not what is under test.
	c.wg.Wait()
	c.stop()

	if got := f.outcomes(checkpointWritten); got != 1 {
		t.Fatalf("written checkpoints = %d, want exactly one: after the batch that ran a tool, none after the loop-side one", got)
	}
	for _, o := range []string{checkpointSkippedBusy, checkpointSkippedUnchanged, checkpointFailed} {
		if got := f.outcomes(o); got != 0 {
			t.Errorf("%s checkpoints = %d, want none — the loop-side batch is not a checkpoint that came due", o, got)
		}
	}
	man, files := f.blob(t)
	if files["one.txt"] != "one.txt" {
		t.Errorf("the checkpoint must hold what the batch wrote: scratch = %v", files)
	}
	c1 := f.resultRow(t, "c1")
	if man.TranscriptPosition == nil || *man.TranscriptPosition != c1 {
		t.Errorf("manifest position = %v, want %v — c1's result, the batch's last row", man.TranscriptPosition, c1)
	}
}

// TestCheckpoint_BarrierHoldsTheNextToolCallNotTheModelCall: the capture runs
// while the model thinks, so the model call after a checkpoint's batch is not
// delayed by it; the next tool call is, until the capture phase is over, and
// the wait is an operation the stall watchdog can see.
func TestCheckpoint_BarrierHoldsTheNextToolCallNotTheModelCall(t *testing.T) {
	f := newCheckpointFixture(t, "r-ckpt-barrier")
	captureStarted := make(chan struct{})
	releaseCapture := make(chan struct{})
	prev := checkpointCapture
	checkpointCapture = func(ctx context.Context, w snapshotWrite) (*capturedSnapshot, error) {
		close(captureStarted)
		<-releaseCapture
		return prev(ctx, w)
	}
	t.Cleanup(func() { checkpointCapture = prev })

	c := f.start()
	f.pastInterval(c)
	tools := &treeToolHost{root: f.tree}
	secondModelCall := make(chan struct{})
	provider := &hookedProvider{
		inner: &scriptedGateProvider{turns: []gateTurn{
			{calls: []domain.ToolCall{bashCall("c1", "write one.txt")}},
			{calls: []domain.ToolCall{bashCall("c2", "write two.txt")}},
			{text: "done"},
		}},
		onCall: func(n int) {
			if n == 2 {
				close(secondModelCall)
			}
		},
	}
	done := make(chan agentloop.Result, 1)
	go func() { done <- f.runEngine(t, context.Background(), c, provider, tools, agentloop.Params{}) }()

	waitFor(t, captureStarted, "the checkpoint's capture to start")
	// The model is asked for its next turn with the capture still held.
	waitFor(t, secondModelCall, "the model call after the checkpoint's batch — the capture must not delay it")

	// And the tool call it returned waits.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, op := f.s.activityFor(f.conversationID).snapshot(); op == "checkpoint" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, op := f.s.activityFor(f.conversationID).snapshot(); op != "checkpoint" {
		t.Errorf("the barrier's wait is operation %q, want checkpoint", op)
	}
	time.Sleep(100 * time.Millisecond)
	if got := tools.commands(); len(got) != 1 {
		t.Fatalf("tools run with the capture still reading the tree = %v, want only c1", got)
	}

	close(releaseCapture)
	select {
	case result := <-done:
		if result.Kind != agentloop.ResultConcluded {
			t.Fatalf("result = %v (err %v), want concluded", result.Kind, result.Err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the engagement never finished once the capture was released")
	}
	c.wg.Wait() // let the upload land rather than cancel it
	c.stop()
	if got := tools.commands(); len(got) != 2 {
		t.Errorf("tools run = %v, want c2 to run once the capture ended", got)
	}
	_, files := f.blob(t)
	if _, ok := files["two.txt"]; ok {
		t.Error("the checkpoint holds c2's write; it must be the tree as it stood after c1's batch")
	}
}

// TestCheckpoint_UnchangedTreeIsNotStored: a checkpoint whose tree matches the
// last one written costs a capture and nothing else — no upload.
func TestCheckpoint_UnchangedTreeIsNotStored(t *testing.T) {
	f := newCheckpointFixture(t, "r-ckpt-unchanged")
	puts := &countingPutStorage{Storage: f.s.Storage()}
	f.s.SetStorage(puts)
	c := f.start()
	defer c.stop()

	f.due(c)
	c.afterToolBatch(context.Background(), 5)
	c.wg.Wait()
	f.due(c)
	c.afterToolBatch(context.Background(), 9)
	c.wg.Wait()

	if got := f.outcomes(checkpointSkippedUnchanged); got != 1 {
		t.Fatalf("unchanged checkpoints = %d, want the second to be skipped", got)
	}
	if got := puts.count(); got != 1 {
		t.Fatalf("uploads = %d, want only the first checkpoint's", got)
	}
	man, _ := f.blob(t)
	if man.TranscriptPosition == nil || *man.TranscriptPosition != 5 {
		t.Errorf("stored position = %v, want 5 — nothing may overwrite a blob that already holds this tree", man.TranscriptPosition)
	}

	// A change is stored.
	writeFile(t, filepath.Join(f.tree, worktree.ScratchDir, "notes.txt"), "changed since")
	f.due(c)
	c.afterToolBatch(context.Background(), 12)
	c.wg.Wait()
	if got := puts.count(); got != 2 {
		t.Fatalf("uploads = %d, want the changed tree stored", got)
	}
}

// TestCheckpoint_SingleFlightAndHostSlotsSkipRatherThanQueue: one checkpoint
// in flight per engagement, and two capturing per host. Either limit skips the
// checkpoint that finds it reached; nothing queues behind it.
func TestCheckpoint_SingleFlightAndHostSlotsSkipRatherThanQueue(t *testing.T) {
	f := newCheckpointFixture(t, "r-ckpt-busy")
	held := &heldPutStorage{Storage: f.s.Storage(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	f.s.SetStorage(held)
	c := f.start()
	defer c.stop()

	f.due(c)
	c.afterToolBatch(context.Background(), 5)
	waitFor(t, held.entered, "the first checkpoint's upload")

	// Due again while the first is still uploading.
	f.due(c)
	c.afterToolBatch(context.Background(), 9)
	if got := f.outcomes(checkpointSkippedBusy); got != 1 {
		t.Fatalf("busy skips = %d, want the second checkpoint skipped while the first uploads", got)
	}
	close(held.release)
	c.wg.Wait()
	if got := f.outcomes(checkpointWritten); got != 1 {
		t.Fatalf("written = %d, want only the first — the skipped one must not have been queued behind it", got)
	}

	// Every host slot taken by other engagements: skipped, not queued.
	for range checkpointHostSlots {
		if !f.s.acquireCheckpointSlot() {
			t.Fatal("could not take a host slot")
		}
	}
	defer func() {
		for range checkpointHostSlots {
			f.s.releaseCheckpointSlot()
		}
	}()
	writeFile(t, filepath.Join(f.tree, worktree.ScratchDir, "notes.txt"), "changed since")
	f.due(c)
	c.afterToolBatch(context.Background(), 12)
	c.wg.Wait()
	if got := f.outcomes(checkpointSkippedBusy); got != 2 {
		t.Errorf("busy skips = %d, want the checkpoint that found no host slot skipped", got)
	}
	man, _ := f.blob(t)
	if man.TranscriptPosition == nil || *man.TranscriptPosition != 5 {
		t.Errorf("stored position = %v, want the first checkpoint's 5", man.TranscriptPosition)
	}
}

// TestCheckpoint_AnEndingDuringASlowUploadOwnsTheRecordAndTheBlob is the
// ordering trap, through the real record and the real ending: an engagement
// ends while its checkpoint is still uploading an older tree. The ending's
// snapshot must be what the key holds afterwards, under a record that says
// written — whichever way the two writers would otherwise have interleaved.
//
// recordNativeResult stopping the checkpointer and waiting for it is the whole
// defence. Without it the ending uploads and closes its record, the held
// checkpoint upload lands after it, and the key holds the older tree.
func TestCheckpoint_AnEndingDuringASlowUploadOwnsTheRecordAndTheBlob(t *testing.T) {
	f := newCheckpointFixture(t, "r-ckpt-ending")
	held := &firstPutHeld{Storage: f.s.Storage(), entered: make(chan struct{}), release: make(chan struct{})}
	f.s.SetStorage(held)
	state := filepath.Join(f.tree, worktree.ScratchDir, "state.txt")
	writeFile(t, state, "as the checkpoint read it")

	c := f.start()
	f.due(c)
	c.afterToolBatch(context.Background(), 5)
	waitFor(t, held.entered, "the checkpoint's upload")

	// The agent carries on after the checkpoint and then the engagement ends:
	// a guard park, the ending with the most ordinary shape.
	writeFile(t, state, "as the ending left it")
	disp := f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID},
		f.keyID, f.tree, "manual", runmode.LocalDefaultUserID, time.Now(),
		agentloop.Result{Kind: agentloop.ResultParked, ParkNotice: "spend cap"}, nil)
	if disp.fenced {
		t.Fatalf("disposition = %+v, want the park to land", disp)
	}

	// Whatever the checkpoint was still holding is let go now, and given the
	// chance to land.
	close(held.release)
	c.wg.Wait()

	_, files := f.blob(t)
	if got := files["state.txt"]; got != "as the ending left it" {
		t.Fatalf("the key holds %q, want the ending's tree — an older checkpoint landed over the ending's snapshot", got)
	}
	assertSnapshotState(t, f.s, f.keyID, domain.WorkspaceSnapshotWritten, f.claimID)
	if got := f.outcomes(checkpointWritten); got != 0 {
		t.Errorf("written checkpoints = %d, want the one the ending cut off to count as not written", got)
	}
}

// TestCheckpoint_AHardKillRestoresTheCheckpointAndNamesWhatCameAfter is the
// crash the feature exists for. An engagement checkpoints after its first
// batch, runs two more calls, and dies inside a fourth with no ending at all —
// no park, no hand-back, its tree gone with its node. The successor restores
// the checkpoint, and the first thing it sends the model names exactly the
// calls whose work the tree no longer holds.
func TestCheckpoint_AHardKillRestoresTheCheckpointAndNamesWhatCameAfter(t *testing.T) {
	f := newCheckpointFixture(t, "r-ckpt-hard-kill")
	c := f.start()
	f.pastInterval(c)

	killCtx, kill := context.WithCancel(context.Background())
	defer kill()
	tools := &treeToolHost{root: f.tree, blockOn: "write four.txt", ctx: killCtx, entered: make(chan struct{})}
	provider := &hookedProvider{
		inner: &scriptedGateProvider{turns: []gateTurn{
			{calls: []domain.ToolCall{bashCall("c1", "write one.txt")}},
			{calls: []domain.ToolCall{bashCall("c2", "write two.txt"), bashCall("c3", "write three.txt")}},
			{calls: []domain.ToolCall{bashCall("c4", "write four.txt")}},
		}},
		onCall: func(n int) {
			if n == 2 {
				// One checkpoint, after the first batch: it has landed before
				// the model answers, and none comes due after it.
				c.wg.Wait()
				c.mu.Lock()
				c.interval = time.Hour
				c.mu.Unlock()
			}
		},
	}
	done := make(chan agentloop.Result, 1)
	go func() { done <- f.runEngine(t, killCtx, c, provider, tools, agentloop.Params{}) }()
	waitFor(t, tools.entered, "the fourth call to be running")

	// The executor dies. Nothing it would have done on the way out happens:
	// the checkpointer simply stops existing, and so does the tree.
	kill()
	<-done
	c.stop()
	if got := f.outcomes(checkpointWritten); got != 1 {
		t.Fatalf("written checkpoints = %d, want the one after the first batch", got)
	}
	if err := os.RemoveAll(f.tree); err != nil {
		t.Fatal(err)
	}
	// The takeover a successor's dispatcher runs once the lease lapses.
	if _, err := f.database.Exec(`UPDATE claims SET released_at = CURRENT_TIMESTAMP, outcome = 'reaped' WHERE id = ?`, f.claimID); err != nil {
		t.Fatalf("take the claim over: %v", err)
	}
	next, err := f.s.conversationQueue.ClaimNextConversation(context.Background(), "exec-successor", 1, db.ClaimPlacement{}, time.Minute)
	if err != nil || next == nil || next.ID != f.conversationID {
		t.Fatalf("successor claim = (%+v, %v), want conversation %s", next, err, f.conversationID)
	}

	wt, prov, asOf, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, &domain.Conversation{
		ID: f.conversationID, ClaimID: next.ClaimID, TaskID: f.task.ID,
		Runtime: domain.ConversationRuntimeNative, WorktreePath: f.tree,
	}, gitSeed{}, nil)
	if err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	if prov != domain.WorkspaceProvenanceRehydrated {
		t.Fatalf("provenance = %q, want rehydrated from the checkpoint", prov)
	}
	if c1 := f.resultRow(t, "c1"); asOf == nil || *asOf != c1 {
		t.Fatalf("restored position = %v, want %v — c1's result, where the checkpoint was taken", asOf, c1)
	}
	assertFileContains(t, filepath.Join(wt, worktree.ScratchDir, "one.txt"), "one.txt")
	for _, gone := range []string{"two.txt", "three.txt", "four.txt"} {
		assertMissing(t, filepath.Join(wt, worktree.ScratchDir, gone))
	}

	// The successor's engine, exactly as runNativeAgent hands it the tree.
	capture := capturingProvider{rows: make(chan []domain.Message, 1)}
	successorCtx, stopSuccessor := context.WithCancel(context.Background())
	defer stopSuccessor()
	successorDone := make(chan struct{})
	go func() {
		defer close(successorDone)
		engine := &agentloop.Engine{
			Transcript:  newNativeTranscript(f.s, runmode.LocalDefaultOrgID, f.conversationID, next.ClaimID),
			Credentials: staticGateCredentials{provider: capture},
			Tools:       &recordingToolHost{},
			Retry:       agentloop.RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }},
		}
		engine.Run(successorCtx, agentloop.Params{
			OrgID: runmode.LocalDefaultOrgID, ConversationID: f.conversationID,
			Model: "claude-sonnet-4-5", SystemPrompt: "system", HasBlueprint: true,
			Workspace: prov, WorkspaceAsOf: asOf,
		})
	}()
	var rows []domain.Message
	select {
	case rows = <-capture.rows:
	case <-time.After(10 * time.Second):
		t.Fatal("the successor never asked the model anything")
	}
	stopSuccessor()
	<-successorDone

	var notice, c4 string
	for _, r := range rows {
		if r.Subtype == domain.MessageSubtypeInjectionExecutorChanged {
			notice = r.Content
		}
		if r.Role == "tool" && r.ToolCallID == "c4" {
			c4 = r.Content
		}
	}
	if !strings.Contains(notice, "`bash: write two.txt` and the 2 tool calls after it came after the checkpoint") {
		t.Errorf("the first request's notice must name exactly the calls after the checkpoint (c2, c3, c4): %q", notice)
	}
	if strings.Contains(notice, "write one.txt") {
		t.Errorf("c1 is in the restored tree and must not be named as missing: %q", notice)
	}
	if !strings.Contains(c4, "checkpoint taken before this call ran") {
		t.Errorf("the call the kill cut off must be answered as absent from the restored tree: %q", c4)
	}
}

// TestSnapshotManifest_TranscriptPosition: a checkpoint's position survives the
// blob, belongs to the conversation that wrote it and to no other on the task,
// and a blob with none — every ending's, and every blob written before
// checkpoints — restores with none.
func TestSnapshotManifest_TranscriptPosition(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s := newStorageSpawner(t)
	src := t.TempDir()
	writeFile(t, filepath.Join(src, worktree.ScratchDir, "a.txt"), "a")

	pos := 41.5
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	withPos := stagedBlob(t, src, snapshotManifest{TranscriptPosition: &pos, ConversationID: "conv-a", CapturedAt: at})
	man, err := s.rehydrateFromSnapshot(context.Background(), filepath.Join(t.TempDir(), "a"), gitSeed{}, withPos)
	if err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if got := man.positionFor("conv-a"); got == nil || *got != pos {
		t.Errorf("position for its own conversation = %v, want %v", got, pos)
	}
	if got := man.positionFor("conv-b"); got != nil {
		t.Errorf("position for another conversation on the task = %v, want none", *got)
	}
	if !man.CapturedAt.Equal(at) {
		t.Errorf("captured_at = %s, want %s", man.CapturedAt, at)
	}

	man, err = s.rehydrateFromSnapshot(context.Background(), filepath.Join(t.TempDir(), "b"), gitSeed{}, stagedBlob(t, src, snapshotManifest{}))
	if err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if got := man.positionFor("conv-a"); got != nil {
		t.Errorf("a blob with no position restored with %v, want none", *got)
	}
	var raw strings.Builder
	if err := writeSnapshotTar(context.Background(), &raw, worktree.CapturedState{}, src, snapshotManifest{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw.String(), "transcript_position") || strings.Contains(raw.String(), "conversation_id") {
		t.Error("a snapshot with no position must not write the keys at all")
	}

	// A manifest from before the fields existed parses to the same answer.
	var legacy snapshotManifest
	if err := json.Unmarshal([]byte(`{"branch":"","head":"","session_id":"","has_git":false}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.positionFor("conv-a") != nil || !legacy.CapturedAt.IsZero() {
		t.Errorf("a pre-checkpoint manifest reads as %+v, want no position and no capture time", legacy)
	}
}

// TestRenewClaimLease_ReportsTheCheckpointAge: the renewal carries how long a
// checkpointing engagement's workspace has gone uncovered, and nothing for an
// engagement that does not checkpoint, which the store stamps as NULL.
func TestRenewClaimLease_ReportsTheCheckpointAge(t *testing.T) {
	for _, tc := range []struct {
		name        string
		checkpoints bool
	}{{"checkpointing", true}, {"not checkpointing", false}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRenewalStore{}
			s := leaseTestSpawner(t, fake, 10*time.Millisecond, time.Second, 90*time.Second)
			conv := leaseTestConversation()
			if tc.checkpoints {
				s.checkpointers = map[string]*checkpointer{conv.ID: {s: s, coveredAt: time.Now().Add(-90 * time.Second)}}
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); s.renewClaimLease(ctx, conv, time.Now(), func(error) {}) }()
			deadline := time.After(5 * time.Second)
			for len(fake.seen()) == 0 {
				select {
				case <-deadline:
					t.Fatal("the loop made no renewal")
				case <-time.After(5 * time.Millisecond):
				}
			}
			cancel()
			<-done

			got := fake.seen()[0].checkpointAge
			if !tc.checkpoints {
				if got != nil {
					t.Errorf("checkpoint age = %s, want none for an engagement that does not checkpoint", *got)
				}
				return
			}
			if got == nil || *got < 90*time.Second || *got > 95*time.Second {
				t.Errorf("checkpoint age = %v, want about 90s", got)
			}
		})
	}
}

// --- fixtures ----------------------------------------------------------------

// checkpointFixture is a claimed native conversation with a blob store, a run
// tree at the task's run root, and the checkpoint counter backed by a manual
// reader.
type checkpointFixture struct {
	stallFixture
	keyID    string
	tree     string
	outcomes func(outcome string) int64
}

func newCheckpointFixture(t *testing.T, conversationID string) checkpointFixture {
	t.Helper()
	paths.SetForTest(t, t.TempDir())
	f := checkpointFixture{stallFixture: newStallFixture(t, conversationID, activityTimings{idle: time.Hour})}
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

func (f checkpointFixture) start() *checkpointer {
	return f.s.startCheckpointer(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.keyID, f.claimID, f.tree)
}

// pastInterval makes the interval read as elapsed since the last checkpoint
// started.
func (f checkpointFixture) pastInterval(c *checkpointer) {
	c.mu.Lock()
	c.lastStarted = time.Now().Add(-2 * c.interval)
	c.mu.Unlock()
}

// due makes a checkpoint due at the next boundary: the interval elapsed and a
// tool run since.
func (f checkpointFixture) due(c *checkpointer) {
	f.pastInterval(c)
	c.noteToolCall()
}

func (f checkpointFixture) blobExists(t *testing.T) bool {
	t.Helper()
	ok, err := f.s.Storage().Exists(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, f.keyID))
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	return ok
}

// blob reads the key's blob back: its manifest and its scratch files.
func (f checkpointFixture) blob(t *testing.T) (snapshotManifest, map[string]string) {
	t.Helper()
	rc, err := f.s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, f.keyID))
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	cr, _, err := snapshotReader(rc)
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	defer func() { _ = cr.Close() }()
	var man snapshotManifest
	files := map[string]string{}
	tr := tar.NewReader(cr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read blob: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read member %s: %v", hdr.Name, err)
		}
		switch {
		case hdr.Name == snapManifest:
			if err := json.Unmarshal(data, &man); err != nil {
				t.Fatalf("manifest: %v", err)
			}
		case strings.HasPrefix(hdr.Name, snapScratchPrefix):
			files[strings.TrimPrefix(hdr.Name, snapScratchPrefix)] = string(data)
		}
	}
	return man, files
}

// resultRow is the assembly key of a call's result row.
func (f checkpointFixture) resultRow(t *testing.T, callID string) float64 {
	t.Helper()
	rows, err := f.s.conversations.ListForAssemblySystem(context.Background(), runmode.LocalDefaultOrgID, f.conversationID)
	if err != nil {
		t.Fatalf("list rows: %v", err)
	}
	for _, r := range rows {
		if r.Role == "tool" && r.ToolCallID == callID {
			if r.Seq != nil {
				return *r.Seq
			}
			return float64(r.ID)
		}
	}
	t.Fatalf("no result for %s", callID)
	return 0
}

// runEngine drives the real engine over the real transcript with the
// checkpointer composed in, the way runNativeAgent does.
func (f checkpointFixture) runEngine(t *testing.T, ctx context.Context, c *checkpointer, provider agentloop.Provider, tools agentloop.ToolHost, params agentloop.Params) agentloop.Result {
	t.Helper()
	transcript := newNativeTranscript(f.s, runmode.LocalDefaultOrgID, f.conversationID, f.claimID)
	if _, err := transcript.Insert(context.Background(), runmode.LocalDefaultOrgID, pendingUserInput(f.conversationID, runmode.LocalDefaultUserID, "do the work")); err != nil {
		t.Errorf("seed the mission: %v", err)
	}
	timings := f.s.resolvedActivityTimings()
	engine := &agentloop.Engine{
		Transcript:  transcript,
		Credentials: staticGateCredentials{provider: provider},
		Tools:       tools,
		Hooks:       checkpointHooks(c, agentloop.Hooks{}),
		Activity:    engineActivity{f.s.activityFor(f.conversationID)},
		ActivityBounds: agentloop.ActivityBounds{
			Provider: timings.providerByte,
			Tool:     timings.toolCall,
		},
		Retry: agentloop.RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }},
	}
	params.OrgID = runmode.LocalDefaultOrgID
	params.ConversationID = f.conversationID
	params.Model = "claude-sonnet-4-5"
	params.SystemPrompt = "system"
	params.HasBlueprint = true
	return engine.Run(ctx, params)
}

// checkpointCounter swaps the checkpoint counter for one backed by a manual
// reader and returns a read of one outcome.
func checkpointCounter(t *testing.T) func(outcome string) int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := workspaceCheckpoints.Swap(newWorkspaceCheckpointStats(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { workspaceCheckpoints.Store(prev) })
	return func(outcome string) int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "workspace.checkpoints" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("workspace.checkpoints is %T, want an int64 sum", m.Data)
				}
				for _, dp := range sum.DataPoints {
					if v, ok := dp.Attributes.Value(attribute.Key("outcome")); ok && v.AsString() == outcome {
						return dp.Value
					}
				}
			}
		}
		return 0
	}
}

// stagedBlob archives src the way every snapshot does and returns the
// compressed blob to read back.
func stagedBlob(t *testing.T, src string, man snapshotManifest) io.Reader {
	t.Helper()
	f, _, _, err := stageSnapshotArchive(context.Background(), worktree.CapturedState{}, src, man)
	if err != nil {
		t.Fatalf("stage archive: %v", err)
	}
	t.Cleanup(func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	})
	return f
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// treeToolHost is a jail that writes into the run tree: `write NAME` puts a
// file NAME under the scratch dir holding its own name. blockOn names a
// command that instead holds until ctx ends, reporting on entered when it
// starts — a tool call a kill lands inside.
type treeToolHost struct {
	root    string
	blockOn string
	ctx     context.Context
	entered chan struct{}

	mu       sync.Mutex
	observed []string
}

func (h *treeToolHost) Call(_ string, args map[string]any) (agentloop.ToolOutcome, error) {
	cmd, _ := args["command"].(string)
	h.mu.Lock()
	h.observed = append(h.observed, cmd)
	h.mu.Unlock()
	if cmd == h.blockOn && h.blockOn != "" {
		close(h.entered)
		<-h.ctx.Done()
		return agentloop.ToolOutcome{}, errors.New("agentloop: read tool response: use of closed network connection")
	}
	if name, ok := strings.CutPrefix(cmd, "write "); ok {
		path := filepath.Join(h.root, worktree.ScratchDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return agentloop.ToolOutcome{ToolError: err.Error()}, nil
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			return agentloop.ToolOutcome{ToolError: err.Error()}, nil
		}
	}
	return agentloop.ToolOutcome{Content: "ok"}, nil
}

func (h *treeToolHost) Close() error { return nil }

func (h *treeToolHost) commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.observed...)
}

// hookedProvider runs onCall with each request's 1-based ordinal before
// answering it.
type hookedProvider struct {
	inner  agentloop.Provider
	onCall func(n int)

	mu sync.Mutex
	n  int
}

func (p *hookedProvider) Stream(ctx context.Context, req inference.Request) (*inference.Completion, error) {
	p.mu.Lock()
	p.n++
	n := p.n
	p.mu.Unlock()
	if p.onCall != nil {
		p.onCall(n)
	}
	return p.inner.Stream(ctx, req)
}

// countingPutStorage counts uploads.
type countingPutStorage struct {
	storage.Storage
	mu   sync.Mutex
	puts int
}

func (c *countingPutStorage) Put(ctx context.Context, key string, r io.Reader) error {
	c.mu.Lock()
	c.puts++
	c.mu.Unlock()
	return c.Storage.Put(ctx, key, r)
}

func (c *countingPutStorage) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts
}

// firstPutHeld holds the first upload until released — yielding to its own
// context, as heldPutStorage does — and passes every later one straight
// through, so one writer can be held open while another writes past it.
type firstPutHeld struct {
	storage.Storage
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *firstPutHeld) Put(ctx context.Context, key string, r io.Reader) error {
	first := false
	h.once.Do(func() { first = true })
	if first {
		close(h.entered)
		select {
		case <-h.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return h.Storage.Put(ctx, key, r)
}
