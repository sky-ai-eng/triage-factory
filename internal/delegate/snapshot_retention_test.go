package delegate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// TestProcessCompletion_DraftPRDoesNotPark: a completed step that queued a draft
// PR does not park. The artifact is an async sidecar — the step completes with
// its real outcome (continue) rather than waiting on a human — and like every
// completed terminal it snapshots on the way out.
func TestProcessCompletion_DraftPRDoesNotPark(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, taskID := setupAdvanceFixture(t, "draftpr-nopark")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	seedDraftPRArtifact(t, s, conversationID)

	task := loadTask(t, s, taskID)
	parked, _ := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, "", task,
		res(`{"outcome":"continue","summary":"opened a PR"}`), t.TempDir(), nil, "", "event", "")

	if parked {
		t.Fatal("processCompletion(draft PR) = true; want parked=false (a draft PR is a sidecar; the step never parks)")
	}
	if conv := loadConversation(t, s, conversationID); conv.Status != "completed" || conv.Outcome != "continue" {
		t.Fatalf("conv = {status:%q outcome:%q}, want {completed continue}", conv.Status, conv.Outcome)
	}
	assertSnapshotPresent(t, s, taskID, true)
}

// TestProcessCompletion_PlainAbortWritesSnapshot: a plain abort (completed +
// outcome=abort, no queued artifact) snapshots its workspace at the terminal
// write but returns parked=false — the worktree is torn down by the cleanup
// defers and cold rehydrate from the snapshot is the resume path.
func TestProcessCompletion_PlainAbortWritesSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, taskID := setupAdvanceFixture(t, "ab-snap")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	task := loadTask(t, s, taskID)
	parked, _ := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, "", task,
		res(`{"outcome":"abort","summary":"looked into it","reason":"needs a human to rotate the token"}`),
		t.TempDir(), nil, "", "event", "")

	if parked {
		t.Error("processCompletion(plain abort) = true; want parked=false (worktree torn down, snapshot is the resume path)")
	}
	if conv := loadConversation(t, s, conversationID); conv.Status != "completed" || conv.Outcome != "abort" {
		t.Fatalf("conv = {status:%q outcome:%q}, want {completed abort}", conv.Status, conv.Outcome)
	}
	assertSnapshotPresent(t, s, taskID, true)
}

// TestProcessCompletion_CleanFinishWritesSnapshot is the case the write policy
// was widened for: a clean finish snapshots its workspace at the terminal write,
// while the worktree and transcript are still on disk, so the work a successful
// run produced is still somewhere once the cleanup defers tear the tree down.
func TestProcessCompletion_CleanFinishWritesSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, taskID := setupAdvanceFixture(t, "fin-snap")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	task := loadTask(t, s, taskID)
	parked, _ := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, "", task,
		res(`{"outcome":"finish","summary":"shipped it"}`), t.TempDir(), nil, "", "event", "")

	if parked {
		t.Error("processCompletion(clean finish) = true; want parked=false (a finish is terminal; the snapshot is the resume path, not a warm tree)")
	}
	if conv := loadConversation(t, s, conversationID); conv.Status != "completed" || conv.Outcome != "finish" {
		t.Fatalf("conv = {status:%q outcome:%q}, want {completed finish}", conv.Status, conv.Outcome)
	}
	assertSnapshotPresent(t, s, taskID, true)
}

// TestProcessCompletion_FailedWritesNoSnapshot is the contrast that keeps the
// widening honest: `completed` is the whole snapshot set, not "any terminal". A
// failed run's infrastructure died, so there is nothing coherent to rehydrate.
func TestProcessCompletion_FailedWritesNoSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, taskID := setupAdvanceFixture(t, "fail-nosnap")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	task := loadTask(t, s, taskID)
	errored := res(`the agent runtime blew up`)
	errored.IsError = true
	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, "", task,
		errored, t.TempDir(), nil, "", "event", "")

	if conv := loadConversation(t, s, conversationID); conv.Status != "failed" {
		t.Fatalf("conv status = %q, want failed", conv.Status)
	}
	assertSnapshotPresent(t, s, taskID, false)
}

// TestTerminateBlueprint_AbortRetainsSnapshot: an aborted blueprint keeps its
// workspace snapshot — its completed+abort step is message-resumable, and the
// TTL sweep is what collects it later.
func TestTerminateBlueprint_AbortRetainsSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, taskID := setupAdvanceFixture(t, "term-abort-keep")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	putTestSnapshot(t, s, taskID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "", time.Now(),
		runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusAborted, "needs a human", nil, true)

	assertSnapshotPresent(t, s, taskID, true)
}

// TestTerminateBlueprint_CancelRetainsSnapshot: a cancelled blueprint keeps its
// workspace snapshot too. Its final step parked `open` rather than being torn
// down, and throwing the workspace away at exactly the moment a user killed a
// wedged run is the behavior this retention exists to stop — the TTL sweep is
// what collects it instead.
func TestTerminateBlueprint_CancelRetainsSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, taskID := setupAdvanceFixture(t, "term-cancel-keep")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	putTestSnapshot(t, s, taskID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "", time.Now(),
		runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusCancelled, "cancelled", nil, true)

	assertSnapshotPresent(t, s, taskID, true)
}

// TestTerminateBlueprint_CompletedRetainsSnapshot: a cleanly completed blueprint
// keeps its workspace snapshot. terminateBlueprint tears the worktree down, so
// discarding here would delete the blob the step just wrote seconds earlier and
// leave a successful blueprint as the one outcome with no workspace at all.
func TestTerminateBlueprint_CompletedRetainsSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, taskID := setupAdvanceFixture(t, "term-finish-keep")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	putTestSnapshot(t, s, taskID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "", time.Now(),
		runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusCompleted, "", nil, true)

	assertSnapshotPresent(t, s, taskID, true)
}

// TestTerminateBlueprint_FailedRetainsSnapshotAndLifecycleRow: a failed
// blueprint keeps its workspace too, blob and lifecycle row alike.
//
// The blob is keyed by the TASK, and a blueprint failing is not the task
// failing: the infrastructure under one run died, which says nothing about the
// tree it died in. Discarding here was what made an infra fault cost the task
// its work — the next delegation on it would find nothing to rehydrate and
// clone the base branch fresh, throwing away every step that had already run.
//
// The lifecycle row travels with the blob because the two are one answer: a
// `written` row pointing at a blob that is gone is what a resume cannot tell
// from a persist still in flight.
func TestTerminateBlueprint_FailedRetainsSnapshotAndLifecycleRow(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, taskID := setupAdvanceFixture(t, "term-failed-keep")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	putTestSnapshot(t, s, taskID)
	if err := s.workspaceSnapshots.BeginSnapshotSystem(context.Background(), runmode.LocalDefaultOrgID, taskID, "claim-term-failed"); err != nil {
		t.Fatalf("record the snapshot lifecycle: %v", err)
	}
	if _, err := s.workspaceSnapshots.FinishSnapshotSystem(context.Background(), runmode.LocalDefaultOrgID, taskID, "claim-term-failed", true); err != nil {
		t.Fatalf("settle the snapshot lifecycle: %v", err)
	}

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "", time.Now(),
		runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusFailed, "crashed", nil, true)

	assertSnapshotPresent(t, s, taskID, true)
	state, err := s.snapshotStateFor(context.Background(), runmode.LocalDefaultOrgID, taskID)
	if err != nil {
		t.Fatalf("read the snapshot lifecycle after the terminal: %v", err)
	}
	if state == nil || state.State != domain.WorkspaceSnapshotWritten {
		t.Errorf("snapshot lifecycle after a failed terminal = %+v, want a written row — a resume reads this to tell a kept blob from a missing one", state)
	}
}

// TestReapExpiredSnapshots_DropsExpiredKeepsFresh is the end-to-end retention
// sweep: a completed+abort run parked past the TTL has its snapshot reaped, while
// a freshly-parked one within the TTL survives.
func TestReapExpiredSnapshots_DropsExpiredKeepsFresh(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, oldConversationID, _ := setupAdvanceFixture(t, "reap-old")
	wireBlobStore(t, s)

	oldKey := taskIDForConversation(t, database, oldConversationID)
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort', completed_at=datetime('now','-20 days') WHERE id=?`, oldConversationID); err != nil {
		t.Fatalf("age old run: %v", err)
	}
	putTestSnapshot(t, s, oldKey)

	seedConversation(t, database, "r-fresh", "sess-fresh", "/tmp/wt-fresh")
	freshKey := taskIDForConversation(t, database, "r-fresh")
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort', completed_at=datetime('now') WHERE id='r-fresh'`); err != nil {
		t.Fatalf("complete fresh run: %v", err)
	}
	putTestSnapshot(t, s, freshKey)

	s.ReapExpiredSnapshots(context.Background())

	assertSnapshotPresent(t, s, oldKey, false)  // parked past the TTL → reaped
	assertSnapshotPresent(t, s, freshKey, true) // within the TTL → kept
}

// TestListReapableSnapshotKeys_CoversEveryCompletedExcludesFailedAndInTTL pins
// the query rules against the widened write policy: a past-TTL completed run is
// eligible whatever its outcome (abort AND finish — the finish case is the one
// the old outcome='abort' filter silently omitted, leaving its blob forever), a
// past-TTL failed run is excluded (it never had a blob to age out), and a
// within-TTL open run is not yet eligible.
func TestListReapableSnapshotKeys_CoversEveryCompletedExcludesFailedAndInTTL(t *testing.T) {
	s, database, abortConversationID, _ := setupAdvanceFixture(t, "reap-rules")
	abortKey := taskIDForConversation(t, database, abortConversationID)
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort', completed_at=datetime('now','-30 days') WHERE id=?`, abortConversationID); err != nil {
		t.Fatalf("age abort run: %v", err)
	}

	seedConversation(t, database, "r-fin2", "s", "/tmp/wt")
	finishKey := taskIDForConversation(t, database, "r-fin2")
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='finish', completed_at=datetime('now','-30 days') WHERE id='r-fin2'`); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	seedConversation(t, database, "r-failed2", "s", "/tmp/wt")
	failedKey := taskIDForConversation(t, database, "r-failed2")
	if _, err := database.Exec(`UPDATE conversations SET status='failed', completed_at=datetime('now','-30 days') WHERE id='r-failed2'`); err != nil {
		t.Fatalf("failed run: %v", err)
	}
	seedConversation(t, database, "r-open2", "s", "/tmp/wt")
	openKey := taskIDForConversation(t, database, "r-open2")
	if _, err := database.Exec(`UPDATE conversations SET status='open', completed_at=NULL, started_at=datetime('now') WHERE id='r-open2'`); err != nil {
		t.Fatalf("open run: %v", err)
	}

	cutoff := time.Now().Add(-14 * 24 * time.Hour)
	keys := reapKeys(t, s, cutoff)
	if !keysContain(keys, abortKey) {
		t.Errorf("past-TTL completed+abort key %s not reapable", abortKey)
	}
	if !keysContain(keys, finishKey) {
		t.Errorf("past-TTL completed+finish key %s not reapable; its snapshot would leak forever", finishKey)
	}
	if keysContain(keys, failedKey) {
		t.Errorf("failed key %s is reapable; failed runs carry no snapshot to age out", failedKey)
	}
	if keysContain(keys, openKey) {
		t.Errorf("within-TTL open key %s is reapable; the TTL has not elapsed", openKey)
	}
	for _, k := range keys {
		if k.OrgID != runmode.LocalDefaultOrgID {
			t.Errorf("key org = %q, want %q", k.OrgID, runmode.LocalDefaultOrgID)
		}
	}
}

// TestListReapableSnapshotKeys_SharedTaskNeedsAllPastTTL pins the shared-key
// rule: a task's conversations share one snapshot blob, so a task whose
// conversations span the TTL boundary (one old, one fresh) is NOT reaped until
// every resumable one is past the TTL.
func TestListReapableSnapshotKeys_SharedTaskNeedsAllPastTTL(t *testing.T) {
	s, database, conversationID1, taskID := setupAdvanceFixture(t, "reap-shared")
	bpr := blueprintRunIDForConversation(t, database, conversationID1)
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort', completed_at=datetime('now','-30 days') WHERE id=?`, conversationID1); err != nil {
		t.Fatalf("age step 1: %v", err)
	}
	// A second step on the SAME blueprint_run, also completed+abort but fresh.
	addStepConversation(t, database, bpr, taskID, "run2-shared", 1, "running")
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort', completed_at=datetime('now') WHERE id='run2-shared'`); err != nil {
		t.Fatalf("complete step 2: %v", err)
	}

	cutoff := time.Now().Add(-14 * 24 * time.Hour)
	keys, err := s.conversations.ListReapableSnapshotKeysSystem(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("ListReapableSnapshotKeysSystem: %v", err)
	}
	for _, k := range keys {
		if k.TaskID == taskID {
			t.Errorf("shared task %s reaped while one of its conversations is still within the TTL", taskID)
		}
	}
}

// TestParkOpenResuming_StampsAndClearsParkedAt pins the park-timestamp
// lifecycle: ParkOpen stamps parked_at (so retention keys off the last park),
// the resume-by-enqueue flip clears it (the run is no longer parked).
func TestParkOpenResuming_StampsAndClearsParkedAt(t *testing.T) {
	s, database, conversationID, _ := setupAdvanceFixture(t, "parked-stamp")
	ctx := context.Background()

	if _, err := s.conversations.ParkOpen(ctx, runmode.LocalDefaultOrgID, conversationID, db.ParkIdle()); err != nil {
		t.Fatalf("ParkOpen: %v", err)
	}
	if !parkedAtSet(t, database, conversationID) {
		t.Error("ParkOpen did not stamp parked_at")
	}

	if _, err := s.conversations.MarkQueuedForResume(ctx, runmode.LocalDefaultOrgID, conversationID); err != nil {
		t.Fatalf("MarkQueuedForResume: %v", err)
	}
	if parkedAtSet(t, database, conversationID) {
		t.Error("MarkQueuedForResume did not clear parked_at")
	}
}

// TestListReapableSnapshotKeys_OpenRunKeysOffParkedAt is the fix for the
// retention-timestamp concern: a long-lived open run started weeks ago but
// re-parked recently must NOT be reaped (retention keys off the last park via
// parked_at, not the never-resetting started_at); once the park itself ages past
// the TTL it becomes reapable.
func TestListReapableSnapshotKeys_OpenRunKeysOffParkedAt(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "reap-parked")
	cutoff := time.Now().Add(-14 * 24 * time.Hour)

	// Started 30 days ago, last re-parked just now → must survive.
	if _, err := database.Exec(`UPDATE conversations SET status='open', started_at=datetime('now','-30 days'), parked_at=datetime('now') WHERE id=?`, conversationID); err != nil {
		t.Fatalf("seed recently-parked open run: %v", err)
	}
	if keysContain(reapKeys(t, s, cutoff), taskID) {
		t.Errorf("recently re-parked open run %s reaped on its old started_at; retention must key off parked_at", taskID)
	}

	// Age the park past the TTL → now reapable.
	if _, err := database.Exec(`UPDATE conversations SET parked_at=datetime('now','-30 days') WHERE id=?`, conversationID); err != nil {
		t.Fatalf("age park: %v", err)
	}
	if !keysContain(reapKeys(t, s, cutoff), taskID) {
		t.Errorf("open run parked 30 days ago not reaped; want reapable past the TTL")
	}
}

// --- helpers ---------------------------------------------------------------

func parkedAtSet(t *testing.T, database *sql.DB, conversationID string) bool {
	t.Helper()
	var pa sql.NullString
	if err := database.QueryRow(`SELECT parked_at FROM conversations WHERE id=?`, conversationID).Scan(&pa); err != nil {
		t.Fatalf("read parked_at: %v", err)
	}
	return pa.Valid
}

func reapKeys(t *testing.T, s *Spawner, cutoff time.Time) []domain.SnapshotReapKey {
	t.Helper()
	keys, err := s.conversations.ListReapableSnapshotKeysSystem(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("ListReapableSnapshotKeysSystem: %v", err)
	}
	return keys
}

func keysContain(keys []domain.SnapshotReapKey, taskID string) bool {
	for _, k := range keys {
		if k.TaskID == taskID {
			return true
		}
	}
	return false
}

func wireBlobStore(t *testing.T, s *Spawner) {
	t.Helper()
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s.SetStorage(blobs)
}

func putTestSnapshot(t *testing.T, s *Spawner, keyID string) {
	t.Helper()
	if err := s.Storage().Put(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, keyID), strings.NewReader("snapshot")); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}

func assertSnapshotPresent(t *testing.T, s *Spawner, keyID string, want bool) {
	t.Helper()
	ok, err := s.Storage().Exists(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, keyID))
	if err != nil {
		t.Fatalf("Exists(%s): %v", keyID, err)
	}
	if ok != want {
		t.Errorf("snapshot for %s present = %v, want %v", keyID, ok, want)
	}
}
