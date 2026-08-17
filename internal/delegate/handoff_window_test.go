// The hand-off window: the moments between a blueprint step writing its
// terminal and the reactor reading that terminal back, during which the
// conversation reads `completed` to every status-only gate and is nonetheless
// not a person's to continue.

package delegate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// giveRunResumeState fills in the fields an SDK resume re-invokes a session
// with, so the refusal ladder reaches the blueprint rungs under test.
func giveRunResumeState(t *testing.T, database *sql.DB, conversationID string) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE conversations SET sdk_session_id = ?, worktree_path = ? WHERE id = ?`,
		"sess-"+conversationID, "/tmp/wt-"+conversationID, conversationID); err != nil {
		t.Fatalf("give %s resume state: %v", conversationID, err)
	}
}

// TestFollowUp_ConcludedStepOfARunningBlueprintIsRefused is the reported
// failure, reproduced at the instant it happened: step 0 of a two-step
// blueprint has written its terminal and the reactor has not run yet. The
// follow-up sent there used to be accepted, which re-queued the row the reactor
// was about to read — so it read a status that was neither terminal nor open
// and failed the blueprint. Step 1 never ran and the workspace went with it.
func TestFollowUp_ConcludedStepOfARunningBlueprintIsRefused(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	s, database, brID, _, step0ConversationID := reactorFixture(t, "handoff", 2, "completed", "continue")
	giveRunResumeState(t, database, step0ConversationID)

	// The composer's half: the run detail read says no, with the rung the
	// frontend renders as "this step just handed off".
	if ok, reason := s.ResumabilityFor(ctx, org, loadRun(t, s, step0ConversationID)); ok || reason != ResumeBlockedStepHandedOff {
		t.Errorf("ResumabilityFor = (%v, %q), want (false, %q)", ok, reason, ResumeBlockedStepHandedOff)
	}

	// The send's half.
	err := s.SendMessage(ctx, org, step0ConversationID, runmode.LocalDefaultUserID, "actually, one more thing")
	if !errors.Is(err, ErrStepHandedOff) {
		t.Fatalf("SendMessage = %v, want ErrStepHandedOff", err)
	}
	if st := storedStatus(t, database, step0ConversationID); st != "completed" {
		t.Errorf("stored status = %q, want completed — a refused wake un-terminals nothing", st)
	}
	if got := pendingRows(t, s, step0ConversationID); len(got) != 0 {
		t.Errorf("a refused follow-up left %d queued rows behind; the gate runs before the write", len(got))
	}

	// The reactor, a beat later, finds exactly the terminal this engagement
	// wrote — so the blueprint advances and step 1 is enqueued.
	stepConversation := loadRun(t, s, step0ConversationID)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	if q := queuedStepRuns(t, database, brID); len(q) != 1 || q[0] != 1 {
		t.Fatalf("queued step runs = %v, want [1] — the next step must run", q)
	}
	br := mustGetRun(t, s, org, brID)
	if br.Status != domain.BlueprintRunStatusRunning {
		t.Errorf("blueprint status = %q, want running", br.Status)
	}
	if br.CurrentStepIndex != 1 {
		t.Errorf("current_step_index = %d, want 1", br.CurrentStepIndex)
	}

	// Past the advance the answer changes to the permanent one: step 0 is
	// behind the blueprint for good, and the moved-past refusal takes over.
	if ok, reason := s.ResumabilityFor(ctx, org, loadRun(t, s, step0ConversationID)); ok || reason != ResumeBlockedBlueprintConcluded {
		t.Errorf("after the advance: ResumabilityFor = (%v, %q), want (false, %q)", ok, reason, ResumeBlockedBlueprintConcluded)
	}
}

// TestFollowUp_FinalStepAfterTheBlueprintFinishesStillLands is the regression
// the refusal above could have introduced: refusing during the window must not
// refuse after it.
func TestFollowUp_FinalStepAfterTheBlueprintFinishesStillLands(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	s, database, brID, _, step0ConversationID := reactorFixture(t, "handoff-final", 1, "completed", "finish")
	giveRunResumeState(t, database, step0ConversationID)

	stepConversation := loadRun(t, s, step0ConversationID)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())
	if br := mustGetRun(t, s, org, brID); br.Status != domain.BlueprintRunStatusCompleted {
		t.Fatalf("blueprint status = %q, want completed before the follow-up", br.Status)
	}

	if ok, reason := s.ResumabilityFor(ctx, org, loadRun(t, s, step0ConversationID)); !ok {
		t.Errorf("ResumabilityFor = (false, %q), want resumable once the blueprint finished", reason)
	}
	if err := s.SendMessage(ctx, org, step0ConversationID, runmode.LocalDefaultUserID, "one more thing"); err != nil {
		t.Fatalf("follow-up on a finished blueprint's final step: %v", err)
	}
	if st := storedStatus(t, database, step0ConversationID); st != "" {
		t.Errorf("stored status = %q, want none — the wake un-terminals the row", st)
	}
	if got := pendingRows(t, s, step0ConversationID); len(got) != 1 {
		t.Errorf("queued follow-up rows = %d, want 1", len(got))
	}
}

// TestFollowUp_StopResumeOfAMidBlueprintStepStillLands states the arm that must
// NOT be fixed alongside the one above, since from a status-only distance the
// two look alike. A stopped step parked `open` never concluded, so no reactor is
// waiting on it — and when it does conclude, advancing the blueprint is right.
func TestFollowUp_StopResumeOfAMidBlueprintStepStillLands(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	s, database, brID, _, step0ConversationID := reactorFixture(t, "handoff-stop", 2, "open", "")
	giveRunResumeState(t, database, step0ConversationID)

	if err := s.SendMessage(ctx, org, step0ConversationID, runmode.LocalDefaultUserID, "keep going"); err != nil {
		t.Fatalf("stop→resume of a mid-blueprint step: %v", err)
	}
	if st := storedStatus(t, database, step0ConversationID); st != "" {
		t.Errorf("stored status = %q, want none — the parked step is claimable again", st)
	}

	// The resumed step concludes. The blueprint is still on it, so this
	// terminal is the one the reactor advances on.
	if _, err := database.Exec(
		`UPDATE conversations SET status = 'completed', outcome = 'continue' WHERE id = ?`, step0ConversationID); err != nil {
		t.Fatalf("conclude the resumed step: %v", err)
	}
	stepConversation := loadRun(t, s, step0ConversationID)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	if q := queuedStepRuns(t, database, brID); len(q) != 1 || q[0] != 1 {
		t.Fatalf("queued step runs = %v, want [1] — a resumed step's conclusion still advances", q)
	}
	if br := mustGetRun(t, s, org, brID); br.CurrentStepIndex != 1 {
		t.Errorf("current_step_index = %d, want 1", br.CurrentStepIndex)
	}
}

// putHookStorage wraps a real blob store so a test can observe the world at the
// exact moment a snapshot lands. Ordering is the property under test, and an
// after-the-fact assertion cannot see it.
type putHookStorage struct {
	storage.Storage
	before func(key string)
}

func (p putHookStorage) Put(ctx context.Context, key string, r io.Reader) error {
	if p.before != nil {
		p.before(key)
	}
	return p.Storage.Put(ctx, key, r)
}

// TestProcessCompletion_SnapshotLandsBeforeTheTerminalCommits pins the SDK
// path's write order against the guarantee it owes every wake: by the time a
// conversation reads `completed`, the workspace that status advertises is whole.
// The assertion is the ordering itself — both orders leave the same rows
// behind, so an end-state check would pass either way.
func TestProcessCompletion_SnapshotLandsBeforeTheTerminalCommits(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	s, database, conversationID, taskID := setupAdvanceFixture(t, "snap-order")

	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	var statusAtPut string
	var puts int
	s.SetStorage(putHookStorage{Storage: blobs, before: func(string) {
		puts++
		statusAtPut = storedStatus(t, database, conversationID)
	}})

	s.processCompletion(ctx, org, conversationID, "seedbpr-"+conversationID, "", loadTask(t, s, taskID),
		res(`{"outcome":"finish","summary":"done"}`), t.TempDir(), nil, "sess-snap-order", "event", "")

	if puts != 1 {
		t.Fatalf("snapshot writes = %d, want 1 — a completed terminal snapshots", puts)
	}
	if statusAtPut == "completed" {
		t.Errorf("the conversation already read completed while the snapshot was still being written; a wake in that window resumes a torn blob")
	}
	if st := storedStatus(t, database, conversationID); st != "completed" {
		t.Errorf("stored status = %q, want completed once both writes are done", st)
	}
}

// TestFollowUp_ImmediatelyAfterAConclusionFindsACompleteSnapshot is what the
// ordering above buys, end to end: a conversation with no reactor between it
// and a person, so the wake gate opens the moment the status commits and there
// is no window to hide an unfinished snapshot in. The blob is asserted whole —
// the session transcript an SDK resume re-invokes by id, and the ephemeral
// scratch tree that exists nowhere else.
func TestFollowUp_ImmediatelyAfterAConclusionFindsACompleteSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	s, database, conversationID, taskID := setupAdvanceFixture(t, "snap-wake")
	// A blueprint that already finished — the follow-up-on-concluded-work
	// shape, where a turn's conclusion advances nothing.
	settleRunBlueprint(t, database, conversationID, "completed")

	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s.SetStorage(blobs)

	const sessionID = "sess-snap-wake"
	cwd := t.TempDir()
	writeSession(t, cwd, sessionID, `{"type":"summary"}`)
	writeFile(t, filepath.Join(cwd, "_tfac", "ci-logs", "build.log"), "the turn's work")

	s.processCompletion(ctx, org, conversationID, "seedbpr-"+conversationID, "", loadTask(t, s, taskID),
		res(`{"outcome":"finish","summary":"done"}`), cwd, nil, sessionID, "event", "")

	// The wake a person can fire the instant the status flips.
	if err := s.SendMessage(ctx, org, conversationID, runmode.LocalDefaultUserID, "one more thing"); err != nil {
		t.Fatalf("follow-up immediately after the conclusion: %v", err)
	}
	if st := storedStatus(t, database, conversationID); st != "" {
		t.Errorf("stored status = %q, want none — the follow-up re-queued the row", st)
	}

	members := snapshotMembers(t, blobs, snapshotKey(org, "seedbpr-"+conversationID))
	for _, want := range []string{snapSession, snapScratchPrefix + "ci-logs/build.log"} {
		if !members[want] {
			t.Errorf("snapshot member %q missing; the resume this follow-up queued would rehydrate an incomplete workspace (members: %v)", want, members)
		}
	}
}

// snapshotMembers reads a snapshot blob's member names, so a test can assert
// what a resume would find without rebuilding a worktree.
func snapshotMembers(t *testing.T, blobs storage.Storage, key string) map[string]bool {
	t.Helper()
	rc, err := blobs.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get snapshot %s: %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	gzr, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatalf("open snapshot gzip: %v", err)
	}
	defer func() { _ = gzr.Close() }()
	out := map[string]bool{}
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("read snapshot tar: %v", err)
		}
		out[hdr.Name] = true
	}
}
